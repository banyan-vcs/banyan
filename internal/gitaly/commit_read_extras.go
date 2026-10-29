package gitaly

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// Commit read extras: diverging counts, language shares, line blame,
// per-ref tips and signature probes. All pure Go over the object plane.

// CountDivergingCommits answers how many commits each side contributes
// between from and to (`rev-list --left-right --count` over the symmetric
// difference; the merge base splits the ranges).
func (cs *commitService) CountDivergingCommits(ctx context.Context, req *gitalypb.CountDivergingCommitsRequest) (*gitalypb.CountDivergingCommitsResponse, error) {
	name, err := cs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	from, err := cs.s.resolveRevision(ctx, name, string(req.GetFrom()))
	if err != nil {
		return nil, err
	}
	to, err := cs.s.resolveRevision(ctx, name, string(req.GetTo()))
	if err != nil {
		return nil, err
	}
	base, err := cs.s.mergeBaseOf(ctx, name, from, to)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	countNotIn := func(tip, stopAt model.OID) int32 {
		n := int32(0)
		_ = cs.s.walkParents(ctx, name, []model.OID{tip}, &walkFilter{}, func(oid model.OID, rec *commitRecord) bool {
			if !stopAt.IsZero() && oid == stopAt {
				return false
			}
			n++
			return req.GetMaxCount() == 0 || n < req.GetMaxCount()
		})
		return n
	}
	return &gitalypb.CountDivergingCommitsResponse{
		LeftCount:  countNotIn(from, base),
		RightCount: countNotIn(to, base),
	}, nil
}

// mergeBaseOf is the two-revision single-base walk behind FindMergeBase and
// the diverging count: A's ancestor set, then B's newest walk hit.
func (s *Server) mergeBaseOf(ctx context.Context, repo string, a, b model.OID) (model.OID, error) {
	ancestors := map[model.OID]bool{}
	if err := s.walkParents(ctx, repo, []model.OID{a}, &walkFilter{}, func(oid model.OID, _ *commitRecord) bool {
		ancestors[oid] = true
		return true
	}); err != nil {
		return "", err
	}
	var found model.OID
	if err := s.walkParents(ctx, repo, []model.OID{b}, &walkFilter{}, func(oid model.OID, _ *commitRecord) bool {
		if !found.IsZero() {
			return false
		}
		if ancestors[oid] {
			found = oid
			return false
		}
		return true
	}); err != nil {
		return "", err
	}
	return found, nil
}

// ---- CommitLanguages ----

// languageByExtension maps common extensions to linguist names (a curated
// subset; docs/19 §6 registers the approximation against the full library).
var languageByExtension = map[string]string{
	".go": "Go", ".rs": "Rust", ".py": "Python", ".rb": "Ruby", ".js": "JavaScript",
	".ts": "TypeScript", ".jsx": "JavaScript", ".tsx": "TypeScript", ".java": "Java",
	".kt": "Kotlin", ".c": "C", ".h": "C", ".cc": "C++", ".cpp": "C++", ".hpp": "C++",
	".cs": "C#", ".swift": "Swift", ".m": "Objective-C", ".sh": "Shell", ".bash": "Shell",
	".css": "CSS", ".scss": "SCSS", ".html": "HTML", ".xml": "XML", ".json": "JSON",
	".yaml": "YAML", ".yml": "YAML", ".toml": "TOML", ".md": "Markdown", ".sql": "SQL",
	".php": "PHP", ".scala": "Scala", ".ex": "Elixir", ".exs": "Elixir", ".erl": "Erlang",
	".lua": "Lua", ".vim": "Vim script", ".proto": "Protocol Buffers",
	".tf": "HCL", ".gradle": "Gradle", ".ipynb": "Jupyter Notebook",
}

// languageColors are GitLab's language badge colors for the mapped set.
var languageColors = map[string]string{
	"Go": "#00ADD8", "Rust": "#dea584", "Python": "#3572A5", "Ruby": "#701516",
	"JavaScript": "#f1e05a", "TypeScript": "#2b7489", "Java": "#b07219", "Kotlin": "#F18E33",
	"C": "#555555", "C++": "#f34b7d", "C#": "#178600", "Swift": "#F05138", "Objective-C": "#438eff",
	"Shell": "#89e051", "CSS": "#563d7c", "SCSS": "#c6538c", "HTML": "#e34c26", "XML": "#0060ac",
	"JSON": "#292929", "YAML": "#cb171e", "TOML": "#9c4221", "Markdown": "#083fa1", "SQL": "#e38c00",
	"PHP": "#4F5D95", "Scala": "#c22d40", "Elixir": "#6e4a7e", "Erlang": "#B83998", "Lua": "#000080",
	"Vim script": "#199f4b", "Dockerfile": "#384d54", "Protocol Buffers": "#e09f6c", "HCL": "#844FBA",
	"Gradle": "#02303a", "Jupyter Notebook": "#DA5B0B",
}

// CommitLanguages answers byte-share statistics by language — extension
// and shebang driven, the linguist classifier subset.
func (cs *commitService) CommitLanguages(ctx context.Context, req *gitalypb.CommitLanguagesRequest) (*gitalypb.CommitLanguagesResponse, error) {
	name, err := cs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	oid, err := cs.s.resolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return nil, err
	}
	rec, err := cs.s.commitRecord(ctx, name, oid)
	if err != nil {
		return nil, err
	}
	blobs, err := cs.s.listTreeBlobPaths(ctx, name, rec.Tree)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	bytesByLang := map[string]uint64{}
	var total uint64
	for _, bp := range blobs {
		_, raw, rerr := cs.s.Objects.Raw(ctx, name, bp.oid)
		if rerr != nil {
			continue
		}
		lang := languageOf(bp.path, raw)
		if lang == "" {
			continue
		}
		bytesByLang[lang] += uint64(len(raw))
		total += uint64(len(raw))
	}
	out := &gitalypb.CommitLanguagesResponse{}
	for lang, b := range bytesByLang {
		share := float32(0)
		if total > 0 {
			share = float32(b) / float32(total)
		}
		out.Languages = append(out.Languages, &gitalypb.CommitLanguagesResponse_Language{
			Name: lang, Share: share, Color: languageColors[lang], Bytes: b,
		})
	}
	sort.Slice(out.Languages, func(i, j int) bool {
		if out.Languages[i].Share != out.Languages[j].Share {
			return out.Languages[i].Share > out.Languages[j].Share
		}
		return out.Languages[i].Name < out.Languages[j].Name
	})
	return out, nil
}

// languageOf resolves a path to a language name via extension, with
// Dockerfile/Makefile name endings and a shebang fallback.
func languageOf(path string, raw []byte) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		if lang, ok := languageByExtension[strings.ToLower(path[i:])]; ok {
			return lang
		}
	}
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, "dockerfile"):
		return "Dockerfile"
	case strings.HasSuffix(lower, "makefile"):
		return "Makefile"
	}
	first := raw
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		first = raw[:i]
	}
	shebang := string(first)
	switch {
	case strings.HasPrefix(shebang, "#!/bin/sh"), strings.HasPrefix(shebang, "#!/usr/bin/env sh"),
		strings.HasPrefix(shebang, "#!/bin/bash"), strings.HasPrefix(shebang, "#!/usr/bin/env bash"):
		return "Shell"
	case strings.HasPrefix(shebang, "#!/usr/bin/env python"):
		return "Python"
	case strings.HasPrefix(shebang, "#!/usr/bin/env node"):
		return "JavaScript"
	case strings.HasPrefix(shebang, "#!/usr/bin/env ruby"):
		return "Ruby"
	}
	return ""
}

// ---- RawBlame ----

// blameStep is one commit of the first-parent chain with its decoded body.
type blameStep struct {
	oid model.OID
	rec *model.AnchoredCommit
}

// blameFile walks first parents attributing every tip line to the commit
// that introduced it: each parent→child diff marks inserted lines as the
// child's; surviving lines keep walking. The root commit owns survivors.
func (cs *commitService) blameChain(ctx context.Context, repo string, tip model.OID) ([]blameStep, error) {
	var chain []blameStep
	seen := map[model.OID]bool{}
	var collect func(oid model.OID) error
	collect = func(oid model.OID) error {
		if seen[oid] {
			return nil
		}
		seen[oid] = true
		typ, raw, err := cs.s.Objects.Raw(ctx, repo, oid)
		if err != nil || typ != model.CommitType {
			return err
		}
		c, err := model.DecodeCommit(raw)
		if err != nil {
			return err
		}
		chain = append(chain, blameStep{oid: oid, rec: c})
		if len(c.Parents) > 0 {
			return collect(c.Parents[0])
		}
		return nil
	}
	if err := collect(tip); err != nil {
		return nil, err
	}
	return chain, nil
}

func splitKeepNewline(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// blameLines attributes every line of path at tip: the tip file's lines,
// the first-parent chain, the owning chain index per line and the line's
// number in the owning revision.
func (cs *commitService) blameLines(ctx context.Context, repo string, tip model.OID, path string) ([]string, []blameStep, []int, []int, error) {
	chain, err := cs.blameChain(ctx, repo, tip)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	entry, err := cs.s.entryAtPath(ctx, repo, chain[0].rec.TreeOID, path)
	if err != nil || entry == nil {
		return nil, nil, nil, nil, status.Errorf(codes.NotFound, "gitaly: no such path %q", path)
	}
	_, tipRaw, err := cs.s.Objects.Raw(ctx, repo, entry.Target)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tipLines := splitKeepNewline(string(tipRaw))

	// alive[i]: the line index in the current older revision carrying tip
	// line i, or -1 once attributed / gone.
	alive := make([]int, len(tipLines))
	for i := range alive {
		alive[i] = i
	}
	next := make([]int, len(tipLines))
	owner := make([]int, len(tipLines)) // index into chain; -1 unattributed
	origNo := make([]int, len(tipLines))
	for i := range owner {
		owner[i] = -1
	}

	for ci := 0; ci < len(chain); ci++ {
		var parentRaw []byte
		if ci+1 < len(chain) {
			if e, err := cs.s.entryAtPath(ctx, repo, chain[ci+1].rec.TreeOID, path); err == nil && e != nil {
				if _, raw, err := cs.s.Objects.Raw(ctx, repo, e.Target); err == nil {
					parentRaw = raw
				}
			}
		}
		childToParent := map[int]int{}
		pi, chi := 0, 0
		for _, op := range lineDiffOps(string(parentRaw), string(tipRaw)) {
			for range splitLines(op.Text) {
				switch op.Type {
				case diffmatchpatch.DiffEqual:
					childToParent[chi] = pi
					pi++
					chi++
				case diffmatchpatch.DiffDelete:
					pi++
				case diffmatchpatch.DiffInsert:
					chi++
				}
			}
		}
		for i := range alive {
			if alive[i] < 0 || owner[i] >= 0 {
				continue
			}
			if p, ok := childToParent[alive[i]]; ok {
				next[i] = p
			} else {
				owner[i] = ci
				origNo[i] = alive[i] + 1
			}
		}
		alive, next = next, alive
		for i := range next {
			next[i] = -1
		}
	}
	for i := range owner {
		if owner[i] < 0 {
			owner[i] = len(chain) - 1
			origNo[i] = alive[i] + 1
		}
	}
	return tipLines, chain, owner, origNo, nil
}

// blamePorcelain renders `git blame --porcelain` output: one block per
// attribution run, full headers, content lines prefixed with a TAB.
func blamePorcelain(lines []string, chain []blameStep, owner []int, origNo []int, path string) string {
	var b strings.Builder
	i := 0
	for i < len(lines) {
		j := i
		for j < len(lines) && owner[j] == owner[i] {
			j++
		}
		step := chain[owner[i]]
		c := step.rec
		fmt.Fprintf(&b, "%s %d %d %d\n", step.oid, origNo[i], i+1, j-i)
		subject := strings.SplitN(strings.TrimSuffix(c.Message, "\n"), "\n", 2)[0]
		fmt.Fprintf(&b, "author %s\n", c.Author.Name)
		fmt.Fprintf(&b, "author-mail <%s>\n", c.Author.Email)
		fmt.Fprintf(&b, "author-time %d\n", c.Author.When.Unix())
		fmt.Fprintf(&b, "author-tz %s\n", c.Author.When.Format("-0700"))
		fmt.Fprintf(&b, "committer %s\n", c.Committer.Name)
		fmt.Fprintf(&b, "committer-mail <%s>\n", c.Committer.Email)
		fmt.Fprintf(&b, "committer-time %d\n", c.Committer.When.Unix())
		fmt.Fprintf(&b, "committer-tz %s\n", c.Committer.When.Format("-0700"))
		fmt.Fprintf(&b, "summary %s\n", subject)
		fmt.Fprintf(&b, "filename %s\n", path)
		b.WriteString("\n")
		for k := i; k < j; k++ {
			b.WriteString("\t")
			b.WriteString(lines[k])
		}
		i = j
	}
	return b.String()
}

// RawBlame streams the porcelain blame of one file, honoring an optional
// "L,N" range.
func (cs *commitService) RawBlame(req *gitalypb.RawBlameRequest, stream gitalypb.CommitService_RawBlameServer) error {
	ctx := stream.Context()
	name, err := cs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	tip, err := cs.s.resolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return err
	}
	lines, chain, owner, origNo, err := cs.blameLines(ctx, name, tip, string(req.GetPath()))
	if err != nil {
		return err
	}
	if r := strings.TrimSpace(string(req.GetRange())); r != "" {
		if lo, hi, ok := parseBlameRange(r); ok {
			if lo > len(lines) {
				lo = len(lines)
			}
			if hi > len(lines) {
				hi = len(lines)
			}
			if lo <= hi {
				lines = lines[lo-1 : hi]
				owner = owner[lo-1 : hi]
				origNo = origNo[lo-1 : hi]
			} else {
				lines, owner, origNo = nil, nil, nil
			}
		}
	}
	out := blamePorcelain(lines, chain, owner, origNo, string(req.GetPath()))
	return streamBytes([]byte(out), func(p []byte) error {
		return stream.Send(&gitalypb.RawBlameResponse{Data: p})
	})
}

// parseBlameRange reads "L", "L,N" or "L,N M" subsets (git's -L syntax).
func parseBlameRange(r string) (lo, hi int, ok bool) {
	r = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r), "-L"))
	parts := strings.FieldsFunc(r, func(c rune) bool { return c == ',' })
	if len(parts) == 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[0], "%d", &lo); err != nil || lo < 1 {
		return 0, 0, false
	}
	hi = lo
	if len(parts) > 1 {
		if _, err := fmt.Sscanf(parts[1], "%d", &hi); err != nil {
			return 0, 0, false
		}
	}
	return lo, hi, hi >= lo
}

// ---- ListCommitsByRefName ----

// ListCommitsByRefName answers the tip commit per requested ref name;
// unresolvable refs answer nothing.
func (cs *commitService) ListCommitsByRefName(req *gitalypb.ListCommitsByRefNameRequest, stream gitalypb.CommitService_ListCommitsByRefNameServer) error {
	ctx := stream.Context()
	name, err := cs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	var batch []*gitalypb.ListCommitsByRefNameResponse_CommitForRef
	for _, refName := range req.GetRefNames() {
		oid, rerr := cs.s.resolveRevision(ctx, name, string(refName))
		if rerr != nil {
			continue
		}
		commit, cerr := cs.s.gitCommit(ctx, name, oid)
		if cerr != nil {
			continue
		}
		batch = append(batch, &gitalypb.ListCommitsByRefNameResponse_CommitForRef{
			Commit: commit, RefName: refName})
		if len(batch) >= 100 {
			if err := stream.Send(&gitalypb.ListCommitsByRefNameResponse{CommitRefs: batch}); err != nil {
				return err
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		return stream.Send(&gitalypb.ListCommitsByRefNameResponse{CommitRefs: batch})
	}
	return nil
}

// ---- signatures ----

// splitCommitSignature extracts the gpgsig (or gpgsig-sha256) header block
// from a raw commit object and renders the signed text — the object with
// that header removed, the exact bytes git's verify-commit signs.
func splitCommitSignature(raw []byte) (signature, signedText []byte, ok bool) {
	var sigLines []string
	var out []string
	inSig := false
	for _, line := range splitKeepNewline(string(raw)) {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case trimmed == "":
			inSig = false
			out = append(out, line)
		case inSig && strings.HasPrefix(trimmed, " "):
			sigLines = append(sigLines, strings.TrimPrefix(trimmed, " "))
		case strings.HasPrefix(trimmed, "gpgsig "), strings.HasPrefix(trimmed, "gpgsig-sha256 "):
			inSig = true
			sigLines = append(sigLines, trimmed[strings.Index(trimmed, " ")+1:])
		default:
			inSig = false
			out = append(out, line)
		}
	}
	if len(sigLines) == 0 {
		return nil, nil, false
	}
	// git strips the trailing blank lines the header folding introduced.
	for len(out) > 1 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return []byte(strings.Join(sigLines, "")), []byte(strings.Join(out, "")), true
}

// GetCommitSignatures streams signature plus signed text per commit id.
func (cs *commitService) GetCommitSignatures(req *gitalypb.GetCommitSignaturesRequest, stream gitalypb.CommitService_GetCommitSignaturesServer) error {
	ctx := stream.Context()
	name, err := cs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, cid := range req.GetCommitIds() {
		typ, raw, err := cs.s.Objects.Raw(ctx, name, model.OID(strings.ToLower(cid)))
		if err != nil || typ != model.CommitType {
			continue
		}
		sig, signedText, ok := splitCommitSignature(raw)
		if !ok {
			continue
		}
		if err := stream.Send(&gitalypb.GetCommitSignaturesResponse{
			CommitId: cid, Signature: sig, SignedText: signedText,
		}); err != nil {
			return err
		}
	}
	return nil
}

// FilterShasWithSignatures is bidirectional: shas stream in, those
// carrying signatures stream back.
func (cs *commitService) FilterShasWithSignatures(stream gitalypb.CommitService_FilterShasWithSignaturesServer) error {
	ctx := stream.Context()
	var name string
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = cs.s.existingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		for _, sha := range req.GetShas() {
			typ, raw, err := cs.s.Objects.Raw(ctx, name, model.OID(strings.ToLower(string(sha))))
			if err != nil || typ != model.CommitType {
				continue
			}
			if _, _, ok := splitCommitSignature(raw); ok {
				if err := stream.Send(&gitalypb.FilterShasWithSignaturesResponse{Shas: [][]byte{sha}}); err != nil {
					return err
				}
			}
		}
	}
}
