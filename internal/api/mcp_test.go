package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
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

	want := []string{"changes_since", "create_document", "download_resource", "edit_document", "get_backlinks", "list_tree", "move_document", "read_document", "read_resource", "search", "write_document"}
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
	if names := toolNames(t, s); !slices.Equal(names, []string{"changes_since", "download_resource", "get_backlinks", "list_tree", "read_document", "read_resource", "search"}) {
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

func TestMCPHistory(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{AfterIdle: 300 * time.Millisecond})
	rel := "guide/setup.md"
	id := e.docID(rel)
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	if names := toolNames(t, s); !slices.Contains(names, "read_history") || !slices.Contains(names, "revert_change") || !slices.Contains(names, "read_revision") {
		t.Fatalf("tools = %v", names)
	}
	doc := map[string]any{"document": rel}
	call := func(name string, args map[string]any, wantFail bool) string {
		t.Helper()
		out, failed := callTool(t, s, name, args)
		if failed != wantFail {
			t.Fatalf("%s failed=%v: %s", name, failed, out)
		}
		return out
	}
	if out := call("changes_since", doc, false); !strings.Contains(out, "you have not read this page before") {
		t.Errorf("first changes_since = %q", out)
	}
	call("read_document", doc, false)

	// a person edits: the agent sees it once
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.appendText("by alice\n")
	eventually(t, "server", func() bool { return e.serverText(rel) == a.text() })
	if out := call("changes_since", doc, false); !strings.Contains(out, "changed by: alice\n+1 -0 lines") || !strings.Contains(out, "\n+by alice") {
		t.Errorf("changes_since = %q", out)
	}
	if out := call("changes_since", doc, false); !strings.HasSuffix(out, "no changes") {
		t.Errorf("changes_since again = %q", out)
	}
	eventually(t, "alice's commit", func() bool { return len(e.commits()) == 2 })

	// its own edit does not count as a change of others
	call("edit_document", map[string]any{"document": rel, "edits": []map[string]string{{"old": "Some text.", "new": "Agent text."}}}, false)
	if out := call("changes_since", doc, false); !strings.HasSuffix(out, "no changes") {
		t.Errorf("changes_since after the own edit = %q", out)
	}
	eventually(t, "agent commit", func() bool { return len(e.commits()) == 3 })
	h := e.history(id)
	agentRev, aliceRev := h[0].Revision, h[1].Revision

	out := call("read_history", doc, false)
	if !strings.Contains(out, agentRev[:12]+" ") || !strings.Contains(out, "by bob (agent claude): Update guide/setup.md, +1 -1 lines in Setup") ||
		!strings.Contains(out, "by alice: Update guide/setup.md, +1 -0 lines") {
		t.Errorf("read_history = %q", out)
	}
	if out := call("read_revision", map[string]any{"document": rel, "revision": aliceRev[:12]}, false); !strings.HasSuffix(out, "---\n"+setupText+"by alice\n") {
		t.Errorf("read_revision = %q", out)
	}

	// only its own commits
	if out := call("revert_change", map[string]any{"document": rel, "revision": aliceRev[:12]}, true); !strings.Contains(out, "only revert commits made with your own app token") {
		t.Errorf("revert of alice's commit = %q", out)
	}
	if out := call("revert_change", map[string]any{"document": rel, "revision": agentRev[:12]}, false); !strings.Contains(out, "reverted "+agentRev[:12]) {
		t.Errorf("revert_change = %q", out)
	}
	if got := e.serverText(rel); got != setupText+"by alice\n" {
		t.Errorf("text after the revert %q", got)
	}
	if out := call("revert_change", map[string]any{"document": rel, "revision": agentRev[:12]}, true); !strings.Contains(out, "not in the page anymore") {
		t.Errorf("second revert = %q", out)
	}
}

func TestMCPReadResource(t *testing.T) {
	e := newSyncEnv(t)
	img := image.NewNRGBA(image.Rect(0, 0, 2000, 1000))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.project, "docs", "guide", "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.writeFile("guide/img/Heizung 1.jpg", buf.String())
	e.writeFile("guide/notes.txt", "some notes\n")
	thermal := thermalJPEG(t, 256, 192)
	e.writeFile("guide/img/P2_thermal.jpg", string(thermal))
	e.writeFile("guide/data.bin", "\x00\x01")
	e.restart()
	s := e.mcpSession(e.appToken("carol", "claude", auth.ScopeRead))
	if !slices.Contains(toolNames(t, s), "read_resource") {
		t.Fatal("read_resource is missing for readers")
	}

	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_resource", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// a link destination of a page, percent-encoded and relative to it
	res := call(map[string]any{"resource": "img/Heizung%201.jpg", "document": "guide/setup.md", "size": 300})
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("image = %+v", res.Content)
	}
	ic, ok := res.Content[1].(*mcp.ImageContent)
	if !ok || ic.MIMEType != "image/jpeg" {
		t.Fatalf("image content = %+v", res.Content[1])
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(ic.Data))
	if err != nil || config.Width != 512 || config.Height != 256 {
		t.Errorf("image = %+v, %v", config, err)
	}
	if tc := res.Content[0].(*mcp.TextContent); !strings.Contains(tc.Text, "path: guide/img/Heizung 1.jpg\n") {
		t.Errorf("header = %q", tc.Text)
	}
	// the path from the wiki root, the default size
	res = call(map[string]any{"resource": "/guide/img/Heizung 1.jpg"})
	if ic, ok = res.Content[len(res.Content)-1].(*mcp.ImageContent); !ok {
		t.Fatalf("by root path = %+v", res.Content)
	}
	if config, _ = jpeg.DecodeConfig(bytes.NewReader(ic.Data)); config.Width != 1024 {
		t.Errorf("default size = %d", config.Width)
	}

	// an image with data beyond the picture says so, and download_resource returns it unchanged
	out, failed := callTool(t, s, "read_resource", map[string]any{"resource": "guide/img/P2_thermal.jpg"})
	if failed || !strings.Contains(out, "camera: InfiRay P2 Pro") || !strings.Contains(out, "download_resource gets the original") {
		t.Errorf("thermal header = %q", out)
	}
	res, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "download_resource", Arguments: map[string]any{"resource": "guide/img/P2_thermal.jpg"}})
	if err != nil || res.IsError || len(res.Content) != 2 {
		t.Fatalf("download_resource = %+v, %v", res, err)
	}
	er, ok := res.Content[1].(*mcp.EmbeddedResource)
	if !ok || !bytes.Equal(er.Resource.Blob, thermal) || er.Resource.MIMEType != "application/octet-stream" || er.Resource.URI != "wiki:///guide/img/P2_thermal.jpg" {
		t.Fatalf("embedded = %+v", res.Content[1])
	}

	out, failed = callTool(t, s, "read_resource", map[string]any{"resource": "guide/notes.txt"})
	if failed || !strings.HasSuffix(out, "---\nsome notes\n") {
		t.Errorf("text = %q", out)
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"resource": "guide/data.bin"}, "cannot show"},
		{map[string]any{"resource": "img/Heizung 1.jpg"}, "no file"},
		{map[string]any{"resource": "../../etc/passwd", "document": "guide/setup.md"}, "no file"},
		{map[string]any{"resource": "guide/img/Heizung 1.jpg", "size": 5000}, "size must be"},
	} {
		if out, failed = callTool(t, s, "read_resource", c.args); !failed || !strings.Contains(out, c.want) {
			t.Errorf("%v = %q, %v", c.args, out, failed)
		}
	}
}
