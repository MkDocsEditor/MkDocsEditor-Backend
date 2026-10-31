package backend

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
	e := newGitEnv(t, GitCommitterOptions{})

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
