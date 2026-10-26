package console

import (
	"bytes"
)

// Line-level unified diff for the review surface: Myers O(ND) over split
// lines, rendered as context-3 hunks. Demo-scale by contract — callers cap
// content size and line count before calling; beyond that a review shows the
// file list only.

type DiffLine struct {
	Kind byte   // ' ' | '-' | '+'
	Text string // without the trailing newline
}

type Hunk struct {
	AStart, ACount int // base side (1-based)
	BStart, BCount int // head side (1-based)
	Lines          []DiffLine
}

const (
	diffMaxLines   = 20000 // per side; larger files render as file-list only
	diffMaxContent = 512 << 10
	diffContext    = 3
)

// SplitLines splits content into lines (terminators dropped, CRLF handled).
func SplitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte("\n"))
	if len(parts[len(parts)-1]) == 0 { // trailing newline: not an extra line
		parts = parts[:len(parts)-1]
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(bytes.TrimSuffix(p, []byte("\r")))
	}
	return out
}

// IsBinary mirrors git's heuristic: a NUL byte in the first 8 KiB.
func IsBinary(b []byte) bool {
	probe := b
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

// myersDiff computes the full edit script (equal lines included) via the
// classic greedy algorithm with a V-trace for backtracking (Myers 1986).
func myersDiff(a, b []string) []DiffLine {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	max := n + m
	offset := max
	v := make([]int, 2*max+1)
	trace := make([][]int, 0, max+1)
outer:
	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1] // down: insertion
			} else {
				x = v[offset+k-1] + 1 // right: deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[offset+k] = x
			if x >= n && y >= m {
				break outer
			}
		}
	}

	// Backtrack from (n, m) building the script in reverse.
	script := make([]DiffLine, 0, n+m)
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		vv := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vv[offset+k-1] < vv[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vv[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY { // the snake into this point
			script = append(script, DiffLine{' ', a[x-1]})
			x, y = x-1, y-1
		}
		if x == prevX { // arrived by a down step: b[prevY] inserted
			script = append(script, DiffLine{'+', b[prevY]})
			y--
		} else { // arrived by a right step: a[prevX] deleted
			script = append(script, DiffLine{'-', a[prevX]})
			x--
		}
	}
	for x > 0 && y > 0 { // the initial d=0 snake
		script = append(script, DiffLine{' ', a[x-1]})
		x, y = x-1, y-1
	}
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return script
}

// Hunks collapses a full edit script into context hunks: groups of changes
// whose gap is ≤ 2×context merge; each group carries diffContext lines of
// surrounding context.
func Hunks(lines []DiffLine) []Hunk {
	// Side positions at every line index, and which lines carry a change.
	type pos struct{ a, b int }
	starts := make([]pos, len(lines))
	changed := make([]bool, len(lines))
	aPos, bPos := 0, 0
	for i, l := range lines {
		starts[i] = pos{aPos, bPos}
		switch l.Kind {
		case ' ':
			aPos, bPos = aPos+1, bPos+1
		case '-':
			aPos++
			changed[i] = true
		case '+':
			bPos++
			changed[i] = true
		}
	}

	var hunks []Hunk
	i := 0
	for i < len(lines) {
		if !changed[i] {
			i++
			continue
		}
		// Extend the group while the next change sits within 2×context.
		last := i
		for j := i + 1; j < len(lines); j++ {
			if changed[j] {
				if j-last <= 2*diffContext {
					last = j
				} else {
					break
				}
			}
		}
		lo, hi := i-diffContext, last+diffContext+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		h := Hunk{Lines: lines[lo:hi], AStart: starts[lo].a + 1, BStart: starts[lo].b + 1}
		for _, l := range h.Lines {
			if l.Kind == ' ' || l.Kind == '-' {
				h.ACount++
			}
			if l.Kind == ' ' || l.Kind == '+' {
				h.BCount++
			}
		}
		hunks = append(hunks, h)
		i = hi
	}
	return hunks
}

// RenderUnifiedDiff produces the hunks for one file's two sides; ok=false
// marks binary or oversized content (callers show a placeholder instead).
func RenderUnifiedDiff(base, head []byte) (hunks []Hunk, ok bool) {
	if IsBinary(base) || IsBinary(head) {
		return nil, false
	}
	if len(base) > diffMaxContent || len(head) > diffMaxContent {
		return nil, false
	}
	a, b := SplitLines(base), SplitLines(head)
	if len(a) > diffMaxLines || len(b) > diffMaxLines {
		return nil, false
	}
	return Hunks(myersDiff(a, b)), true
}
