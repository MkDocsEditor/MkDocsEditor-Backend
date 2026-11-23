package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
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

	want := []string{"changes_since", "check_links", "create_document", "create_section", "delete_document", "delete_resource",
		"delete_section", "download_resource", "edit_document", "get_backlinks", "link_to", "list_tree", "move_document",
		"move_resource", "move_section", "point_at", "read_document", "read_resource", "recent_changes", "request_upload", "search", "upload_resource",
		"write_document"}
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
	if names := toolNames(t, s); !slices.Equal(names, []string{"changes_since", "check_links", "download_resource", "get_backlinks", "link_to",
		"list_tree", "point_at", "read_document", "read_resource", "recent_changes", "search"}) {
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

func TestMCPTreeTools(t *testing.T) {
	e := newSyncEnv(t)
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	ok := func(name string, args map[string]any) string {
		t.Helper()
		out, failed := callTool(t, s, name, args)
		if failed {
			t.Fatalf("%s %v: %s", name, args, out)
		}
		return out
	}
	fails := func(name string, args map[string]any, want string) {
		t.Helper()
		if out, failed := callTool(t, s, name, args); !failed || !strings.Contains(out, want) {
			t.Errorf("%s %v = %q, %v; want an error with %q", name, args, out, failed, want)
		}
	}

	// upload next to a page, relative to it, into a new section
	out := ok("upload_resource", map[string]any{"path": "tools/p2pro_thermal.py", "document": "guide/setup.md", "text": "print(1)\n"})
	if !strings.Contains(out, "uploaded guide/tools/p2pro_thermal.py") || !strings.Contains(out, "created sections: guide/tools") {
		t.Fatalf("upload = %q", out)
	}
	if got := e.readFile("guide/tools/p2pro_thermal.py"); got != "print(1)\n" {
		t.Fatalf("file = %q", got)
	}
	fails("upload_resource", map[string]any{"path": "guide/tools/p2pro_thermal.py", "text": "x"}, "pass replace")
	ok("upload_resource", map[string]any{"path": "guide/tools/p2pro_thermal.py", "base64": "cHJpbnQoMikK", "replace": true})
	if got := e.readFile("guide/tools/p2pro_thermal.py"); got != "print(2)\n" {
		t.Fatalf("replaced file = %q", got)
	}
	fails("upload_resource", map[string]any{"path": "guide/new.md", "text": "# x"}, "create_document")
	fails("upload_resource", map[string]any{"path": "../outside.txt", "document": "index.md", "text": "x"}, "invalid path")
	fails("upload_resource", map[string]any{"path": "guide/.hidden", "text": "x"}, "invalid file name")

	out = ok("list_tree", map[string]any{"files": true})
	if !strings.Contains(out, "sections:\nguide/\nguide/tools/") || !strings.Contains(out, "guide/tools/p2pro_thermal.py\t9 B\t") {
		t.Errorf("list_tree files = %q", out)
	}

	// link to it, use the link, move the file: the link follows
	out = ok("link_to", map[string]any{"document": "guide/setup.md", "target": "guide/tools/p2pro_thermal.py"})
	if !strings.Contains(out, `"destination":"tools/p2pro_thermal.py"`) {
		t.Fatalf("link_to = %q", out)
	}
	id := e.docID("guide/setup.md")
	_, revision := e.readContent(id, e.appToken("bob", "other", auth.ScopeWrite))
	ok("write_document", map[string]any{"document": id, "baseRevision": revision, "content": setupText + "\n[script](tools/p2pro_thermal.py)\n[gone](nope.md)\n"})
	eventually(t, "reference index", func() bool {
		out, _ = callTool(t, s, "check_links", map[string]any{"section": "guide"})
		return strings.Contains(out, "guide/setup.md:") && strings.Contains(out, "nope.md")
	})
	if strings.Contains(out, "p2pro_thermal.py") {
		t.Errorf("check_links lists the linked file: %q", out)
	}
	out = ok("move_resource", map[string]any{"resource": "guide/tools/p2pro_thermal.py", "section": "/", "name": "thermal.py"})
	if !strings.Contains(out, `"newPath":"thermal.py"`) || !strings.Contains(out, `"updatedDocuments":1`) {
		t.Fatalf("move_resource = %q", out)
	}
	eventually(t, "updated link", func() bool { return strings.Contains(e.serverText("guide/setup.md"), "(../thermal.py)") })

	// sections: create, move, delete
	ok("create_section", map[string]any{"path": "archive/2026"})
	fails("create_section", map[string]any{"path": "archive"}, "exists")
	out = ok("move_section", map[string]any{"section": "guide/tools", "target": "archive"})
	if !strings.Contains(out, `"newPath":"archive/tools"`) {
		t.Errorf("move_section = %q", out)
	}
	fails("delete_section", map[string]any{"section": "archive"}, "pass recursive")
	ok("delete_section", map[string]any{"section": "archive", "recursive": true})
	fails("delete_section", map[string]any{"section": "/"}, "top level")

	// deleting the linked file lists the broken link
	out = ok("delete_resource", map[string]any{"resource": "thermal.py"})
	if !strings.Contains(out, "deleted thermal.py") || !strings.Contains(out, "guide/setup.md:") {
		t.Errorf("delete_resource = %q", out)
	}
	ok("create_document", map[string]any{"section": "", "name": "scratch"})
	out = ok("delete_document", map[string]any{"document": "scratch"})
	if out != "deleted scratch.md" {
		t.Errorf("delete_document = %q", out)
	}

	out = ok("recent_changes", map[string]any{"limit": 50})
	if !strings.Contains(out, "deleted document scratch.md by Bob") && !strings.Contains(out, "deleted document scratch.md by bob") {
		t.Errorf("recent_changes = %q", out)
	}

	// readers get none of the write tools
	r := e.mcpSession(e.appToken("carol", "claude", auth.ScopeWrite))
	if names := toolNames(t, r); slices.Contains(names, "upload_resource") || slices.Contains(names, "delete_document") {
		t.Errorf("reader tools = %v", names)
	}
}

func TestMCPTreeToolsWithGit(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	if out, failed := callTool(t, s, "upload_resource", map[string]any{"path": "tools/a.py", "text": "a\n"}); failed {
		t.Fatal(out)
	}
	if got := e.commits()[0]; got != "Upload tools/a.py" {
		t.Errorf("upload commit %q", got)
	}
	if out, failed := callTool(t, s, "upload_resource", map[string]any{"path": "tools/a.py", "text": "b\n", "replace": true}); failed {
		t.Fatal(out)
	}
	if got := e.commits()[0]; got != "Replace tools/a.py" {
		t.Errorf("replace commit %q", got)
	}
	if out, failed := callTool(t, s, "delete_resource", map[string]any{"resource": "tools/a.py"}); failed {
		t.Fatal(out)
	}
	if got := e.commits()[0]; got != "Delete tools/a.py" {
		t.Errorf("delete commit %q", got)
	}
	out, failed := callTool(t, s, "blame", map[string]any{"document": "guide/setup.md"})
	if failed || !strings.Contains(out, "path: guide/setup.md") || !strings.Contains(out, "by Test: Initial") {
		t.Errorf("blame = %q", out)
	}
}

func TestMCPRequestUpload(t *testing.T) {
	e := newSyncEnv(t)
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	urlOf := func(out string) string {
		i := strings.Index(out, "'")
		j := strings.LastIndex(out, "'")
		if i < 0 || j <= i {
			t.Fatalf("no URL in %q", out)
		}
		return out[i+1 : j]
	}
	put := func(url string, body []byte) (int, string) {
		req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// a file above the limit of a tool call, no login on the PUT
	out, failed := callTool(t, s, "request_upload", map[string]any{"path": "img/big.bin"})
	if failed || !strings.Contains(out, "curl -sS -T <file> '"+e.http.URL+APIPrefix+EndpointPathUpload) {
		t.Fatalf("request_upload = %q, %v", out, failed)
	}
	big := bytes.Repeat([]byte{0xAB, 0x00, 0x7F}, 2<<20)
	url := urlOf(out)
	if code, body := put(url, big); code != http.StatusOK || !strings.Contains(body, "uploaded img/big.bin") {
		t.Fatalf("PUT = %d %q", code, body)
	}
	if got := e.readFile("img/big.bin"); got != string(big) {
		t.Errorf("stored %d bytes, want %d", len(got), len(big))
	}
	// the URL works once
	if code, _ := put(url, big); code != http.StatusNotFound {
		t.Errorf("second PUT = %d", code)
	}
	if code, _ := put(e.http.URL+APIPrefix+EndpointPathUpload+"nope/", big); code != http.StatusNotFound {
		t.Errorf("unknown URL = %d", code)
	}

	// an existing file needs replace, at request and at upload
	if out, failed = callTool(t, s, "request_upload", map[string]any{"path": "img/big.bin"}); !failed || !strings.Contains(out, "pass replace") {
		t.Errorf("existing = %q, %v", out, failed)
	}
	out, _ = callTool(t, s, "request_upload", map[string]any{"path": "img/big.bin", "replace": true})
	if code, body := put(urlOf(out), []byte("small")); code != http.StatusOK || !strings.Contains(body, "replaced img/big.bin") {
		t.Fatalf("replace = %d %q", code, body)
	}
	if got := e.readFile("img/big.bin"); got != "small" {
		t.Errorf("replaced = %q", got)
	}

	// the file appeared after the URL was issued
	out, _ = callTool(t, s, "request_upload", map[string]any{"path": "late.txt"})
	callTool(t, s, "upload_resource", map[string]any{"path": "late.txt", "text": "x"})
	if code, body := put(urlOf(out), []byte("y")); code != http.StatusConflict || !strings.Contains(body, "pass replace") {
		t.Errorf("late = %d %q", code, body)
	}

	// pages, empty bodies, expired URLs
	if out, failed = callTool(t, s, "request_upload", map[string]any{"path": "x.md"}); !failed || !strings.Contains(out, "create_document") {
		t.Errorf("page = %q, %v", out, failed)
	}
	out, _ = callTool(t, s, "request_upload", map[string]any{"path": "empty.txt"})
	if code, _ := put(urlOf(out), nil); code != http.StatusBadRequest {
		t.Errorf("empty = %d", code)
	}
	out, _ = callTool(t, s, "request_upload", map[string]any{"path": "old.txt"})
	e.rs.uploads.mu.Lock()
	for _, tk := range e.rs.uploads.tickets {
		tk.expires = time.Now().Add(-time.Second)
	}
	e.rs.uploads.mu.Unlock()
	if code, _ := put(urlOf(out), []byte("z")); code != http.StatusNotFound {
		t.Errorf("expired = %d", code)
	}

	// server.maxUploadBytes, and no temporary file is left behind
	e.rs.server.MaxUploadBytes = 1000
	out, _ = callTool(t, s, "request_upload", map[string]any{"path": "limited.bin"})
	if !strings.Contains(out, "1000 B") {
		t.Errorf("limit not named: %q", out)
	}
	if code, _ := put(urlOf(out), make([]byte, 1001)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large = %d", code)
	}
	e.rs.server.MaxUploadBytes = 0
	if left, _ := filepath.Glob(filepath.Join(e.project, "docs", ".upload-*")); len(left) > 0 {
		t.Errorf("left over: %v", left)
	}

	// readers get no tool
	r := e.mcpSession(e.appToken("carol", "claude", auth.ScopeWrite))
	if names := toolNames(t, r); slices.Contains(names, "request_upload") {
		t.Errorf("reader tools = %v", names)
	}
}
