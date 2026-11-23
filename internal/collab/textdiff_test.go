package collab

import "testing"

func TestFocusOfSplices(t *testing.T) {
	for _, c := range []struct {
		name    string
		splices []splice
		want    int
		ok      bool
	}{
		{"none", nil, 0, false},
		{"one insert", []splice{{pos: 3, insert: "abc"}}, 6, true},
		{"replace", []splice{{pos: 9, del: 10, insert: "Agent text."}}, 20, true},
		{"delete only", []splice{{pos: 4, del: 2}}, 4, true},
		// the last insert in order, not the highest position, moved by the splices after it
		{"insert, then an insert before it", []splice{{pos: 10, insert: "xy"}, {pos: 0, insert: "ab"}}, 2, true},
		{"insert, then a delete before it", []splice{{pos: 10, insert: "xy"}, {pos: 0, del: 3}}, 9, true},
		{"insert, then a delete after it", []splice{{pos: 10, insert: "xy"}, {pos: 20, del: 3}}, 12, true},
		{"insert, then a delete over its end", []splice{{pos: 10, insert: "xy"}, {pos: 11, del: 3}}, 11, true},
		{"code points", []splice{{pos: 1, insert: "😀😀"}}, 3, true},
	} {
		got, ok := focusOfSplices(c.splices)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %d, %v, want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
