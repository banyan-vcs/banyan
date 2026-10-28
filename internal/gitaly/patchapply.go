package gitaly

// Mailbox splitting and unified-diff application (UserApplyPatch, docs/19):
// the web-editor patch flow sends an mbox stream whose mails carry one
// unified diff each. Everything here is pure Go — parsing mirrors
// git mailsplit/mailinfo's essential shape (From-lines, RFC-822 headers,
// "[PATCH n/m]" subject stripping), the applier mirrors git apply's
// context matching (fuzzy offsets only; binary and rename/copy patches
// are rejected explicitly rather than misapplied).

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parsedPatch is one mail of the mbox: the author identity, the message
// and the unified diff text.
type parsedPatch struct {
	authorName, authorEmail string
	authorDate              time.Time
	subject, body           string
	diff                    string
}

// errPatchReject marks a patch the applier refuses on its face (binary,
// rename/copy, malformed hunks) as opposed to a context conflict.
var errPatchReject = errors.New("patch rejected")

// splitMailbox parses an mbox blob into its mails. Splitting follows
// mailsplit: a line starting "From " begins a mail (the ">From " escaped
// form stays body text); headers end at the first empty line.
func splitMailbox(data []byte) ([]parsedPatch, error) {
	var out []parsedPatch
	lines := strings.Split(string(data), "\n")
	var cur *parsedPatch
	var header bool
	var bodyLines, diffLines []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.body = strings.Join(bodyLines, "\n")
		cur.diff = strings.Join(diffLines, "\n")
		out = append(out, *cur)
		cur = nil
	}
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "From ") && !strings.HasPrefix(line, "From :") {
			flush()
			cur = &parsedPatch{}
			header = true
			bodyLines, diffLines = nil, nil
			continue
		}
		if cur == nil {
			continue // preamble before the first mail
		}
		if header {
			if line == "" {
				header = false
				continue
			}
			parseMailHeader(cur, line)
			continue
		}
		// Body: the diff starts at the first "diff --git" line and runs to
		// the end of the mail.
		if len(diffLines) == 0 && strings.HasPrefix(line, "diff --git ") {
			diffLines = append(diffLines, line)
			continue
		}
		if len(diffLines) > 0 {
			diffLines = append(diffLines, line)
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	flush()
	if len(out) == 0 {
		return nil, fmt.Errorf("no mails in mailbox: %w", errPatchReject)
	}
	for i := range out {
		out[i].subject = stripPatchPrefix(out[i].subject)
	}
	return out, nil
}

// parseMailHeader consumes one RFC-822 header line (continuation lines
// appended with a space, mailinfo-style).
func parseMailHeader(p *parsedPatch, line string) {
	key, val, ok := strings.Cut(line, ":")
	if !ok {
		return
	}
	val = strings.TrimSpace(val)
	switch strings.ToLower(key) {
	case "from":
		name, email := parseMailFrom(val)
		p.authorName, p.authorEmail = name, email
	case "date":
		if t, err := parseMailDate(val); err == nil {
			p.authorDate = t
		}
	case "subject":
		p.subject = val
	}
}

// parseMailFrom splits "Name <email>" (either side optional).
func parseMailFrom(v string) (string, string) {
	if i := strings.IndexByte(v, '<'); i >= 0 {
		name := strings.TrimSpace(v[:i])
		email := strings.TrimSuffix(strings.TrimSpace(v[i:]), ">")
		email = strings.TrimPrefix(email, "<")
		if name == "" {
			name = email
		}
		return name, email
	}
	return strings.TrimSpace(v), strings.TrimSpace(v)
}

// parseMailDate accepts RFC-2822 dates and the "unix timestamp +zone"
// git format ("1690000000 +0200").
func parseMailDate(v string) (time.Time, error) {
	if segs := strings.Fields(v); len(segs) == 2 {
		if sec, err := strconv.ParseInt(segs[0], 10, 64); err == nil {
			return time.Unix(sec, 0), nil
		}
	}
	return time.Parse(time.RFC1123Z, v)
}

// stripPatchPrefix removes leading "[PATCH]", "[PATCH 2/5]"-style tags and
// surrounding whitespace from a mail subject.
func stripPatchPrefix(s string) string {
	for {
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "[") {
			return s
		}
		end := strings.Index(s, "]")
		if end < 0 || !strings.HasPrefix(s, "[PATCH") {
			return s
		}
		s = s[end+1:]
	}
}

// filePatch is one file's change inside a unified diff.
type filePatch struct {
	oldPath, newPath string // clean repo-relative paths; "" = /dev/null side
	oldMode, newMode string // six-digit octal ("100644"); "" = unset
	deleteFile       bool
	newFile          bool
	hunks            []hunk
}

// hunk is one @@ section: the old/new side starts and their ORIGINAL line
// counts (what matching uses), plus parse-time remaining counters.
type hunk struct {
	oldStart, newStart int
	oldLines, newLines int // original totals from the header
	remOld, remNew     int // parse-time remaining budget
	lines              []string
}

// parseUnifiedDiff splits a whole-diff text into per-file patches.
func parseUnifiedDiff(diff string) ([]filePatch, error) {
	var out []filePatch
	var cur *filePatch
	lines := strings.Split(diff, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if cur != nil {
				out = append(out, *cur)
			}
			a, b := parseDiffGitPaths(line)
			cur = &filePatch{oldPath: a, newPath: b}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "index "):
			// "index <old>..<new> [mode]" — modes ride the last field.
		case strings.HasPrefix(line, "old mode "):
			cur.oldMode = strings.TrimPrefix(line, "old mode ")
		case strings.HasPrefix(line, "new mode "):
			cur.newMode = strings.TrimPrefix(line, "new mode ")
		case strings.HasPrefix(line, "new file mode "):
			cur.newFile = true
			cur.newMode = strings.TrimPrefix(line, "new file mode ")
		case strings.HasPrefix(line, "deleted file mode "):
			cur.deleteFile = true
			cur.oldMode = strings.TrimPrefix(line, "deleted file mode ")
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "),
			strings.HasPrefix(line, "copy from "), strings.HasPrefix(line, "copy to "):
			return nil, fmt.Errorf("rename/copy patches unsupported: %w", errPatchReject)
		case strings.HasPrefix(line, "GIT binary patch"), strings.HasPrefix(line, "Binary files "):
			return nil, fmt.Errorf("binary patches unsupported: %w", errPatchReject)
		case strings.HasPrefix(line, "@@ "):
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			h.remOld, h.remNew = h.oldLines, h.newLines
			// Hunk body runs until the next non-body line.
			i++
			for ; i < len(lines); i++ {
				l := strings.TrimSuffix(lines[i], "\r")
				if l == "" {
					// An empty line can be a context line whose trailing
					// space was stripped by a mailer: treat as " ".
					if h.remOld > 0 || h.remNew > 0 {
						h.lines = append(h.lines, " ")
						countHunkLine(&h, ' ')
						continue
					}
					break
				}
				switch l[0] {
				case ' ', '-', '+':
					h.lines = append(h.lines, l)
					countHunkLine(&h, l[0])
				case '\\':
					h.lines = append(h.lines, l)
				default:
					goto hunkDone
				}
				if h.remOld <= 0 && h.remNew <= 0 {
					break
				}
			}
		hunkDone:
			i-- // compensate the outer loop increment
			cur.hunks = append(cur.hunks, h)
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no diff in patch: %w", errPatchReject)
	}
	return out, nil
}

// countHunkLine tracks the remaining old/new line budget.
func countHunkLine(h *hunk, prefix byte) {
	switch prefix {
	case ' ':
		h.remOld--
		h.remNew--
	case '-':
		h.remOld--
	case '+':
		h.remNew--
	}
}

// parseDiffGitPaths extracts the a//b paths from a "diff --git a/x b/x"
// line (quoted paths not supported — patches from the web flow are plain).
func parseDiffGitPaths(line string) (string, string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	// The split is ambiguous with spaces in paths; the web flow's plain
	// paths split at the " b/" marker of the second path.
	if i := strings.Index(rest, " b/"); i >= 0 {
		a := strings.TrimPrefix(rest[:i], "a/")
		b := strings.TrimPrefix(rest[i+1:], "b/")
		return a, b
	}
	return rest, rest
}

// parseHunkHeader parses "@@ -l,s +l,s @@ [section heading]".
func parseHunkHeader(line string) (hunk, error) {
	var h hunk
	rest := strings.TrimPrefix(line, "@@ ")
	section := strings.Index(rest, "@@")
	if section < 0 {
		return h, fmt.Errorf("malformed hunk header %q: %w", line, errPatchReject)
	}
	fields := strings.Fields(rest[:section])
	if len(fields) < 2 {
		return h, fmt.Errorf("malformed hunk header %q: %w", line, errPatchReject)
	}
	parseSide := func(s string, def int) (int, int, error) {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
		if c := strings.IndexByte(s, ','); c >= 0 {
			n, err := strconv.Atoi(s[c+1:])
			return atoiDefault(s[:c]), n, err
		}
		start, err := strconv.Atoi(s)
		return start, def, err
	}
	var err error
	if h.oldStart, h.oldLines, err = parseSide(fields[0], 1); err != nil {
		return h, fmt.Errorf("malformed old side %q: %w", fields[0], errPatchReject)
	}
	if h.newStart, h.newLines, err = parseSide(fields[1], 1); err != nil {
		return h, fmt.Errorf("malformed new side %q: %w", fields[1], errPatchReject)
	}
	return h, nil
}

func atoiDefault(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// applyToLines applies one hunk's edit at the recorded position, falling
// back to a forward scan for matching context (git apply's offset search).
func applyToLines(dst []string, h hunk) ([]string, bool) {
	matchAt := func(at int) bool {
		if at < 0 || at+h.oldLines > len(dst) {
			return false
		}
		k := at
		for _, l := range h.lines {
			switch l[0] {
			case ' ', '\\':
				if l[0] == ' ' && dst[k] != strings.TrimPrefix(l, " ") {
					return false
				}
				if l[0] == ' ' {
					k++
				}
			case '-':
				if dst[k] != strings.TrimPrefix(l, "-") {
					return false
				}
				k++
			}
		}
		return k == at+h.oldLines
	}
	start := h.oldStart - 1
	if !matchAt(start) {
		for off := 1; start-off >= 0 || start+off+h.oldLines <= len(dst); off++ {
			if matchAt(start - off) {
				start -= off
				break
			}
			if matchAt(start + off) {
				start += off
				break
			}
		}
	}
	if !matchAt(start) {
		return dst, false
	}
	out := make([]string, 0, len(dst))
	out = append(out, dst[:start]...)
	for _, l := range h.lines {
		switch l[0] {
		case ' ':
			out = append(out, strings.TrimPrefix(l, " "))
		case '-':
			// dropped
		case '+':
			out = append(out, strings.TrimPrefix(l, "+"))
		}
	}
	out = append(out, dst[start+h.oldLines:]...)
	return out, true
}

// joinPatchLines renders content lines preserving a missing trailing
// newline (the "\ No newline at end of file" marker).
func joinPatchLines(lines []string) []byte {
	noNewline := false
	for _, l := range lines {
		if strings.HasPrefix(l, "\\") {
			noNewline = true
		}
	}
	body := lines
	if noNewline && len(body) > 0 {
		body = body[:len(body)-1]
	}
	var b bytes.Buffer
	for _, l := range body {
		b.WriteString(strings.TrimPrefix(strings.TrimPrefix(l, "+"), " "))
		b.WriteString("\n")
	}
	out := b.Bytes()
	if noNewline && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out
}
