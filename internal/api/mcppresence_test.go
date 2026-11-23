package api

import (
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
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
