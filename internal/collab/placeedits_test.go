package collab

import (
	"slices"
	"testing"
)

func TestPlaceEditsSplicesOnlyTheChange(t *testing.T) {
	text := "# Setup\n\nSome text here.\nMore text.\n"
	for _, c := range []struct {
		name  string
		edits []TextEdit
		want  []splice
	}{
		{"an insertion with context", []TextEdit{{Old: "Some text here.", New: "Some new text here."}},
			[]splice{{pos: 14, insert: "new "}}},
		{"one word replaced", []TextEdit{{Old: "Some text here.", New: "Some words here."}},
			[]splice{{pos: 14, del: 4, insert: "words"}}},
		{"a deletion with context", []TextEdit{{Old: "Some text here.\n", New: "Some here.\n"}},
			[]splice{{pos: 14, del: 5}}},
		{"old equals new", []TextEdit{{Old: "More text.", New: "More text."}}, nil},
		{"in order, on the text left by the one before", []TextEdit{
			{Old: "Some text", New: "Some fine text"}, {Old: "fine text here", New: "fine text there"}},
			[]splice{{pos: 14, insert: "fine "}, {pos: 24, insert: "t"}}},
		// "aa" to "aaa": the common ends do not overlap
		{"repeated characters", []TextEdit{{Old: "More", New: "Moore"}}, []splice{{pos: 27, insert: "o"}}},
		// code points: ä and ö share their first UTF-8 byte
		{"code points", []TextEdit{{Old: "# Setup", New: "# Sätup"}}, []splice{{pos: 3, del: 1, insert: "ä"}}},
	} {
		got, err := placeEdits(text, c.edits)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: splices = %+v, want %+v", c.name, got, c.want)
		}
	}
	// the uniqueness check stays on the whole old
	if _, err := placeEdits("ab ab", []TextEdit{{Old: "ab", New: "abc"}}); err == nil {
		t.Error("an ambiguous old passed")
	}
}

func TestCommonEnds(t *testing.T) {
	for _, c := range []struct {
		a, b           string
		prefix, suffix int
	}{
		{"abc", "abc", 3, 0},
		{"abc", "axc", 1, 1},
		{"aa", "aaa", 2, 0},
		{"aaa", "aa", 2, 0},
		{"ö", "ä", 0, 0},
		{"xöy", "xäy", 1, 1},
		{"", "a", 0, 0},
	} {
		if p, s := commonEnds(c.a, c.b); p != c.prefix || s != c.suffix {
			t.Errorf("commonEnds(%q, %q) = %d, %d, want %d, %d", c.a, c.b, p, s, c.prefix, c.suffix)
		}
	}
}
