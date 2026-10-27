package cli

// import — bulk migration of an existing git repository into a banyan
// server, segment by segment (docs/03 §5): each push carries one
// topologically contiguous slice under a byte budget, so server memory
// stays bounded no matter the history size (streamed ingest), and the root
// timeline fast-forwards once per segment — the multi-pack model keeps the
// fragments first-class. Resume is inherent: the next run starts at
// whatever tip the remote already holds. The command drives local git
// plumbing plus ordinary git push; the source repository is never modified.

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

func runImport(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var src, dst string
	branch := "main"
	segmentBytes := int64(3 << 30)
	dryRun := false
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--branch" && i+1 < len(args):
			branch = args[i+1]
			i += 2
		case a == "--segment-bytes" && i+1 < len(args):
			n, err := parseBytes(args[i+1])
			if err != nil {
				return usageErr(stderr, "import: --segment-bytes %q: %v", args[i+1], err)
			}
			segmentBytes = n
			i += 2
		case a == "--dry-run":
			dryRun = true
			i++
		case strings.HasPrefix(a, "--"):
			return usageErr(stderr, "import: unknown flag %q", a)
		case src == "":
			src = a
			i++
		case dst == "":
			dst = a
			i++
		default:
			return usageErr(stderr, "import: unexpected argument %q", a)
		}
	}
	if src == "" || dst == "" {
		return usageErr(stderr, "import: <local-repo> <server-url> required (credentials ride the URL, e.g. http://admin:pass@host/r/repo.git)")
	}
	if segmentBytes < 16<<10 {
		return usageErr(stderr, "import: --segment-bytes must be ≥ 16KiB")
	}

	tip, err := gitOut(src, "rev-parse", "--verify", branch+"^{commit}")
	if err != nil {
		return usageErr(stderr, "import: %s: %v", src, err)
	}
	tip = strings.TrimSpace(tip)
	remoteTip, err := lsRemoteTip(dst, branch)
	if err != nil {
		fmt.Fprintf(stderr, "error: import: remote unreachable: %v\n", err)
		return 3
	}
	if remoteTip == tip {
		fmt.Fprintf(stdout, "up to date: %s/%s already at %s\n", dst, branch, short(tip))
		return 0
	}
	if remoteTip != "" {
		if err := gitRun(src, "merge-base", "--is-ancestor", remoteTip, tip); err != nil {
			return usageErr(stderr, "import: remote %s/%s (%s) is not an ancestor of local %s — histories diverged; resolve manually", dst, branch, short(remoteTip), short(tip))
		}
	}

	rangeArg := tip
	if remoteTip != "" {
		rangeArg = remoteTip + ".." + tip
	}
	out, err := gitOut(src, "rev-list", "--reverse", "--topo-order", rangeArg)
	if err != nil {
		return usageErr(stderr, "import: rev-list: %v", err)
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			commits = append(commits, line)
		}
	}
	if len(commits) == 0 {
		fmt.Fprintln(stdout, "up to date: nothing to import")
		return 0
	}

	segs, err := planSegments(src, remoteTip, commits, segmentBytes)
	if err != nil {
		return usageErr(stderr, "import: planning: %v", err)
	}
	total, _ := rangeObjectsBytes(src, remoteTip, tip)
	fmt.Fprintf(stdout, "import %s → %s/%s: %d commits, %d segments, ~%.1f MiB\n",
		short(tip), dst, branch, len(commits), len(segs), float64(total)/(1<<20))
	if dryRun {
		for i, s := range segs {
			fmt.Fprintf(stdout, "  segment %d/%d → %s\n", i+1, len(segs), short(s))
		}
		return 0
	}

	for i, segTip := range segs {
		fmt.Fprintf(stdout, "  segment %d/%d → %s: pushing…\n", i+1, len(segs), short(segTip))
		if out, err := gitOut(src, "-c", "core.autocrlf=false", "push", dst, segTip+":refs/heads/"+branch); err != nil {
			fmt.Fprintf(stderr, "error: import: segment %d push failed:\n%s", i+1, out)
			return 3
		}
	}

	finalTip, err := lsRemoteTip(dst, branch)
	if err != nil || finalTip != tip {
		if err == nil {
			err = fmt.Errorf("remote tip %s ≠ local tip %s", short(finalTip), short(tip))
		}
		fmt.Fprintf(stderr, "error: import: final verification failed: %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "imported: %s/%s → %s (%d segments)\n", dst, branch, short(tip), len(segs))
	return 0
}

// planSegments cuts the oldest→newest commit list into segments whose
// exclusive object bytes stay under the budget. Growth is exponential with
// re-measurement at each probe, so the planning cost per segment is a
// bounded multiple of its size; an overflowing probe ends the segment at
// the last good candidate (segments may land well under budget — they are
// upper bounds, never overruns). A single commit larger than the budget
// becomes its own segment rather than an error.
func planSegments(src, base string, commits []string, budget int64) ([]string, error) {
	var segs []string
	prev := base
	start := 0
	for start < len(commits) {
		end := start + 1
		step := 1
		for {
			cand := end + step
			if cand > len(commits) {
				cand = len(commits)
			}
			b, err := rangeObjectsBytes(src, prev, commits[cand-1])
			if err != nil {
				return nil, err
			}
			if b > budget {
				break
			}
			end = cand
			if end >= len(commits) {
				break
			}
			if b > budget/2 {
				break // close enough: don't re-measure the tail for crumbs
			}
			step *= 2
		}
		segs = append(segs, commits[end-1])
		prev = commits[end-1]
		start = end
	}
	return segs, nil
}

// rangeObjectsBytes sums the raw object sizes exclusively reachable between
// two commits (rev-list --objects piped through cat-file --batch-check) —
// the byte volume a push of that range would carry, modulo delta savings.
// Input lines are cut to the first field: without %(rest) in the format,
// cat-file takes the WHOLE line as the object name, and rev-list decorates
// tree/blob lines with a trailing space or path.
func rangeObjectsBytes(dir, from, to string) (int64, error) {
	rangeArg := to
	if from != "" {
		rangeArg = from + ".." + to
	}
	list, err := exec.Command("git", "-C", dir, "rev-list", "--objects", rangeArg).Output()
	if err != nil {
		return 0, fmt.Errorf("rev-list --objects %s: %w", rangeArg, err)
	}
	var names strings.Builder
	for _, line := range strings.Split(string(list), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names.WriteString(f[0])
			names.WriteByte('\n')
		}
	}
	cmd := exec.Command("git", "-C", dir, "cat-file", "--batch-check=%(objectsize)")
	cmd.Stdin = strings.NewReader(names.String())
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("cat-file --batch-check: %w", err)
	}
	var total int64
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("batch-check line %q: %w", line, err)
		}
		total += n
	}
	return total, nil
}

// lsRemoteTip resolves the remote branch tip. A repository that does not
// exist yet is not an error — it is a fresh import (banyan auto-creates on
// the first push); anything else (network, auth) is.
func lsRemoteTip(remote, branch string) (string, error) {
	cmd := exec.Command("git", "ls-remote", remote, "refs/heads/"+branch)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		if strings.Contains(strings.ToLower(buf.String()), "not found") ||
			strings.Contains(buf.String(), "404") {
			return "", nil
		}
		return "", fmt.Errorf("git ls-remote %s: %w\n%s", remote, err, buf.String())
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
			return fields[0], nil
		}
	}
	return "", nil
}

func gitOut(dir string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func gitRun(dir string, args ...string) error {
	_, err := gitOut(dir, args...)
	return err
}

// parseBytes accepts plain byte counts or K/M/G suffixes (KiB/KB/K all
// binary — the budget is an upper bound, not a billing line).
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	num, suffix := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("want a byte count like 3G, 512MiB or 1048576")
	}
	var mult int64 = 1
	switch suffix {
	case "", "b":
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("unknown suffix %q (want K/M/G)", suffix)
	}
	return n * mult, nil
}
