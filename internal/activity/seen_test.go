package activity

import (
	"slices"
	"testing"
	"time"
)

func TestSeen(t *testing.T) {
	s, c := newStore(t)
	if seen, err := s.LastSeen("alice", "a/x.md"); err != nil || seen != nil {
		t.Fatalf("never seen: %v %v", seen, err)
	}
	for _, p := range []string{"a/x.md", "a/b/y.md", "a_x.md", "other.md"} {
		if _, err := s.SetSeen("alice", p, "tok", "text of "+p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetSeen("bob", "c/x.md", "old", "bob saw c"); err != nil {
		t.Fatal(err)
	}
	// moving a section moves what was seen below it, not lookalikes
	if err := s.MoveSeen("a", "c"); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"c/x.md": "text of a/x.md", "c/b/y.md": "text of a/b/y.md", "a_x.md": "text of a_x.md"} {
		if seen, _ := s.LastSeen("alice", p); seen == nil || seen.Text != want {
			t.Errorf("%s: %+v", p, seen)
		}
	}
	if seen, _ := s.LastSeen("bob", "c/x.md"); seen == nil || seen.Text != "bob saw c" {
		t.Errorf("bob's own entry at the new path: %+v", seen)
	}
	if err := s.DeleteSeen("c"); err != nil {
		t.Fatal(err)
	}
	if seen, _ := s.LastSeen("alice", "c/b/y.md"); seen != nil {
		t.Error("seen survives the delete")
	}

	// authors since a time follow the document through moves
	start := c.now()
	c.advance(time.Minute)
	s.RecordEdit("old.md", "a\n", "b\n", []string{"alice"})
	s.Wait()
	c.advance(time.Hour)
	s.RecordItem(KindMoved, "document", "new.md", "old.md", "bob")
	s.RecordEdit("new.md", "b\n", "c\n", []string{"carol", "alice"})
	s.RecordEdit("unrelated.md", "b\n", "c\n", []string{"dave"})
	s.Wait()
	authors, err := s.EditAuthorsSince("new.md", start)
	if err != nil || !slices.Equal(authors, []string{"alice", "carol"}) {
		t.Errorf("authors %v %v", authors, err)
	}
	if authors, _ = s.EditAuthorsSince("new.md", c.now()); len(authors) != 0 {
		t.Errorf("authors after everything %v", authors)
	}
}
