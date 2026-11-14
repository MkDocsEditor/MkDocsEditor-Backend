package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
)

// appToken creates an app token of a user
func (e *syncEnv) appToken(user, name, scope string) string {
	e.t.Helper()
	_, token, err := e.authn.CreateAppToken(e.authn.UserByUsername(user), name, scope, 0, auth.RequestInfo{})
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

// tokenResponse is the answer of a request with an app token
type tokenResponse struct {
	httpRecorder
	header http.Header
}

func (r *tokenResponse) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, r.Body.String())
	}
}

func (r *tokenResponse) code(t *testing.T) string {
	t.Helper()
	var e ErrorResult
	r.json(t, &e)
	return e.Code
}

// doToken sends a request with an app token and a JSON body (nil for none)
func (e *syncEnv) doToken(method, path, token string, body any) *tokenResponse {
	e.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.http.URL+APIPrefix+path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := &tokenResponse{httpRecorder: httpRecorder{Code: resp.StatusCode}, header: resp.Header}
	_, _ = r.Body.ReadFrom(resp.Body)
	return r
}

// readContent returns the text and the revision of a document
func (e *syncEnv) readContent(id, token string) (string, string) {
	e.t.Helper()
	r := e.doToken(http.MethodGet, "/document/"+id+"/content/", token, nil)
	if r.Code != http.StatusOK {
		e.t.Fatalf("GET content = %d %s", r.Code, r.Body.String())
	}
	etag := r.header.Get("ETag")
	if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		e.t.Fatalf("ETag = %q", etag)
	}
	return r.Body.String(), strings.Trim(etag, `"`)
}

func expectCode(t *testing.T, r *tokenResponse, status int, code string) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status = %d, want %d: %s", r.Code, status, r.Body.String())
	}
	if code != "" && r.code(t) != code {
		t.Fatalf("code = %q, want %q: %s", r.code(t), code, r.Body.String())
	}
}

// A websocket client types at the top of the document while REST edits land at the bottom,
// based on a revision from before the typing: both survive, everywhere.
func TestContentEditMergesWithTyping(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	token := e.appToken("bob", "claude", auth.ScopeWrite)

	text, base := e.readContent(id, token)
	if text != setupText {
		t.Fatalf("text = %q", text)
	}

	b := newTestClient(t, e, "bob")
	b.connect(id)
	for i := range 5 {
		b.insertText(i, "x")
	}

	r := e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, map[string]any{
		"baseRevision": base,
		"edits":        []map[string]string{{"old": "Some text.", "new": "Other text."}, {"old": "Other", "new": "Changed"}},
	})
	expectCode(t, r, http.StatusOK, "")
	var rev RevisionResponse
	r.json(t, &rev)
	if rev.Revision == "" || r.header.Get("ETag") != `"`+rev.Revision+`"` {
		t.Fatalf("revision %q, ETag %q", rev.Revision, r.header.Get("ETag"))
	}
	b.insertText(0, "y")

	want := "yxxxxx# Setup\n\nChanged text.\n"
	b.waitFor("the REST edit", func() bool { return b.textLocked() == want })
	eventually(t, "server text", func() bool { return e.serverText("guide/setup.md") == want })
	eventually(t, "disk write", func() bool { return e.readFile("guide/setup.md") == want })

	// the actor of the edit is bob's, marked as an agent
	b.waitFor("agent actor", func() bool {
		for _, m := range b.messages {
			if m["type"] == collab.MsgActor && m["agent"] != nil {
				agent := m["agent"].(map[string]any)
				return m["username"] == "bob" && agent["name"] == "claude"
			}
		}
		return false
	})
	var actors []collab.ActorInfo
	_ = json.Unmarshal(e.do(http.MethodGet, "/actors/", "bob").bytes(), &actors)
	agents := 0
	for _, a := range actors {
		if a.Agent != nil {
			agents++
			if a.Username != "bob" || a.Agent.Name != "claude" {
				t.Errorf("agent actor %+v", a)
			}
		}
	}
	if agents != 1 {
		t.Errorf("%d agent actors, want 1", agents)
	}

	// edits without a base revision work on the current text; the agent keeps its actor
	r = e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, map[string]any{
		"edits": []map[string]string{{"old": "yxxxxx", "new": ""}},
	})
	expectCode(t, r, http.StatusOK, "")
	b.waitFor("second edit", func() bool { return b.textLocked() == "# Setup\n\nChanged text.\n" })
	_ = json.Unmarshal(e.do(http.MethodGet, "/actors/", "bob").bytes(), &actors)
	agents = 0
	for _, a := range actors {
		if a.Agent != nil {
			agents++
		}
	}
	if agents != 1 {
		t.Errorf("%d agent actors after the second edit, want 1", agents)
	}
}

func TestContentEditErrors(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	edits := func(pairs ...string) map[string]any {
		var list []map[string]string
		for i := 0; i < len(pairs); i += 2 {
			list = append(list, map[string]string{"old": pairs[i], "new": pairs[i+1]})
		}
		return map[string]any{"edits": list}
	}
	post := func(body any) *tokenResponse {
		return e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, body)
	}

	// the first edit is fine, the second does not match: nothing is applied
	r := post(edits("Some", "Any", "missing", "x"))
	expectCode(t, r, http.StatusConflict, CodeEditNotFound)
	var er ErrorResult
	r.json(t, &er)
	if er.Details["index"] != float64(1) {
		t.Errorf("details = %v", er.Details)
	}
	r = post(edits("e", "E"))
	expectCode(t, r, http.StatusConflict, CodeEditAmbiguous)
	// overlapping occurrences are ambiguous too
	e.writeFile("index.md", "aaa\n")
	expectCode(t, e.doToken(http.MethodPost, "/document/"+e.docID("index.md")+"/edits/", token, edits("aa", "b")), http.StatusConflict, CodeEditAmbiguous)
	expectCode(t, post(edits("", "x")), http.StatusBadRequest, CodeBadRequest)
	expectCode(t, post(map[string]any{"edits": []any{}}), http.StatusBadRequest, CodeBadRequest)
	if text, _ := e.readContent(id, token); text != setupText {
		t.Fatalf("text changed: %q", text)
	}

	// revisions
	expectCode(t, post(map[string]any{"edits": edits("Some", "Any")["edits"], "baseRevision": "nope"}), http.StatusBadRequest, CodeBadRequest)
	unknown := strings.Repeat("ab", 32)
	expectCode(t, post(map[string]any{"edits": edits("Some", "Any")["edits"], "baseRevision": unknown}), http.StatusConflict, CodeUnknownRevision)

	// PUT needs a base revision
	expectCode(t, e.doToken(http.MethodPut, "/document/"+id+"/content/", token, map[string]any{"content": "new"}), http.StatusPreconditionRequired, CodeBaseRevisionRequired)
	expectCode(t, e.doToken(http.MethodPut, "/document/"+id+"/content/", token, map[string]any{"baseRevision": unknown}), http.StatusBadRequest, CodeBadRequest)

	// read tokens and readers may not change content
	read := e.appToken("bob", "reader", auth.ScopeRead)
	expectCode(t, e.doToken(http.MethodPost, "/document/"+id+"/edits/", read, edits("Some", "Any")), http.StatusForbidden, CodeForbidden)
	carol := e.appToken("carol", "claude", auth.ScopeWrite)
	expectCode(t, e.doToken(http.MethodPost, "/document/"+id+"/edits/", carol, edits("Some", "Any")), http.StatusForbidden, CodeForbidden)
	if _, rev := e.readContent(id, read); rev == "" {
		t.Error("read token got no revision")
	}

	// unknown documents and too large requests
	expectCode(t, e.doToken(http.MethodPost, "/document/nope/edits/", token, edits("a", "b")), http.StatusNotFound, CodeNotFound)
	huge := strings.Repeat("x", maxContentRequestBytes)
	_, rev := e.readContent(id, token)
	expectCode(t, e.doToken(http.MethodPut, "/document/"+id+"/content/", token, map[string]any{"content": huge, "baseRevision": rev}), http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
}

// PUT content merges three-way: what a client typed after the base revision stays
func TestContentWriteMergesThreeWay(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	_, base := e.readContent(id, token)

	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.insertText(0, "Typed. ")
	eventually(t, "typing on the server", func() bool { return strings.HasPrefix(e.serverText("guide/setup.md"), "Typed. ") })

	r := e.doToken(http.MethodPut, "/document/"+id+"/content/", token, map[string]any{
		"content":      "# Setup\n\nSome text.\n\nA new paragraph.\n",
		"baseRevision": base,
	})
	expectCode(t, r, http.StatusOK, "")
	want := "Typed. # Setup\n\nSome text.\n\nA new paragraph.\n"
	a.waitFor("the write", func() bool { return a.textLocked() == want })
	text, rev := e.readContent(id, token)
	if text != want {
		t.Fatalf("text = %q", text)
	}

	// the same content again changes nothing
	r = e.doToken(http.MethodPut, "/document/"+id+"/content/", token, map[string]any{"content": want, "baseRevision": rev})
	expectCode(t, r, http.StatusOK, "")
	var same RevisionResponse
	r.json(t, &same)
	if same.Revision != rev {
		t.Errorf("revision changed without a change: %s -> %s", rev, same.Revision)
	}
}

// edits of a document nobody has open are written to disk and committed with the agent trailer
func TestContentEditCommit(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{AfterIdle: 300 * time.Millisecond})
	id := e.docID("guide/setup.md")
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	for _, edit := range []string{"One", "Two", "Three"} {
		r := e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, map[string]any{
			"edits": []map[string]string{{"old": "text.", "new": "text. " + edit + "."}},
		})
		expectCode(t, r, http.StatusOK, "")
	}
	eventually(t, "disk write", func() bool { return e.readFile("guide/setup.md") == "# Setup\n\nSome text. Three. Two. One.\n" })
	eventually(t, "commit", func() bool { return len(e.commits()) == 2 })
	msg := e.git0("log", "-1", "--format=%an%n%B")
	if !strings.HasPrefix(msg, "bob\nUpdate guide/setup.md") || !strings.Contains(msg, "Via-App-Token: claude (bob <bob@mkdocseditor.invalid>)") {
		t.Errorf("commit = %q", msg)
	}
	var history []HistoryEntry
	_ = json.Unmarshal(e.do(http.MethodGet, "/document/"+id+"/history/", "bob").bytes(), &history)
	if len(history) != 2 || len(history[0].AppTokens) != 1 || history[0].AppTokens[0] != "claude" {
		t.Errorf("history = %+v", history)
	}
}

func TestSearch(t *testing.T) {
	e := newSyncEnv(t)
	e.writeFile("index.md", "# Home\n\nWelcome to the wiki.\nThe SETUP guide is linked below.\n")
	e.tm.CreateItemTree()
	token := e.appToken("carol", "claude", auth.ScopeRead)

	r := e.doToken(http.MethodGet, "/search/?q=setup", token, nil)
	expectCode(t, r, http.StatusOK, "")
	var res SearchResponse
	r.json(t, &res)
	if res.Total != 2 || res.Results[0].Path != "guide/setup.md" || res.Results[1].Path != "index.md" {
		t.Fatalf("results = %s", r.Body.String())
	}
	if s := res.Results[1].Snippets; len(s) != 1 || s[0].Line != 4 || s[0].Text != "The SETUP guide is linked below." {
		t.Errorf("snippets = %+v", s)
	}
	if res.Results[0].ID != e.docID("guide/setup.md") || res.Results[0].Title == "" {
		t.Errorf("result = %+v", res.Results[0])
	}

	// every word must match
	r = e.doToken(http.MethodGet, "/search/?q=setup+welcome", token, nil)
	r.json(t, &res)
	if res.Total != 1 || res.Results[0].Path != "index.md" {
		t.Errorf("results = %s", r.Body.String())
	}
	r = e.doToken(http.MethodGet, "/search/?q=setup&limit=1", token, nil)
	r.json(t, &res)
	if res.Total != 2 || len(res.Results) != 1 {
		t.Errorf("limited results = %s", r.Body.String())
	}
	expectCode(t, e.doToken(http.MethodGet, "/search/?q=", token, nil), http.StatusBadRequest, CodeBadRequest)
	expectCode(t, e.doToken(http.MethodGet, "/search/?q=a&limit=0", token, nil), http.StatusBadRequest, CodeBadRequest)

	// the live text counts, not only the file
	b := newTestClient(t, e, "bob")
	b.connect(e.docID("guide/setup.md"))
	b.appendText("Unflushed zebra.\n")
	eventually(t, "server text", func() bool { return strings.Contains(e.serverText("guide/setup.md"), "zebra") })
	r = e.doToken(http.MethodGet, "/search/?q=ZEBRA", token, nil)
	r.json(t, &res)
	if res.Total != 1 {
		t.Errorf("live text not searched: %s", r.Body.String())
	}
}

func TestSnippetAround(t *testing.T) {
	line := strings.Repeat("ä", 300) + "needle" + strings.Repeat("b", 300)
	s := around(line, strings.ToLower(line), strings.Index(line, "needle"))
	if !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || !strings.Contains(s, "needle") {
		t.Errorf("snippet = %q", s)
	}
}
