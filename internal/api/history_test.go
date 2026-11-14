package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
)

func (e *syncEnv) history(id string) []HistoryEntry {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/document/"+id+"/history/", "carol")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
	var entries []HistoryEntry
	if err := json.Unmarshal(rec.bytes(), &entries); err != nil {
		e.t.Fatal(err)
	}
	return entries
}

func TestHistory(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})

	a := newTestClient(t, e, "alice")
	a.connect(e.docID("guide/setup.md"))
	a.appendText("by alice\n")
	b := newTestClient(t, e, "bob")
	b.connect(e.docID("guide/setup.md"))
	b.waitFor("a's edit", func() bool { return strings.HasSuffix(b.textLocked(), "by alice\n") })
	a.appendText("more by alice\n")
	b.waitFor("a's edit", func() bool { return strings.HasSuffix(b.textLocked(), "more by alice\n") })
	b.appendText("by bob\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == b.text() })
	a.disconnect()
	b.disconnect()
	e.restart() // commits

	if rec := e.doBody(http.MethodPut, "/document/"+e.docID("guide/setup.md")+"/", "bob", `{"name":"install"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}
	id := e.docID("guide/install.md")
	b = newTestClient(t, e, "bob")
	b.connect(id)
	b.appendText("after rename\n")
	eventually(t, "server", func() bool { return e.serverText("guide/install.md") == b.text() })

	entries := e.history(id)
	var got []string
	for _, h := range entries {
		line := h.Message + "|" + h.Author.Username + "|" + h.Path
		for _, co := range h.CoAuthors {
			line += "|co:" + co.Username
		}
		got = append(got, line)
	}
	want := []string{
		"Rename guide/setup.md to guide/install.md|bob|guide/install.md",
		"Update guide/setup.md|alice|guide/setup.md|co:bob",
		"Initial||guide/setup.md",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if entries[1].Author.Email != "alice@mkdocseditor.invalid" || entries[1].Time.IsZero() || len(entries[1].Revision) != 40 {
		t.Errorf("entry %+v", entries[1])
	}
	// authors look like the feed's, every entry says what it changed
	alice := e.authn.UserByUsername("alice")
	if au := entries[1].Author; au.DisplayName != alice.DisplayName || au.Color != alice.Color || au.DisplayName == "" {
		t.Errorf("author %+v", au)
	}
	if s := entries[1].Summary; s.LinesAdded != 3 || s.LinesRemoved != 0 || len(s.Headings) != 1 || s.Headings[0] != "Setup" {
		t.Errorf("summary of the edit %+v", s)
	}
	if s := entries[0].Summary; s.LinesAdded != 0 || s.LinesRemoved != 0 {
		t.Errorf("summary of the rename %+v", s)
	}
	if s := entries[2].Summary; s.LinesAdded != strings.Count(setupText, "\n") || s.LinesRemoved != 0 {
		t.Errorf("summary of the first commit %+v", s)
	}
	if again := e.history(id); again[1].Summary.LinesAdded != 3 {
		t.Errorf("cached summary %+v", again[1].Summary)
	}

	// structured diff
	rec0 := e.do(http.MethodGet, "/document/"+id+"/history/diff/?from="+entries[2].Revision+"&to="+entries[1].Revision+"&format=json", "carol")
	var sd HistoryDiffResponse
	if err := json.Unmarshal(rec0.bytes(), &sd); err != nil || rec0.Code != http.StatusOK {
		t.Fatalf("json diff: %d %s", rec0.Code, rec0.Body.String())
	}
	if len(sd.Hunks) != 1 || sd.From != entries[2].Revision || sd.To != entries[1].Revision || sd.Hunks[0].Heading == nil || *sd.Hunks[0].Heading != "Setup" {
		t.Fatalf("json diff: %s", rec0.Body.String())
	}
	var added []string
	for _, l := range sd.Hunks[0].Lines {
		if l.Op == "+" {
			added = append(added, l.Text)
		}
	}
	if strings.Join(added, "|") != "by alice|more by alice|by bob" {
		t.Errorf("added lines %v", added)
	}

	// content at a revision, also under the old path and with a short revision
	edited := setupText + "by alice\nmore by alice\nby bob\n"
	rec := e.do(http.MethodGet, "/document/"+id+"/history/"+entries[1].Revision[:10]+"/content/", "carol")
	if rec.Code != http.StatusOK || rec.Body.String() != edited {
		t.Errorf("content: %d %q", rec.Code, rec.Body.String())
	}
	for _, rev := range []string{"0000000000", "--output=x", "current"} {
		if rec = e.do(http.MethodGet, "/document/"+id+"/history/"+rev+"/content/", "carol"); rec.Code != http.StatusNotFound {
			t.Errorf("content of %q: %d", rev, rec.Code)
		}
	}

	// diff against the live text
	rec = e.do(http.MethodGet, "/document/"+id+"/history/diff/?from="+entries[1].Revision, "carol")
	if rec.Code != http.StatusOK {
		t.Fatalf("diff: %d %s", rec.Code, rec.Body.String())
	}
	diff := rec.Body.String()
	if !strings.HasPrefix(diff, "--- a/guide/setup.md ("+entries[1].Revision[:12]+")\n+++ b/guide/install.md (current)\n@@") ||
		!strings.Contains(diff, "\n+after rename\n") {
		t.Errorf("diff:\n%s", diff)
	}
	rec = e.do(http.MethodGet, "/document/"+id+"/history/diff/?from="+entries[2].Revision+"&to="+entries[1].Revision, "carol")
	if !strings.Contains(rec.Body.String(), "\n+by bob\n") || strings.Contains(rec.Body.String(), "after rename") {
		t.Errorf("diff between revisions:\n%s", rec.Body.String())
	}

	// restore: readers may not, editors may; connected clients get it as a normal change
	if rec = e.do(http.MethodPost, "/document/"+id+"/history/"+entries[2].Revision+"/restore/", "carol"); rec.Code != http.StatusForbidden {
		t.Errorf("reader restore: %d", rec.Code)
	}
	if rec = e.do(http.MethodPost, "/document/"+id+"/history/"+entries[2].Revision+"/restore/", "bob"); rec.Code != http.StatusNoContent {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	b.waitFor("restored text", func() bool { return b.textLocked() == setupText })
	if e.readFile("guide/install.md") != setupText {
		t.Error("restore not written before the commit")
	}
	h := e.history(id)
	if h[0].Message != "Restore guide/install.md to "+entries[2].Revision[:12] || h[0].Author.Username != "bob" {
		t.Errorf("restore commit %+v", h[0])
	}
	// the edit made before the restore went into the restore commit
	if rec = e.do(http.MethodGet, "/document/"+id+"/history/diff/?from="+h[0].Revision, "carol"); rec.Body.String() != "" {
		t.Errorf("uncommitted after restore:\n%s", rec.Body.String())
	}
}

func TestHistoryRequiresGit(t *testing.T) {
	e := newSyncEnv(t)
	rec := e.do(http.MethodGet, "/document/"+e.docID("guide/setup.md")+"/history/", "carol")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "git.enabled") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

// new pages start empty, so git sees each one as a copy of the previous one; the history must
// not follow copies
func TestHistoryIgnoresCopiesOfEmptyPages(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	for _, name := range []string{"first", "second", "third"} {
		if rec := e.doBody(http.MethodPost, "/document/", "bob", `{"parent":"root","name":"`+name+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("create %s: %d", name, rec.Code)
		}
	}
	id := e.docID("third.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.appendText("text\n")
	eventually(t, "server", func() bool { return e.serverText("third.md") == a.text() })
	a.disconnect()
	e.restart()
	if rec := e.doBody(http.MethodPut, "/document/"+id+"/", "bob", `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}

	var got []string
	for _, h := range e.history(e.docID("renamed.md")) {
		got = append(got, h.Message+"|"+h.Path)
	}
	want := []string{"Rename third.md to renamed.md|renamed.md", "Update third.md|third.md", "Create third.md|third.md"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if h := e.history(e.docID("second.md")); len(h) != 1 || h[0].Message != "Create second.md" {
		t.Errorf("history of second.md: %+v", h)
	}
}

// A page deleted and created again at the same path: its history starts at the new creation,
// and nothing reads the file at a revision without it
func TestHistoryRecreatedFile(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	rel := "guide/setup.md"
	id := e.docID(rel)
	e.appendCommitted(id, rel, "bob", "old line\n")
	e.stop()
	e.git0("rm", "-q", "docs/"+rel)
	e.git0("commit", "-q", "-m", "Delete "+rel)
	e.writeFile(rel, "# New\n")
	e.git0("add", "docs/"+rel)
	e.git0("commit", "-q", "-m", "Create "+rel)
	e.start()
	e.appendCommitted(id, rel, "alice", "new line\n")

	h := e.history(id)
	if len(h) != 2 || h[0].Author.Username != "alice" || h[1].Message != "Create "+rel {
		t.Fatalf("history %+v", h)
	}
	if s := h[1].Summary; s.LinesAdded != 1 || s.LinesRemoved != 0 {
		t.Errorf("summary of the creation %+v", s)
	}
	if s := h[0].Summary; s.LinesAdded != 1 || s.LinesRemoved != 0 {
		t.Errorf("summary of the edit %+v", s)
	}
	created, edited := h[1].Revision, h[0].Revision
	if rec := e.do(http.MethodGet, "/document/"+id+"/history/"+created+"/content/", "carol"); rec.Code != http.StatusOK || rec.Body.String() != "# New\n" {
		t.Errorf("content %d %q", rec.Code, rec.Body.String())
	}
	// revisions of the deleted page are not in this history
	old := strings.TrimSpace(e.git0("rev-list", "-1", "HEAD~3"))
	for _, path := range []string{"/history/" + old + "/content/", "/history/diff/?from=" + old, "/history/" + old + "/revert/"} {
		if rec := e.do(http.MethodGet, "/document/"+id+path, "carol"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(http.MethodGet, "/document/"+id+"/history/diff/?from="+created, "carol"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "+new line") {
		t.Errorf("diff %d %s", rec.Code, rec.Body.String())
	}
	// the creation has no text before it
	if rec := e.do(http.MethodGet, "/document/"+id+"/history/"+created+"/revert/", "carol"); rec.Code != http.StatusBadRequest {
		t.Errorf("check of the creation: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodGet, "/document/"+id+"/history/"+edited+"/revert/", "carol"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"possible": true`) {
		t.Errorf("check of the edit: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do(http.MethodGet, "/document/"+id+"/blame/", "carol")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), old) {
		t.Errorf("blame %d %s", rec.Code, rec.Body.String())
	}
}
