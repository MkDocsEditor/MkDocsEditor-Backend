package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	automerge "github.com/automerge/automerge-go"
)

// appendCommitted appends a line as a user and commits it with a restart
func (e *syncEnv) appendCommitted(id, rel, user, line string) {
	e.t.Helper()
	c := newTestClient(e.t, e, user)
	c.connect(id)
	c.appendText(line)
	eventually(e.t, "server", func() bool { return e.serverText(rel) == c.text() })
	c.disconnect()
	e.restart()
}

func TestRevert(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	rel := "guide/setup.md"
	id := e.docID(rel)
	e.appendCommitted(id, rel, "bob", "line A\n")
	e.appendCommitted(id, rel, "alice", "line B\n")
	h := e.history(id)
	initial, commitA, commitB := h[2].Revision, h[1].Revision, h[0].Revision

	// an uncommitted edit stays and is committed before the revert
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.appendText("## Notes\nlive edit\n")
	eventually(t, "server", func() bool { return e.serverText(rel) == a.text() })

	revert := func(rev, user string) *httpRecorder {
		return e.do(http.MethodPost, "/document/"+id+"/history/"+rev+"/revert/", user)
	}
	if rec := revert(commitA, "carol"); rec.Code != http.StatusForbidden {
		t.Errorf("reader revert: %d", rec.Code)
	}
	if rec := revert(initial, "bob"); rec.Code != http.StatusBadRequest {
		t.Errorf("revert of the first revision: %d %s", rec.Code, rec.Body.String())
	}
	if rec := revert("0000000000", "bob"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown revision: %d", rec.Code)
	}

	// an agent reverts bob's commit: only line A goes, later and live edits stay
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	r := e.doToken(http.MethodPost, "/document/"+id+"/history/"+commitA[:10]+"/revert/", token, nil)
	expectCode(t, r, http.StatusOK, "")
	var entry HistoryEntry
	r.json(t, &entry)
	want := setupText + "line B\n## Notes\nlive edit\n"
	if entry.Message != "Revert "+commitA[:12]+" on "+rel || entry.Author.Username != "bob" ||
		len(entry.Agents) != 1 || entry.Agents[0].Name != "claude" ||
		entry.Summary.LinesRemoved != 1 || entry.Summary.LinesAdded != 0 {
		t.Errorf("entry %+v", entry)
	}
	a.waitFor("reverted text", func() bool { return a.textLocked() == want })
	if got := e.git0("show", "HEAD:docs/"+rel); got != want {
		t.Errorf("committed %q", got)
	}
	if body := e.git0("log", "-1", "--format=%b"); !strings.Contains(body, "This reverts commit "+commitA+".") {
		t.Errorf("body %q", body)
	}
	h = e.history(id)
	if h[1].Message != "Update "+rel || h[1].Author.Username != "alice" || len(h[1].Agents) != 0 {
		t.Errorf("the pending edit was not committed apart: %+v", h[1])
	}

	// twice: nothing left to revert
	rec := revert(commitA, "bob")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), CodeNothingToRevert) {
		t.Errorf("second revert: %d %s", rec.Code, rec.Body.String())
	}

	// a later edit of the same line conflicts and changes nothing
	a.insertText(len(setupText+"line B"), " changed")
	eventually(t, "server", func() bool { return strings.Contains(e.serverText(rel), "line B changed") })
	rec = revert(commitB, "bob")
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Code    string `json:"code"`
		Details struct {
			Headings  []string         `json:"headings"`
			Conflicts []RevertConflict `json:"conflicts"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Code != CodeRevertConflict || strings.Join(res.Details.Headings, ",") != "Setup" ||
		len(res.Details.Conflicts) != 1 || res.Details.Conflicts[0].Line != 4 || res.Details.Conflicts[0].HeadingOccurrence != 1 {
		t.Errorf("conflict %s", rec.Body.String())
	}
	if !strings.Contains(e.serverText(rel), "line B changed") {
		t.Error("the conflicting revert changed the text")
	}
}

func TestRestoreSection(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	rel := "guide/setup.md"
	id := e.docID(rel)
	e.appendCommitted(id, rel, "bob", "## A\na1\n### A.1\nsub\n## B\nb1\n")
	rev := e.history(id)[0].Revision

	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.edit(func(text *automerge.Text) error {
		s, _ := text.Get()
		for _, r := range [][2]string{{"a1", "a2"}, {"sub", "sub2"}, {"b1", "b2"}} {
			i := strings.Index(s, r[0])
			if err := text.Splice(i, len(r[0]), r[1]); err != nil {
				return err
			}
			s = s[:i] + r[1] + s[i+len(r[0]):]
		}
		return nil
	})
	eventually(t, "server", func() bool { return e.serverText(rel) == a.text() })

	restore := func(body string) *httpRecorder {
		return e.doBody(http.MethodPost, "/document/"+id+"/history/"+rev+"/restore/", "bob", body)
	}
	if rec := restore(`{"heading":"Nope"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), CodeSectionNotFound) {
		t.Errorf("missing section: %d %s", rec.Code, rec.Body.String())
	}
	if rec := restore(`{"heading":" "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty heading: %d", rec.Code)
	}
	// the section with its subsections, the rest stays
	if rec := restore(`{"heading":"A","headingOccurrence":1}`); rec.Code != http.StatusNoContent {
		t.Fatalf("restore section: %d %s", rec.Code, rec.Body.String())
	}
	want := setupText + "## A\na1\n### A.1\nsub\n## B\nb2\n"
	a.waitFor("restored section", func() bool { return a.textLocked() == want })
	h := e.history(id)
	if h[0].Message != `Restore section "A" of guide/setup.md to `+rev[:12] || h[0].Author.Username != "bob" {
		t.Errorf("commit %+v", h[0])
	}
	if h[1].Author.Username != "alice" {
		t.Errorf("pending edits not committed apart: %+v", h[1])
	}
	if got := e.git0("show", "HEAD:docs/"+rel); got != want {
		t.Errorf("committed %q", got)
	}
}
