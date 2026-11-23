package collab

import (
	"strings"
	"testing"
)

func TestIndexMap(t *testing.T) {
	old := "Hello world. Some text."
	current := "Hi world. Some more text."
	m := &indexMap{diffs: dmp.DiffCleanupSemantic(dmp.DiffMain(old, current, false))}
	length := len([]rune(current))
	for _, c := range []struct{ index, want int }{
		{0, 0}, // before the change
		{strings.Index(old, "world"), strings.Index(current, "world")}, // after a replaced part
		{strings.Index(old, "text"), strings.Index(current, "text")},   // after an insertion before it
		{len(old), length},     // the end
		{len(old) + 5, length}, // beyond the end
	} {
		if got := m.mapIndex(c.index, length); got != c.want {
			t.Errorf("mapIndex(%d) = %d, want %d", c.index, got, c.want)
		}
	}
	// an index in deleted text goes to the place of the deletion
	m = &indexMap{diffs: dmp.DiffMain("abcdef", "abef", false)}
	if got := m.mapIndex(3, 4); got != 2 {
		t.Errorf("deleted = %d", got)
	}
	// without a map the index is clamped
	var none *indexMap
	if got := none.mapIndex(10, 4); got != 4 {
		t.Errorf("clamped = %d", got)
	}
	// code points, not bytes
	m = &indexMap{diffs: dmp.DiffMain("äö x", "Xäö x", false)}
	if got := m.mapIndex(3, 5); got != 4 {
		t.Errorf("code points = %d", got)
	}
}

func TestParsePresenceIndexes(t *testing.T) {
	rev := strings.Repeat("a", 64)
	for _, c := range []struct {
		data string
		ok   bool
	}{
		{`{"at":{"revision":"` + rev + `","selections":[{"anchor":1,"head":3}],"viewport":{"top":0,"bottom":9}}}`, true},
		{`{"at":{"revision":"` + rev + `","selections":[]}}`, true},
		{`{}`, false},
		{`{"at":null}`, false},
		{`{"at":{"revision":"nope","selections":[]}}`, false},
		{`{"at":{"revision":"` + rev + `","selections":[{"anchor":-1,"head":3}]}}`, false},
		{`{"at":{"revision":"` + rev + `","selections":[],"viewport":{"top":5,"bottom":2}}}`, false},
		{`{"at":{"revision":"` + rev + `","selections":[],"x":"` + strings.Repeat("x", maxPresenceIndexesSize) + `"}}`, false},
	} {
		if got := parsePresenceIndexes([]byte(c.data)) != nil; got != c.ok {
			t.Errorf("%.80s: %v", c.data, got)
		}
	}
}
