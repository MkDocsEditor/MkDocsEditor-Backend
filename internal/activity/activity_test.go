package activity

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	s, err := open(t.TempDir(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

func feed(t *testing.T, s *Store) []Entry {
	t.Helper()
	s.Wait()
	entries, _, err := s.Feed("", 100)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestSummarize(t *testing.T) {
	before := "intro\n# One\na\nb\n```\n# not a heading\n```\n## Two ##\nc\n# Three\nd\n"
	after := "intro\n# One\na\nB\n```\n# not a heading\n```\n## Two ##\nc\nnew\n# Three\n"
	got := Summarize(before, after)
	want := Summary{LinesAdded: 2, LinesRemoved: 2, Headings: []string{"One", "Two", "Three"}, TextHash: TextHash(after),
		Ranges: []Range{{Line: 4, Count: 1, Heading: ptr("One"), HeadingOccurrence: 1, Offset: 2},
			// "new" and the removed "d" are 1 line apart: one range
			{Line: 10, Count: 2, Heading: ptr("Two"), HeadingOccurrence: 1, Offset: 2}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if got := Summarize("a\nb", "a\nb\nc"); got.LinesAdded != 1 || got.LinesRemoved != 0 || len(got.Headings) != 0 {
		t.Errorf("last line without a break: %+v", got)
	}
	many := ""
	for _, h := range []string{"A", "B", "C", "D", "E", "F"} {
		many += "# " + h + "\ntext\n"
	}
	changed := ""
	for _, h := range []string{"A", "B", "C", "D", "E", "F"} {
		changed += "# " + h + "\nchanged\n"
	}
	if got := Summarize(many, changed); !reflect.DeepEqual(got.Headings, []string{"A", "B", "C", "D", "E"}) {
		t.Errorf("headings = %v", got.Headings)
	}
}

func ptr(s string) *string { return &s }

func TestRanges(t *testing.T) {
	str := func(rs []Range) string {
		out := []string{}
		for _, r := range rs {
			h := "-"
			if r.Heading != nil {
				h = *r.Heading
			}
			out = append(out, fmt.Sprintf("%d+%d@%s#%d+%d", r.Line, r.Count, h, r.HeadingOccurrence, r.Offset))
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct{ name, before, after, want string }{
		{"removed only", "a\nb\nc\n", "a\nc\n", "2+0@-#0+1"},
		{"removed at the end", "# H\na\nb\n", "# H\na\n", "3+0@H#1+2"},
		{"everything removed", "a\n", "", "1+0@-#0+0"},
		{"repeated heading", "# Notes\nx\n# Notes\ny\n", "# Notes\nx\n# Notes\nY\n", "4+1@Notes#2+1"},
		{"front matter is no heading", "---\n# comment\n---\na\n", "---\n# comment\n---\nA\n", "4+1@-#0+3"},
		{"CR stays in the line", "a\r\nb\r\n", "a\r\nB\r\n", "2+1@-#0+1"},
		{"far apart", "a\nb\nc\nd\ne\nf\ng\n", "A\nb\nc\nd\ne\nf\nG\n", "1+1@-#0+0 7+1@-#0+6"},
		{"near", "a\nb\nc\nd\ne\n", "A\nb\nc\nd\nE\n", "1+5@-#0+0"},
	} {
		if got := str(Summarize(tc.before, tc.after).Ranges); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}

	// at most maxRanges: the closest neighbours merge, every change stays covered
	var before, after strings.Builder
	for i := 0; i < 30; i++ {
		gap := 5
		if i%2 == 1 {
			gap = 10
		}
		before.WriteString("x\n")
		after.WriteString("changed\n")
		for j := 0; j < gap; j++ {
			before.WriteString("same\n")
			after.WriteString("same\n")
		}
	}
	rs := Summarize(before.String(), after.String()).Ranges
	covered := 0
	for i, r := range rs {
		covered += r.Count
		if i > 0 && r.Line <= rs[i-1].Line+rs[i-1].Count-1 {
			t.Errorf("ranges overlap or are out of order: %s", str(rs))
		}
	}
	last := rs[len(rs)-1]
	// the last changed line is 245: 30 blocks of 1 + 5 or 10 lines, the last of 11 lines
	if len(rs) != maxRanges || rs[0].Line != 1 || last.Line > 245 || last.Line+last.Count-1 < 245 || covered < 30 {
		t.Errorf("%d ranges: %s", len(rs), str(rs))
	}
	if TextHash("") != "e3b0c44298fc1c14" {
		t.Errorf("hash = %s", TextHash(""))
	}
}

func TestEditSessions(t *testing.T) {
	s, c := newStore(t)
	s.RecordEdit("a.md", "one\n", "one\ntwo\n", []string{"alice"})
	c.advance(5 * time.Minute)
	s.RecordEdit("a.md", "one\ntwo\n", "one\ntwo\nthree\n", []string{"bob", "alice"})
	entries := feed(t, s)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	if e.Kind != KindEdited || !reflect.DeepEqual(e.Authors, []string{"alice", "bob"}) || e.Summary.LinesAdded != 2 ||
		!e.Time.Equal(c.now()) {
		t.Errorf("entry = %+v %+v", e, e.Summary)
	}

	// back to the start text: no entry
	s.RecordEdit("a.md", "one\ntwo\nthree\n", "one\n", []string{"alice"})
	if entries := feed(t, s); len(entries) != 0 {
		t.Fatalf("after the revert: %+v", entries)
	}

	// a pause longer than the session starts a new entry
	s.RecordEdit("a.md", "one\n", "one\nx\n", []string{"alice"})
	c.advance(EditSession + time.Second)
	s.RecordEdit("a.md", "one\nx\n", "one\n", []string{"alice"})
	if entries := feed(t, s); len(entries) != 2 || entries[0].Summary.LinesRemoved != 1 {
		t.Fatalf("after the pause: %+v", entries)
	}

	// a move ends the session
	s.RecordItem(KindMoved, "document", "b.md", "a.md", "alice")
	s.RecordEdit("a.md", "one\n", "one\ny\n", []string{"alice"})
	if entries := feed(t, s); len(entries) != 4 || entries[0].Kind != KindEdited || entries[1].Kind != KindMoved || entries[1].OldPath != "a.md" {
		t.Fatalf("after the move: %+v", entries)
	}
}

func TestFeedPagesAndRetention(t *testing.T) {
	s, c := newStore(t)
	for range 5 {
		s.RecordItem(KindCreated, "document", "x.md", "", "alice")
		c.advance(time.Second)
	}
	s.Wait()
	var all []Entry
	cursor := ""
	for {
		page, next, err := s.Feed(cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(all) != 5 {
		t.Fatalf("got %d entries", len(all))
	}
	for i := 1; i < len(all); i++ {
		if !all[i].Time.Before(all[i-1].Time) {
			t.Errorf("not newest first: %v", all)
		}
	}
	if _, _, err := s.Feed("garbage", 2); err != ErrInvalidCursor {
		t.Errorf("err = %v", err)
	}

	// the newest is 1 s old
	c.advance(Retention - 1500*time.Millisecond)
	if entries := feed(t, s); len(entries) != 1 {
		t.Errorf("after the retention: %d entries", len(entries))
	}
	s.cleanup()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM feed`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v", n, err)
	}
}

func TestPins(t *testing.T) {
	s, c := newStore(t)
	add := func(account string, path string) {
		t.Helper()
		if _, err := s.AddPin(account, path, "alice"); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Second)
	}
	add(Family, "guide/setup.md")
	add(Family, "index.md")
	add("bob", "guide/sub/deep.md")
	add("carol", "guide/setup.md")
	if added, _ := s.AddPin(Family, "index.md", "bob"); added {
		t.Error("pinned twice")
	}

	family, personal, err := s.Pins("bob")
	if err != nil {
		t.Fatal(err)
	}
	// new pins go to the end
	if len(family) != 2 || family[0].Path != "guide/setup.md" || len(personal) != 1 || personal[0].Path != "guide/sub/deep.md" {
		t.Fatalf("family %+v personal %+v", family, personal)
	}

	changes, err := s.MovePins("guide", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if !changes.Family || !reflect.DeepEqual(changes.Accounts, []string{"bob", "carol"}) && !reflect.DeepEqual(changes.Accounts, []string{"carol", "bob"}) {
		t.Errorf("changes = %+v", changes)
	}
	_, personal, _ = s.Pins("bob")
	if personal[0].Path != "manual/sub/deep.md" {
		t.Errorf("personal = %+v", personal)
	}
	// guidebook is not below guide
	add(Family, "guidebook.md")
	if changes, _ := s.DeletePins("guide"); !changes.Empty() {
		t.Errorf("deleted %+v", changes)
	}
	changes, _ = s.DeletePins("manual")
	family, _, _ = s.Pins("bob")
	if !changes.Family || len(family) != 2 {
		t.Errorf("changes %+v family %+v", changes, family)
	}
	if removed, _ := s.RemovePin(Family, "index.md"); !removed {
		t.Error("not removed")
	}
	if removed, _ := s.RemovePin(Family, "index.md"); removed {
		t.Error("removed twice")
	}
}

func TestReorderPins(t *testing.T) {
	s, c := newStore(t)
	for _, p := range []string{"a.md", "b.md", "c.md", "d.md"} {
		if _, err := s.AddPin(Family, p, "alice"); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Second)
	}
	order := func() string {
		family, _, err := s.Pins("bob")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range family {
			out = append(out, p.Path)
		}
		return strings.Join(out, ",")
	}
	// unknown paths are ignored, the missing pins follow in their order
	changed, err := s.ReorderPins(Family, []string{"c.md", "nope.md", "a.md", "c.md"})
	if err != nil || !changed || order() != "c.md,a.md,b.md,d.md" {
		t.Fatalf("changed %v, %v, order %s", changed, err, order())
	}
	if changed, _ := s.ReorderPins(Family, []string{"c.md"}); changed {
		t.Error("the same order changed")
	}
	if _, err := s.AddPin(Family, "e.md", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MovePins("a.md", "z.md"); err != nil {
		t.Fatal(err)
	}
	if order() != "c.md,z.md,b.md,d.md,e.md" {
		t.Errorf("order %s", order())
	}
	// personal pins are separate
	if changed, _ := s.ReorderPins("bob", []string{"c.md"}); changed {
		t.Error("bob has no pins")
	}
}

// The pins of a database from before the order keep the order they were listed in, newest first
func TestPinOrderMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err = db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`PRAGMA user_version = 2;
		INSERT INTO pins (account_id, path, pinned_by, pinned_at) VALUES ('', 'old.md', 'a', 1), ('', 'new.md', 'a', 3), ('', 'mid.md', 'a', 2), ('bob', 'x.md', 'b', 5)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	family, personal, err := s.Pins("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(family) != 3 || family[0].Path != "new.md" || family[1].Path != "mid.md" || family[2].Path != "old.md" || len(personal) != 1 {
		t.Errorf("family %+v personal %+v", family, personal)
	}
}

func TestRangesMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:3] {
		if _, err = db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`PRAGMA user_version = 3;
		INSERT INTO feed (time, kind, item_type, path, authors, lines_added, headings) VALUES (?, 'edited', 'document', 'a.md', '["alice"]', 1, '["H"]')`,
		time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	entries := feed(t, s)
	if len(entries) != 1 || entries[0].Summary.Ranges != nil || entries[0].Summary.TextHash != "" || entries[0].Summary.Headings[0] != "H" {
		t.Errorf("entries = %+v %+v", entries, entries[0].Summary)
	}
	s.RecordEdit("b.md", "x\n", "y\n", []string{"alice"})
	if e := feed(t, s)[0]; e.Path != "b.md" || len(e.Summary.Ranges) != 1 || e.Summary.TextHash != TextHash("y\n") {
		t.Errorf("new entry = %+v", e.Summary)
	}
}
