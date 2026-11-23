package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
)

// agentPresence returns the presence of the agent a client sees, nil for none. Caller holds c.mu.
func agentPresence(c *testClient) map[string]any {
	for _, p := range c.presences {
		if p["agent"] != nil {
			return p
		}
	}
	return nil
}

// agentFocus returns the focus of the agent a client sees, nil for none. Caller holds c.mu.
func agentFocus(c *testClient) map[string]any {
	p := agentPresence(c)
	if p == nil {
		return nil
	}
	f, _ := p["agent"].(map[string]any)["focus"].(map[string]any)
	return f
}

func TestMCPPointAt(t *testing.T) {
	e := newSyncEnv(t)
	rel := "guide/setup.md"
	id := e.docID(rel)
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	doc := func(args map[string]any) map[string]any {
		args["document"] = rel
		return args
	}

	// nobody has the page open: nothing to point at, and the page is not loaded for it
	out, failed := callTool(t, s, "point_at", doc(map[string]any{"select": "Some text."}))
	if !failed || !strings.Contains(out, "nobody has this page open") {
		t.Fatalf("point_at without viewers = %q, %v", out, failed)
	}
	if out, _ = callTool(t, s, "read_document", doc(map[string]any{})); !strings.Contains(out, "\nviewers: none\n") {
		t.Errorf("read_document without viewers = %q", out)
	}

	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)
	if out, _ = callTool(t, s, "read_document", doc(map[string]any{})); !strings.Contains(out, "\nviewers: alice, bob (you)\n") {
		t.Errorf("read_document with viewers = %q", out)
	}

	// a selection by text: code points of the text at the revision
	out, failed = callTool(t, s, "point_at", doc(map[string]any{"select": "Some text."}))
	if failed || !strings.Contains(out, `"viewers":["alice","bob (you)"]`) {
		t.Fatalf("point_at select = %q, %v", out, failed)
	}
	a.waitFor("the focus", func() bool {
		f := agentFocus(a)
		return f != nil && f["from"] == float64(9) && f["to"] == float64(19) && f["revision"] != "" && f["viewport"] == nil
	})
	a.mu.Lock()
	p := agentPresence(a)
	if p["username"] != "bob" || p["deviceName"] != "claude" || len(p["selections"].([]any)) != 0 {
		t.Errorf("agent presence = %v", p)
	}
	a.mu.Unlock()

	// a heading: the cursor at its start, the viewport over its section
	if out, failed = callTool(t, s, "point_at", doc(map[string]any{"heading": "## Setup"})); failed {
		t.Fatalf("point_at heading = %q", out)
	}
	a.waitFor("the heading focus", func() bool {
		f := agentFocus(a)
		if f == nil || f["from"] != float64(0) || f["to"] != float64(0) {
			return false
		}
		v, _ := f["viewport"].(map[string]any)
		return v != nil && v["top"] == float64(0) && v["bottom"] == float64(len(setupText)-1)
	})

	// errors the agent can act on
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"select": "e"}, "more than once"},
		{map[string]any{"select": "nowhere"}, "does not occur"},
		{map[string]any{"heading": "Nope"}, "no such heading"},
		{map[string]any{"select": "Some", "heading": "Setup"}, "exactly one of"},
		{map[string]any{}, "exactly one of"},
		{map[string]any{"select": strings.Repeat("x", maxPointAtText+1)}, "4 KiB"},
	} {
		if out, failed = callTool(t, s, "point_at", doc(c.args)); !failed || !strings.Contains(out, c.want) {
			t.Errorf("point_at %v = %q, %v", c.args, out, failed)
		}
	}

	// someone who joins later finds the focus in the snapshot
	c := newTestClient(t, e, "carol")
	c.connect(id)
	c.waitFor("the focus in the snapshot", func() bool { f := agentFocus(c); return f != nil && f["viewport"] != nil })

	// an edit keeps the focus
	_, revision := e.readContent(id, e.appToken("bob", "other", auth.ScopeWrite))
	if out, failed = callTool(t, s, "edit_document", doc(map[string]any{"baseRevision": revision,
		"edits": []map[string]string{{"old": "Some text.", "new": "Agent text."}}})); failed || !strings.Contains(out, `"viewers":["alice","bob (you)","carol"]`) {
		t.Fatalf("edit_document = %q", out)
	}
	a.waitFor("the edit", func() bool { return strings.Contains(a.textLocked(), "Agent text.") })
	a.mu.Lock()
	if agentFocus(a) == nil {
		t.Error("the edit ended the focus")
	}
	a.mu.Unlock()

	// clear ends the presence at once, without a ghost
	if out, failed = callTool(t, s, "point_at", doc(map[string]any{"clear": true})); failed {
		t.Fatalf("point_at clear = %q", out)
	}
	a.waitFor("the agent to leave", func() bool {
		for _, l := range a.leaves {
			if strings.HasPrefix(l["clientId"].(string), "agent-") {
				return agentPresence(a) == nil && l["ghostMs"] == nil
			}
		}
		return false
	})
}

func TestMCPPointAtScopesAndTokens(t *testing.T) {
	e := newSyncEnv(t)
	rel := "guide/setup.md"
	a := newTestClient(t, e, "alice")
	a.connect(e.docID(rel))
	args := map[string]any{"document": rel, "select": "Some text."}

	// a read token may point
	s := e.mcpSession(e.appToken("bob", "reader", auth.ScopeRead))
	if out, failed := callTool(t, s, "point_at", args); failed {
		t.Fatalf("point_at with a read token = %q", out)
	}
	a.waitFor("the focus of the read token", func() bool { return agentFocus(a) != nil })

	// a session token has no agent presence
	s = e.mcpSession(strings.TrimPrefix(authHeader(t, e.authn, "bob", "bob-pw"), "Bearer "))
	if out, failed := callTool(t, s, "point_at", args); !failed || !strings.Contains(out, "needs an app token") {
		t.Errorf("point_at with a session token = %q, %v", out, failed)
	}
}

func TestAgentFocusLifetime(t *testing.T) {
	e := newSyncEnv(t)
	e.rs.sync.AgentPresenceLifetime = 50 * time.Millisecond
	e.rs.sync.AgentFocusLifetime = 600 * time.Millisecond
	rel := "guide/setup.md"
	a := newTestClient(t, e, "alice")
	a.connect(e.docID(rel))
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	start := time.Now()
	if out, failed := callTool(t, s, "point_at", map[string]any{"document": rel, "select": "Some text."}); failed {
		t.Fatalf("point_at = %q", out)
	}
	a.waitFor("the focus", func() bool { return agentFocus(a) != nil })
	a.waitFor("the agent to leave", func() bool { return len(a.leaves) > 0 })
	// the focus keeps the agent longer than an agent without one
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Errorf("the agent with a focus left after %v", d)
	}
}

// messageIndex is the index of the first message from index from on that matches, -1 for none.
// Caller holds c.mu.
func messageIndex(c *testClient, from int, match func(m map[string]any) bool) int {
	for i := from; i < len(c.messages); i++ {
		if match(c.messages[i]) {
			return i
		}
	}
	return -1
}

func TestMCPLiveEdit(t *testing.T) {
	e := newSyncEnv(t)
	rel := "guide/setup.md"
	id := e.docID(rel)
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	s := e.mcpSession(token)
	a := newTestClient(t, e, "alice")
	a.connect(id)
	_, base := e.readContent(id, token)

	// a person types at the start meanwhile, the agent edits the older revision
	a.insertText(0, "Hello\n")
	eventually(t, "server", func() bool { return e.serverText(rel) == "Hello\n"+setupText })
	a.mu.Lock()
	start := len(a.messages)
	a.mu.Unlock()
	out, failed := callTool(t, s, "edit_document", map[string]any{"document": rel, "baseRevision": base, "live": true,
		"edits": []map[string]string{{"old": "Some text.", "new": "Agent text."}}})
	if failed {
		t.Fatalf("edit_document = %q", out)
	}
	a.waitFor("the edit", func() bool { return strings.Contains(a.textLocked(), "Agent text.") })
	a.waitFor("agent-change", func() bool {
		return messageIndex(a, start, func(m map[string]any) bool { return m["type"] == collab.MsgAgentChange }) >= 0
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	change := a.messages[messageIndex(a, start, func(m map[string]any) bool { return m["type"] == collab.MsgAgentChange })]
	hash := change["change"].(string)
	if change["base"] != base || len(hash) != 64 || change["agent"].(map[string]any)["name"] != "claude" ||
		!strings.HasPrefix(change["clientId"].(string), "agent-") {
		t.Errorf("agent-change = %v, base %s", change, base)
	}
	// the focus is at the end of the inserted text, at the revision of the change itself, not of
	// the merged document
	if strings.Contains(out, `"revision":"`+hash+`"`) {
		t.Errorf("the merged revision is the change alone: %s", out)
	}
	presence := messageIndex(a, start, func(m map[string]any) bool {
		agent, _ := m["agent"].(map[string]any)
		focus, _ := agent["focus"].(map[string]any)
		return m["type"] == collab.MsgPresence && focus != nil && focus["revision"] == hash &&
			focus["from"] == float64(20) && focus["to"] == float64(20)
	})
	announced := messageIndex(a, start, func(m map[string]any) bool { return m["type"] == collab.MsgAgentChange })
	synced := messageIndex(a, start, func(m map[string]any) bool { return m["type"] == collab.MsgSyncRequest })
	if presence < 0 || presence >= announced || announced >= synced {
		t.Errorf("order: presence %d, agent-change %d, sync %d", presence, announced, synced)
	}
}

func TestInstantEditSetsTheFocus(t *testing.T) {
	e := newSyncEnv(t)
	rel := "guide/setup.md"
	id := e.docID(rel)
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	a := newTestClient(t, e, "alice")
	a.connect(id)

	// over REST with an app token: a focus, no agent-change
	text, revision := e.readContent(id, token)
	r := e.doToken(http.MethodPut, "/document/"+id+"/content/", token, map[string]any{"content": text + "More.\n", "baseRevision": revision})
	if r.Code != http.StatusOK {
		t.Fatalf("PUT content = %d %s", r.Code, r.Body.String())
	}
	a.waitFor("the focus at the end", func() bool {
		f := agentFocus(a)
		return f != nil && f["from"] == float64(len(setupText)+len("More.\n"))
	})
	a.mu.Lock()
	if messageIndex(a, 0, func(m map[string]any) bool { return m["type"] == collab.MsgAgentChange }) >= 0 {
		t.Error("agent-change for an instant edit")
	}
	a.mu.Unlock()

	// live needs an app token
	s := e.mcpSession(strings.TrimPrefix(authHeader(t, e.authn, "bob", "bob-pw"), "Bearer "))
	if out, failed := callTool(t, s, "edit_document", map[string]any{"document": rel, "live": true,
		"edits": []map[string]string{{"old": "More.", "new": "Less."}}}); !failed || !strings.Contains(out, "needs an app token") {
		t.Errorf("live with a session token = %q, %v", out, failed)
	}
}

// TestMCPLiveListed checks that every kind of caller sees live and its hint in tools/list: a
// session token, an app token and an OAuth grant. A session token sees live too and gets an error
// when it uses it.
func TestMCPLiveListed(t *testing.T) {
	e := newSyncEnv(t)
	f := &oauthFlow{e: e, clientID: e.registerClient("http://127.0.0.1/callback"), redirectURI: "http://127.0.0.1:43123/callback",
		verifier: strings.Repeat("v", 50)}
	grant := tokensOf(t, f.exchange(f.approve("bob", f.authorize(url.Values{"resource": {e.http.URL + "/api/mcp"}}), nil), f.verifier))
	for name, token := range map[string]string{
		"session": strings.TrimPrefix(authHeader(t, e.authn, "bob", "bob-pw"), "Bearer "),
		"app":     e.appToken("bob", "claude", auth.ScopeWrite),
		"oauth":   grant.AccessToken,
	} {
		res, err := e.mcpSession(token).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		listed := 0
		for _, tool := range res.Tools {
			if tool.Name != "edit_document" && tool.Name != "write_document" {
				continue
			}
			listed++
			schema, _ := json.Marshal(tool.InputSchema)
			var s struct {
				Properties map[string]struct {
					Type any `json:"type"`
				} `json:"properties"`
			}
			if err = json.Unmarshal(schema, &s); err != nil {
				t.Fatal(err)
			}
			if s.Properties["live"].Type != "boolean" {
				t.Errorf("%s: %s has no live: %s", name, tool.Name, schema)
			}
			if !strings.Contains(tool.Description, mcpLiveHint) {
				t.Errorf("%s: %s has no hint: %q", name, tool.Name, tool.Description)
			}
		}
		if listed != 2 {
			t.Errorf("%s: %d write tools listed", name, listed)
		}
	}
}
