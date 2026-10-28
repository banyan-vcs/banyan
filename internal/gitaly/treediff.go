package gitaly

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Tree diff and patch generation — the shared primitive behind
// DiffService, CommitStats and path-filtered history walks. Pure Go over
// the object plane; the line engine is go-diff (the module's merge path
// already rides it).

// treeChange is one file-level difference between two trees.
type treeChange struct {
	path             string
	oldOID, newOID   model.OID
	oldMode, newMode string // "" = absent side
}

func (c treeChange) status() byte {
	switch {
	case c.oldMode == "":
		return 'A'
	case c.newMode == "":
		return 'D'
	default:
		return 'M'
	}
}

// treeDiff computes file-level changes from fromTree to toTree (recursive;
// type flips become delete+add pairs, git's default posture without rename
// detection; directory additions/deletions flatten to their files).
func (s *Server) treeDiff(ctx context.Context, repo string, fromTree, toTree model.OID) ([]treeChange, error) {
	lookup := s.Objects.Lookup(repo)
	var out []treeChange
	var walk func(a, b model.OID, prefix string) error
	walk = func(a, b model.OID, prefix string) error {
		at, aerr := lookup.Tree(a)
		bt, berr := lookup.Tree(b)
		if aerr != nil && berr != nil {
			return nil
		}
		if aerr != nil {
			at = &model.Tree{}
		}
		if berr != nil {
			bt = &model.Tree{}
		}
		index := map[string][2]*model.TreeEntry{}
		for i := range at.Entries {
			index[at.Entries[i].Name] = [2]*model.TreeEntry{&at.Entries[i], nil}
		}
		for i := range bt.Entries {
			e := bt.Entries[i]
			if pair, ok := index[e.Name]; ok {
				pair[1] = &e
				index[e.Name] = pair
			} else {
				index[e.Name] = [2]*model.TreeEntry{nil, &e}
			}
		}
		for _, name := range sortedNames(index) {
			pair := index[name]
			old, new := pair[0], pair[1]
			path := prefix + name
			switch {
			case old == nil:
				flattenAdd(&out, lookup, new, path)
			case new == nil:
				flattenDel(&out, lookup, old, path)
			case old.IsDir && new.IsDir:
				if err := walk(old.Target, new.Target, path+"/"); err != nil {
					return err
				}
			case old.IsDir != new.IsDir:
				flattenDel(&out, lookup, old, path)
				flattenAdd(&out, lookup, new, path)
			case old.Target == new.Target && old.Mode == new.Mode:
				// unchanged
			default:
				out = append(out, treeChange{path: path, oldOID: old.Target, newOID: new.Target,
					oldMode: old.Mode, newMode: new.Mode})
			}
		}
		return nil
	}
	if err := walk(fromTree, toTree, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// flattenAdd/Del expand directory adds/deletes down to file entries.
func flattenAdd(out *[]treeChange, lookup model.ObjectLookup, e *model.TreeEntry, path string) {
	if !e.IsDir {
		*out = append(*out, treeChange{path: path, newOID: e.Target, newMode: e.Mode})
		return
	}
	if t, err := lookup.Tree(e.Target); err == nil {
		for i := range t.Entries {
			flattenAdd(out, lookup, &t.Entries[i], path+"/"+t.Entries[i].Name)
		}
	}
}

func flattenDel(out *[]treeChange, lookup model.ObjectLookup, e *model.TreeEntry, path string) {
	if !e.IsDir {
		*out = append(*out, treeChange{path: path, oldOID: e.Target, oldMode: e.Mode})
		return
	}
	if t, err := lookup.Tree(e.Target); err == nil {
		for i := range t.Entries {
			flattenDel(out, lookup, &t.Entries[i], path+"/"+t.Entries[i].Name)
		}
	}
}

func sortedNames(m map[string][2]*model.TreeEntry) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// pathFilter matches a change against request pathspecs (path or directory
// prefix, the pathspec semantics this surface needs).
type pathFilter [][]byte

func (p pathFilter) matches(path string) bool {
	if len(p) == 0 {
		return true
	}
	for _, spec := range p {
		s := string(spec)
		if path == s || strings.HasPrefix(path, s+"/") {
			return true
		}
	}
	return false
}

// diffBinaryThreshold mirrors git's binary detection window.
const diffBinaryThreshold = 8000

func looksBinary(b []byte) bool {
	if len(b) > diffBinaryThreshold {
		b = b[:diffBinaryThreshold]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// patchStats counts additions/deletions between two contents.
func patchStats(oldContent, newContent []byte) (added, removed int32) {
	// A trailing chunk without a newline is still a line (git --numstat
	// semantics); counting only "\n" misses files with no trailing newline.
	count := func(text string) int32 {
		if text == "" {
			return 0
		}
		n := int32(strings.Count(text, "\n"))
		if !strings.HasSuffix(text, "\n") {
			n++
		}
		return n
	}
	for _, d := range lineDiffOps(string(oldContent), string(newContent)) {
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			added += count(d.Text)
		case diffmatchpatch.DiffDelete:
			removed += count(d.Text)
		}
	}
	return added, removed
}

func lineDiffOps(a, b string) []diffmatchpatch.Diff {
	dmp := diffmatchpatch.New()
	ra, rb, lines := dmp.DiffLinesToRunes(a, b)
	diffs := dmp.DiffMainRunes(ra, rb, false)
	return dmp.DiffCharsToLines(diffs, lines) // decode runes back to line text
}

// diffContext is the unified-diff context band (git default 3).
const diffContext = 3

type diffLine struct {
	op   diffmatchpatch.Operation
	text string
}

// unifiedPatch renders a git-style patch (headers + @@ hunks) for one
// change. Binary pairs yield a nil body — callers set the Binary flag.
// unifiedPatch renders a git-style patch (headers + @@ hunks) for one
// change. Binary pairs yield a nil body — callers set the Binary flag.
func unifiedPatch(oldContent, newContent []byte, oldPath, newPath string) []byte {
	if looksBinary(oldContent) || looksBinary(newContent) {
		return nil
	}
	var lines []diffLine
	for _, d := range lineDiffOps(string(oldContent), string(newContent)) {
		for _, l := range splitLines(d.Text) {
			lines = append(lines, diffLine{d.Type, l})
		}
	}
	n := len(lines)
	changed := make([]bool, n)
	oldNo := make([]int, n) // 0-based old-side line number at this line
	newNo := make([]int, n)
	oi, ni := 0, 0
	for i, l := range lines {
		oldNo[i], newNo[i] = oi, ni
		changed[i] = l.op != diffmatchpatch.DiffEqual
		if l.op != diffmatchpatch.DiffInsert {
			oi++
		}
		if l.op != diffmatchpatch.DiffDelete {
			ni++
		}
	}
	var out bytes.Buffer
	oldHdr, newHdr := oldPath, newPath
	if oldHdr != "/dev/null" {
		oldHdr = "a/" + oldHdr
	}
	if newHdr != "/dev/null" {
		newHdr = "b/" + newHdr
	}
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldHdr, newHdr)
	i := 0
	for i < n {
		if !changed[i] {
			i++
			continue
		}
		// Cluster: extend while the equal gap between changes stays within
		// 2*context lines (git's inter-hunk grouping).
		last := i
		for k := i + 1; k < n; k++ {
			if changed[k] {
				last = k
				continue
			}
			gapEnd := k
			for gapEnd < n && !changed[gapEnd] {
				gapEnd++
			}
			if gapEnd-k > 2*diffContext || gapEnd >= n {
				break
			}
			k = gapEnd - 1
		}
		start := i - diffContext
		if start < 0 {
			start = 0
		}
		end := last + 1 + diffContext
		if end > n {
			end = n
		}
		oCount, nCount := 0, 0
		for _, l := range lines[start:end] {
			switch l.op {
			case diffmatchpatch.DiffEqual:
				oCount, nCount = oCount+1, nCount+1
			case diffmatchpatch.DiffDelete:
				oCount++
			case diffmatchpatch.DiffInsert:
				nCount++
			}
		}
		os, ns := oldNo[start]+1, newNo[start]+1
		if oCount == 0 {
			os--
		}
		if nCount == 0 {
			ns--
		}
		rng := func(start, count int) string {
			if count == 1 {
				return fmt.Sprintf("%d", start)
			}
			return fmt.Sprintf("%d,%d", start, count)
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", rng(os, oCount), rng(ns, nCount))
		for _, l := range lines[start:end] {
			switch l.op {
			case diffmatchpatch.DiffEqual:
				out.WriteString(" " + l.text)
			case diffmatchpatch.DiffDelete:
				out.WriteString("-" + l.text)
			case diffmatchpatch.DiffInsert:
				out.WriteString("+" + l.text)
			}
		}
		i = end
	}
	return out.Bytes()
}


func splitLines(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s+"\n")
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}
