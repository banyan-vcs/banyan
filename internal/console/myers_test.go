package console

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func script(t *testing.T, base, head string) []DiffLine {
	t.Helper()
	hunks, ok := RenderUnifiedDiff([]byte(base), []byte(head))
	require.True(t, ok)
	var out []DiffLine
	for _, h := range hunks {
		out = append(out, h.Lines...)
	}
	return out
}

// Roundtrip: applying '-' removals and '+' additions to base yields head.
func applyScript(base string, lines []DiffLine) string {
	cur := SplitLines([]byte(base))
	ai, bi := 0, 0
	out := []string{}
	for _, l := range lines {
		switch l.Kind {
		case ' ':
			out = append(out, cur[ai])
			ai++
			bi++
		case '-':
			ai++
		case '+':
			out = append(out, l.Text)
			bi++
		}
	}
	out = append(out, cur[ai:]...)
	return joinLines(out)
}

func joinLines(ls []string) string {
	s := ""
	for _, l := range ls {
		s += l + "\n"
	}
	return s
}

func TestMyersRoundtrip(t *testing.T) {
	cases := []struct{ base, head string }{
		{"a\nb\nc\n", "a\nb\nc\n"}, // identical
		{"a\nb\nc\n", "a\nB\nc\n"}, // modify
		{"a\nb\n", "a\nb\nc\nd\n"}, // append
		{"a\nb\nc\nd\n", "a\nd\n"}, // delete middle
		{"", "new\nfile\n"},        // from empty
		{"gone\naway\n", ""},       // to empty
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "1\n2\n3\nx\n7\n8\n9\n10\n"}, // two edits
		{"a\n", "b\n"}, // replace single
	}
	for _, c := range cases {
		got := applyScript(c.base, script(t, c.base, c.head))
		assert.Equal(t, c.head, got, "base=%q head=%q", c.base, c.head)
	}
}

func TestMyersHunkShape(t *testing.T) {
	// One modification deep inside a long file: exactly one hunk, bounded
	// context (3 before/after), correct side starts and counts.
	base, head := "", ""
	for i := 1; i <= 20; i++ {
		line := "line" + string(rune('a'+i-1))
		base += line + "\n"
		if i == 10 {
			head += "CHANGED\n"
		} else {
			head += line + "\n"
		}
	}
	hunks, ok := RenderUnifiedDiff([]byte(base), []byte(head))
	require.True(t, ok)
	require.Len(t, hunks, 1)
	h := hunks[0]
	// 3 context + '-' + '+' + 3 context = 8 lines.
	assert.Len(t, h.Lines, 8)
	assert.Equal(t, 7, h.AStart) // line 10 minus 3 context
	assert.Equal(t, 7, h.BStart)
	assert.Equal(t, 7, h.ACount)
	assert.Equal(t, 7, h.BCount)
	kinds := ""
	for _, l := range h.Lines {
		kinds += string(l.Kind)
	}
	assert.Equal(t, "   -+   ", kinds)
}

func TestMyersSeparatedChangesTwoHunks(t *testing.T) {
	base := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n"
	head := "1x\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15x\n"
	hunks, ok := RenderUnifiedDiff([]byte(base), []byte(head))
	require.True(t, ok)
	require.Len(t, hunks, 2, "far-apart changes must not merge: %+v", hunks)
}

func TestMyersBinaryAndCaps(t *testing.T) {
	_, ok := RenderUnifiedDiff([]byte{0, 1, 2}, []byte{0, 1, 3})
	assert.False(t, ok, "binary")
	big := make([]byte, diffMaxContent+1)
	for i := range big {
		big[i] = 'x'
	}
	_, ok = RenderUnifiedDiff(big, big)
	assert.False(t, ok, "oversized")
}
