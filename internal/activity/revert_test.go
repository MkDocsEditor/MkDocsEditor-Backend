package activity

import (
	"fmt"
	"testing"
)

func TestRevertChange(t *testing.T) {
	tests := []struct {
		name, current, before, after string
		want                         string
		conflicts                    []int
	}{
		{"added line, edit next to it", "a\nX\nb\nc\n", "a\nb\n", "a\nX\nb\n", "a\nb\nc\n", nil},
		{"added line, line added right below", "a\nX\nY\nb\n", "a\nb\n", "a\nX\nb\n", "a\nY\nb\n", nil},
		{"removed line comes back", "a\nc\nd\n", "a\nb\nc\n", "a\nc\n", "a\nb\nc\nd\n", nil},
		{"changed line", "x\na\nB2\nc\n", "a\nb\nc\n", "a\nB2\nc\n", "x\na\nb\nc\n", nil},
		{"changed again", "a\nB3\nc\n", "a\nb\nc\n", "a\nB2\nc\n", "", []int{1}},
		{"removed since", "a\nc\n", "a\nb\nc\n", "a\nB2\nc\n", "", []int{1}},
		{"no line end at the end", "a\nb\nX", "a\nb", "a\nb\nX", "a\nb", nil},
		{"line end kept elsewhere", "a\nX\nb", "a\nb\n", "a\nX\nb\n", "a\nb", nil},
		{"first text", "a\n", "", "a\n", "", nil},
		{"into an empty text", "", "a\n", "", "a\n", nil},
		{"two blocks", "a\nX\nb\nc\nY\nd\n", "a\nb\nc\nd\n", "a\nX\nb\nc\nY\nd\n", "a\nb\nc\nd\n", nil},
	}
	for _, tt := range tests {
		got, conflicts := RevertChange(tt.current, tt.before, tt.after)
		if got != tt.want || fmt.Sprint(conflicts) != fmt.Sprint(tt.conflicts) {
			t.Errorf("%s: got %q %v, want %q %v", tt.name, got, conflicts, tt.want, tt.conflicts)
		}
	}
}
