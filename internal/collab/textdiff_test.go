package collab

import (
	"slices"
	"strings"
	"testing"

	automerge "github.com/automerge/automerge-go"
)

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

// spliceDiffOn applies spliceDiff to a new document with the text oldText and checks the result
func spliceDiffOn(t *testing.T, oldText string, newText string) []splice {
	t.Helper()
	doc := automerge.New()
	if err := doc.Path(ContentKey).Set(automerge.NewText(oldText)); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Commit("old"); err != nil {
		t.Fatal(err)
	}
	splices, err := spliceDiff(doc.Path(ContentKey).Text(), oldText, newText)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ContentText(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got != newText {
		t.Fatalf("text = %q, want %q", got, newText)
	}
	return splices
}

// A write of the whole text changes only what differs, so the rest keeps its identity and the
// cursors of others in it stay where they are
func TestSpliceDiffChangesOnlyWhatDiffers(t *testing.T) {
	para := func(name string) string {
		return "## " + name + "\n\nThis paragraph is about " + name + " and has a few words in it.\n\n"
	}
	text := para("one") + para("two") + para("three") + para("four")

	// one word in the middle
	changed := strings.Replace(text, "about two", "about 2", 1)
	at := strings.Index(text, "about two") + len("about ")
	if got, want := spliceDiffOn(t, text, changed), []splice{{pos: at, del: 3}, {pos: at, insert: "2"}}; !slices.Equal(got, want) {
		t.Errorf("one word: splices = %+v, want %+v", got, want)
	}

	// the same text: nothing
	if got := spliceDiffOn(t, text, text); got != nil {
		t.Errorf("same text: splices = %+v", got)
	}

	// a paragraph moved: it is removed and inserted again, everything else stays
	moved := para("one") + para("three") + para("two") + para("four")
	splices := spliceDiffOn(t, text, moved)
	touched := 0
	for _, sp := range splices {
		touched += sp.del + len(sp.insert)
	}
	if limit := 2 * len(para("two")); touched > limit {
		t.Errorf("moved paragraph: %d characters deleted or inserted, want at most %d: %+v", touched, limit, splices)
	}
}
