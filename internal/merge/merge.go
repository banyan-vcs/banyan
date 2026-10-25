// Package merge implements the three-way merge subset banyan's convergence
// needs (docs/15 §2.2): blob-level diff3 in the xdl_merge structure (change
// intervals from two pairwise diffs against the base, disjoint changes
// applied, overlaps conflicted with git-compatible markers) and directory
// tree entry merge (the merge-ort process_entries trivial subset plus
// recursive content merge). Pure Go — the diff engine is go-diff, no git
// CLI involvement (behavioral reference: xdiff/xmerge.c, verified against
// `git merge-file` in the oracle tests).
package merge

import (
	"bytes"
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Options labels the conflict markers (git merge-file style, no ancestor
// section — the default "merge" conflict style).
type Options struct {
	OursLabel   string
	TheirsLabel string
}

// isBinary mirrors git's buffer_is_binary: a NUL byte in the first 8KiB.
func isBinary(b []byte) bool {
	if len(b) > 8192 {
		b = b[:8192]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// region is one side's change against the base: base lines [b0,b1) are
// replaced by side lines [s0,s1) (b0==b1 marks a pure insertion). The
// xdl_merge xdmerge_t equivalent.
type region struct {
	b0, b1 int
	s0, s1 int
}

// Blobs merges ours and theirs changes to base. The result carries conflict
// markers wherever both sides changed overlapping base lines (or both
// inserted at the same point); clean reports whether any conflict occurred.
// Binary input (any side) is never merged: ours is returned, clean=false —
// ll_binary_merge semantics.
func Blobs(base, ours, theirs []byte, o Options) ([]byte, bool) {
	if isBinary(base) || isBinary(ours) || isBinary(theirs) {
		return ours, false
	}
	if bytes.Equal(ours, theirs) {
		return ours, true
	}
	if bytes.Equal(base, ours) {
		return theirs, true
	}
	if bytes.Equal(base, theirs) {
		return ours, true
	}

	baseL := splitLines(base)
	oursL := splitLines(ours)
	theirsL := splitLines(theirs)
	ro := changes(baseL, oursL)
	rt := changes(baseL, theirsL)

	var out bytes.Buffer
	clean := true
	bi, i, j := 0, 0, 0
	for i < len(ro) || j < len(rt) {
		switch {
		case j >= len(rt) || (i < len(ro) && ro[i].b0 < rt[j].b0):
			r := ro[i]
			writeLines(&out, baseL[bi:r.b0])
			writeLines(&out, oursL[r.s0:r.s1])
			bi = r.b1
			i++
		case i >= len(ro) || rt[j].b0 < ro[i].b0:
			r := rt[j]
			writeLines(&out, baseL[bi:r.b0])
			writeLines(&out, theirsL[r.s0:r.s1])
			bi = r.b1
			j++
		default: // same base start: identical change applies once
			r := ro[i]
			if sameContent(ro[i], rt[j], oursL, theirsL) {
				writeLines(&out, baseL[bi:r.b0])
				writeLines(&out, oursL[r.s0:r.s1])
				bi = r.b1
				i++
				j++
				continue
			}
			// Conflict: expand to the union span, collecting every region
			// of both sides inside it (expansion may pull more in), then
			// render each side's version of the span between markers.
			u0, u1 := r.b0, r.b1
			var oc, tc []region
			oc = append(oc, ro[i])
			i++
			if rt[j].b0 == u0 {
				tc = append(tc, rt[j])
				if rt[j].b1 > u1 {
					u1 = rt[j].b1
				}
				j++
			}
			for i < len(ro) && ro[i].b0 < u1 {
				oc = append(oc, ro[i])
				if ro[i].b1 > u1 {
					u1 = ro[i].b1
				}
				i++
			}
			for j < len(rt) && rt[j].b0 < u1 {
				tc = append(tc, rt[j])
				if rt[j].b1 > u1 {
					u1 = rt[j].b1
				}
				j++
				for i < len(ro) && ro[i].b0 < u1 {
					oc = append(oc, ro[i])
					if ro[i].b1 > u1 {
						u1 = ro[i].b1
					}
					i++
				}
			}
			writeLines(&out, baseL[bi:u0])
			bi = u1
			clean = false
			writeMarker(&out, "<<<<<<< "+label(o.OursLabel, "ours"), oursSpan(baseL, oursL, oc, u0, u1))
			writeMarker(&out, "=======", theirsSpan(baseL, theirsL, tc, u0, u1))
			writeMarker(&out, ">>>>>>> "+label(o.TheirsLabel, "theirs"), nil)
		}
	}
	writeLines(&out, baseL[bi:])
	return out.Bytes(), clean
}

// span materializes one side's version of base span [u0,u1): its
// replacements for the regions inside the span plus the untouched base
// lines between them (diff3 semantics).
func span(baseL, sideL []string, regs []region, u0, u1 int) []string {
	var out []string
	pos := u0
	for _, r := range regs {
		// Inside the span: a normal region overlaps it; a zero-length
		// insertion sits at a point within it (u0 <= b0 <= u1).
		inside := (r.b0 < u1 && r.b1 > u0) || (r.b0 == r.b1 && r.b0 >= u0 && r.b0 <= u1)
		if !inside {
			continue
		}
		if r.b0 > pos {
			out = append(out, baseL[pos:r.b0]...)
			pos = r.b0
		}
		out = append(out, sideL[r.s0:r.s1]...)
		if r.b1 > pos {
			pos = r.b1
		}
	}
	out = append(out, baseL[pos:u1]...)
	return out
}

func oursSpan(baseL, sideL []string, regs []region, u0, u1 int) []string {
	return span(baseL, sideL, regs, u0, u1)
}

func theirsSpan(baseL, sideL []string, regs []region, u0, u1 int) []string {
	return span(baseL, sideL, regs, u0, u1)
}

func writeMarker(out *bytes.Buffer, marker string, lines []string) {
	ensureNewline(out)
	out.WriteString(marker + "\n")
	writeLines(out, lines)
}

func ensureNewline(out *bytes.Buffer) {
	if out.Len() > 0 && out.Bytes()[out.Len()-1] != '\n' {
		out.WriteByte('\n')
	}
}

func label(l, def string) string {
	if l == "" {
		return def
	}
	return l
}

func sameContent(a, b region, oursL, theirsL []string) bool {
	return a.b0 == b.b0 && a.b1 == b.b1 &&
		equalLines(oursL[a.s0:a.s1], theirsL[b.s0:b.s1])
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitLines keeps the trailing newline on each line (the last line may
// lack one — it is still a line).
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, string(b[start:i+1]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func writeLines(out *bytes.Buffer, lines []string) {
	for _, l := range lines {
		out.WriteString(l)
	}
}

// changes computes one side's change regions against the base via the diff
// engine: contiguous Delete(+adjacent Insert) runs collapse into regions.
// DiffLinesToRunes encodes every line of both texts into one rune
// vocabulary, so the diff walks whole lines as atoms.
func changes(baseL, sideL []string) []region {
	dmp := diffmatchpatch.New()
	rb, rs, _ := dmp.DiffLinesToRunes(join(baseL), join(sideL))
	diffs := dmp.DiffMainRunes(rb, rs, false)
	var regs []region
	bi, si := 0, 0
	cur := region{-1, -1, -1, -1}
	flush := func() {
		if cur.b0 >= 0 {
			regs = append(regs, cur)
			cur = region{-1, -1, -1, -1}
		}
	}
	for _, d := range diffs {
		n := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			flush()
			bi += n
			si += n
		case diffmatchpatch.DiffDelete:
			if cur.b0 < 0 {
				cur = region{b0: bi, b1: bi, s0: si, s1: si}
			}
			bi += n
		case diffmatchpatch.DiffInsert:
			if cur.b0 < 0 {
				cur = region{b0: bi, b1: bi, s0: si, s1: si}
			}
			si += n
		}
		if cur.b0 >= 0 {
			cur.b1, cur.s1 = bi, si
		}
	}
	flush()
	return regs
}

// join maps lines back to text for the runes encoding: every line (in both
// inputs, in order) shares one vocabulary — DiffLinesToRunes builds it from
// both texts itself, so join simply reconstructs each text.
func join(lines []string) string {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
	}
	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
