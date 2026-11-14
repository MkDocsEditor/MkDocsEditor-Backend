package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
)

func (e *syncEnv) changes(id, user, since string) ChangesResponse {
	e.t.Helper()
	path := "/document/" + id + "/changes/"
	if since != "" {
		path += "?since=" + since
	}
	r := e.session(http.MethodGet, path, user, nil)
	if r.Code != http.StatusOK {
		e.t.Fatalf("changes = %d %s", r.Code, r.Body.String())
	}
	var c ChangesResponse
	r.json(e.t, &c)
	return c
}

func (e *syncEnv) markSeen(id, user, token string) {
	e.t.Helper()
	if r := e.session(http.MethodPut, "/document/"+id+"/seen/", user, map[string]string{"seenToken": token}); r.Code != http.StatusOK {
		e.t.Fatalf("seen = %d %s", r.Code, r.Body.String())
	}
}

func addedLines(c ChangesResponse) []string {
	var out []string
	for _, h := range c.Hunks {
		for _, l := range h.Lines {
			if l.Op == "+" {
				out = append(out, l.Text)
			}
		}
	}
	return out
}

func TestChangesSinceSeen(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	id := e.docID("guide/setup.md")

	// never seen: no starting point, nothing to show
	c := e.changes(id, "carol", "")
	if c.Since != nil || len(c.Hunks) != 0 || c.SeenToken == "" {
		t.Fatalf("first changes %+v", c)
	}
	// readers mark pages as seen
	e.markSeen(id, "carol", c.SeenToken)
	if c = e.changes(id, "carol", "seen"); c.Since == nil || len(c.Hunks) != 0 || len(c.Authors) != 0 {
		t.Fatalf("after seen %+v", c)
	}
	seenToken := c.SeenToken

	// alice types, bob's agent edits over REST
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.appendText("by alice\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	token := e.appToken("bob", "Claude Code", auth.ScopeWrite)
	expectCode(t, e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, map[string]any{"edits": []map[string]string{{"old": "Some text.", "new": "Some agent text."}}}), http.StatusOK, "")
	a.waitFor("agent edit", func() bool { return strings.Contains(a.textLocked(), "agent text") })

	eventually(t, "both authors", func() bool { return len(e.changes(id, "carol", "").Authors) == 2 })
	c = e.changes(id, "carol", "")
	if got := addedLines(c); !slices.Contains(got, "by alice") || !slices.Contains(got, "Some agent text.") {
		t.Errorf("added %v", got)
	}
	if c.Since.SeenToken != seenToken || c.SeenToken == seenToken || c.Summary.LinesAdded != 2 || c.Summary.LinesRemoved != 1 {
		t.Errorf("changes %+v", c)
	}
	byName := map[string]ChangeAuthor{}
	for _, au := range c.Authors {
		byName[au.Username] = au
	}
	if au := byName["alice"]; au.DisplayName == "" || au.Color == "" || len(au.Agents) != 0 {
		t.Errorf("alice %+v", au)
	}
	if au := byName["bob"]; len(au.Agents) != 1 || au.Agents[0] != "Claude Code" {
		t.Errorf("bob %+v", au)
	}

	// what was shown counts as seen, not an edit that landed meanwhile
	shown := c.SeenToken
	a.appendText("while reading\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	e.markSeen(id, "carol", shown)
	if got := addedLines(e.changes(id, "carol", "")); len(got) != 1 || got[0] != "while reading" {
		t.Errorf("after seen: added %v", got)
	}
	// only who changed something after the seen mark, though alice's edits share the feed's
	// edit session with the agent's
	e.markSeen(id, "carol", e.changes(id, "carol", "").SeenToken)
	expectCode(t, e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, map[string]any{"edits": []map[string]string{{"old": "Some agent text.", "new": "Some better agent text."}}}), http.StatusOK, "")
	// the feed has the edit session of both
	eventually(t, "file written", func() bool { return strings.Contains(e.readFile("guide/setup.md"), "better agent text") })
	e.ss.Activity().Wait()
	c = e.changes(id, "carol", "")
	if len(c.Authors) != 1 || c.Authors[0].Username != "bob" || len(c.Authors[0].Agents) != 1 || c.Authors[0].Agents[0] != "Claude Code" {
		t.Errorf("authors after the agent edit %+v", c.Authors)
	}
	// another account has its own starting point
	if c := e.changes(id, "bob", ""); c.Since != nil {
		t.Errorf("bob never saw it: %+v", c.Since)
	}
	// an explicit starting point
	if got := addedLines(e.changes(id, "bob", seenToken)); len(got) != 3 {
		t.Errorf("since token: added %v", got)
	}
	expectCode(t, e.session(http.MethodGet, "/document/"+id+"/changes/?since=nonsense", "bob", nil), http.StatusBadRequest, CodeBadRequest)
	expectCode(t, e.session(http.MethodGet, "/document/"+id+"/changes/?since="+strings.Repeat("ab", 32), "bob", nil), http.StatusConflict, CodeUnknownRevision)
	expectCode(t, e.session(http.MethodPut, "/document/"+id+"/seen/", "bob", map[string]string{"seenToken": strings.Repeat("ab", 32)}), http.StatusConflict, CodeUnknownRevision)
	// read tokens may mark pages as seen
	if r := e.doToken(http.MethodPut, "/document/"+id+"/seen/", e.appToken("carol", "reader", auth.ScopeRead), map[string]string{"seenToken": shown}); r.Code != http.StatusOK {
		t.Errorf("read token seen = %d %s", r.Code, r.Body.String())
	}
	a.disconnect()

	// what was seen moves with the page
	e.markSeen(id, "carol", e.changes(id, "carol", "").SeenToken)
	if rec := e.doBody(http.MethodPut, "/document/"+id+"/", "bob", `{"name":"install"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}
	if c := e.changes(e.docID("guide/install.md"), "carol", ""); c.Since == nil || len(c.Hunks) != 0 {
		t.Errorf("after the move %+v", c)
	}
}
