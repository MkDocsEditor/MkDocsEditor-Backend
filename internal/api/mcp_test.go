package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerTransport adds an Authorization header to every request
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// mcpSession connects an MCP client with an app token
func (e *syncEnv) mcpSession(token string) *mcp.ClientSession {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             e.http.URL + APIPrefix + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = session.Close() })
	return session
}

// callTool calls a tool and returns its text and whether it failed
func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return strings.Join(out, "\n"), res.IsError
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestMCPTools(t *testing.T) {
	e := newSyncEnv(t)
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	s := e.mcpSession(token)

	want := []string{"create_document", "edit_document", "get_backlinks", "list_tree", "move_document", "read_document", "search", "write_document"}
	if names := toolNames(t, s); !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}

	out, failed := callTool(t, s, "list_tree", nil)
	if failed || !strings.Contains(out, "guide/setup.md\tSetup") || !strings.Contains(out, "index.md\t") {
		t.Fatalf("list_tree = %q", out)
	}
	out, _ = callTool(t, s, "search", map[string]any{"query": "some text"})
	if !strings.Contains(out, `"path":"guide/setup.md"`) {
		t.Errorf("search = %q", out)
	}

	// read by path without .md, edit, see the change on the server
	out, failed = callTool(t, s, "read_document", map[string]any{"document": "guide/setup"})
	if failed || !strings.HasSuffix(out, "---\n"+setupText) || !strings.Contains(out, "path: guide/setup.md\n") {
		t.Fatalf("read_document = %q", out)
	}
	revision := strings.TrimPrefix(strings.Split(out, "\n")[2], "revision: ")

	// a websocket client sees the agent as a peer
	id := e.docID("guide/setup.md")
	b := newTestClient(t, e, "alice")
	b.connect(id)

	out, failed = callTool(t, s, "edit_document", map[string]any{
		"document": "guide/setup.md", "baseRevision": revision,
		"edits": []map[string]string{{"old": "Some text.", "new": "Agent text."}},
	})
	if failed || !strings.Contains(out, `"revision"`) {
		t.Fatalf("edit_document = %q", out)
	}
	b.waitFor("the edit", func() bool { return strings.Contains(b.textLocked(), "Agent text.") })
	b.waitFor("agent presence", func() bool {
		for _, m := range b.messages {
			if m["type"] == "presence" && m["agent"] != nil {
				agent := m["agent"].(map[string]any)
				return agent["name"] == "claude" && m["username"] == "bob" && m["deviceName"] == "claude"
			}
		}
		return false
	})

	// edit errors are tool errors the agent can act on
	out, failed = callTool(t, s, "edit_document", map[string]any{
		"document": "guide/setup.md", "edits": []map[string]string{{"old": "t", "new": "T"}},
	})
	if !failed || !strings.Contains(out, "more than once") {
		t.Errorf("ambiguous edit = %q, %v", out, failed)
	}
	out, failed = callTool(t, s, "read_document", map[string]any{"document": "nope.md"})
	if !failed || !strings.Contains(out, "no page") {
		t.Errorf("unknown page = %q", out)
	}
	if _, failed = callTool(t, s, "write_document", map[string]any{"document": "guide/setup.md", "content": "x"}); !failed {
		t.Error("write_document without baseRevision succeeded")
	}

	// create with content, link to it, move it: the link follows
	out, failed = callTool(t, s, "create_document", map[string]any{"section": "guide", "name": "notes", "content": "# Notes\n"})
	if failed {
		t.Fatalf("create_document = %q", out)
	}
	var created mcpCreated
	_ = json.Unmarshal([]byte(out), &created)
	if created.Path != "guide/notes.md" || e.serverText("guide/notes.md") != "# Notes\n" {
		t.Fatalf("created = %q, text %q", out, e.serverText("guide/notes.md"))
	}
	_, revision = e.readContent(id, token)
	if _, failed = callTool(t, s, "write_document", map[string]any{"document": id, "baseRevision": revision,
		"content": "# Setup\n\nAgent text. See [notes](notes.md).\n"}); failed {
		t.Fatal("write_document failed")
	}
	eventually(t, "reference index", func() bool {
		out, _ = callTool(t, s, "get_backlinks", map[string]any{"document": "guide/notes.md"})
		return strings.Contains(out, `"path":"guide/setup.md"`)
	})
	out, failed = callTool(t, s, "move_document", map[string]any{"document": "guide/notes.md", "section": "/", "name": "journal"})
	if failed || !strings.Contains(out, `"newPath":"journal.md"`) {
		t.Fatalf("move_document = %q", out)
	}
	eventually(t, "updated link", func() bool { return strings.Contains(e.serverText("guide/setup.md"), "(../journal.md)") })
}

func TestMCPReadScopeAndAuth(t *testing.T) {
	e := newSyncEnv(t)
	s := e.mcpSession(e.appToken("bob", "reader", auth.ScopeRead))
	if names := toolNames(t, s); !slices.Equal(names, []string{"get_backlinks", "list_tree", "read_document", "search"}) {
		t.Errorf("read scope tools = %v", names)
	}
	// readers get the read tools only, whatever the token scope
	s = e.mcpSession(e.appToken("carol", "claude", auth.ScopeWrite))
	if names := toolNames(t, s); slices.Contains(names, "edit_document") {
		t.Errorf("reader tools = %v", names)
	}

	// without credentials: 401; GET is not supported without sessions: 405, not 404
	resp, err := http.Post(e.http.URL+APIPrefix+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST = %d", resp.StatusCode)
	}
	r := e.do(http.MethodGet, "/mcp", "bob")
	if r.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d %s", r.Code, r.Body.String())
	}
}
