package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

const testClientURL = "https://wiki.example"

// newURLEnv is a sync env whose web client is at clientURL ("" for none)
func newURLEnv(t *testing.T, clientURL string) *syncEnv {
	t.Helper()
	return newSyncEnv(t, func(c *configuration.Configuration) { c.Server.Auth.ClientURL = clientURL })
}

func TestMCPPageURLs(t *testing.T) {
	e := newURLEnv(t, testClientURL+"/")
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	setupURL := testClientURL + "/document/" + e.docID("guide/setup.md")

	out, failed := callTool(t, s, "read_document", map[string]any{"document": "guide/setup.md"})
	if failed || !strings.Contains(out, "\nurl: "+setupURL+"\n") {
		t.Fatalf("read_document = %q", out)
	}
	revision := ""
	for _, l := range strings.Split(out, "\n") {
		if r, ok := strings.CutPrefix(l, "revision: "); ok {
			revision = r
		}
	}
	out, failed = callTool(t, s, "edit_document", map[string]any{"document": "guide/setup.md", "baseRevision": revision,
		"edits": []map[string]string{{"old": "Some text.", "new": "Agent text."}}})
	if failed || !strings.Contains(out, `"url":"`+setupURL+`"`) {
		t.Errorf("edit_document = %q", out)
	}
	if out, _ = callTool(t, s, "search", map[string]any{"query": "agent text"}); !strings.Contains(out, `"url":"`+setupURL+`"`) {
		t.Errorf("search = %q", out)
	}
	out, _ = callTool(t, s, "list_tree", map[string]any{"files": true})
	for _, want := range []string{"page url: " + testClientURL + "/document/<id>\npath\tid\ttitle\n",
		"guide/setup.md\t" + e.docID("guide/setup.md") + "\tSetup", "url " + testClientURL + "/section/<id>"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_tree has no %q: %q", want, out)
		}
	}

	// a new page, moved: the url follows its id
	out, failed = callTool(t, s, "create_document", map[string]any{"section": "guide", "name": "fresh", "content": "# Fresh\n"})
	var created mcpCreated
	if failed || json.Unmarshal([]byte(out), &created) != nil || created.URL != testClientURL+"/document/"+created.ID {
		t.Fatalf("create_document = %q", out)
	}
	out, failed = callTool(t, s, "move_document", map[string]any{"document": "guide/fresh.md", "name": "moved"})
	var moved mcpMoved
	if failed || json.Unmarshal([]byte(out), &moved) != nil || moved.URL != testClientURL+"/document/"+e.docID("guide/moved.md") {
		t.Errorf("move_document = %q", out)
	}

	if out, failed = callTool(t, s, "set_pin", map[string]any{"document": "guide/setup.md", "list": "personal", "pinned": true}); failed ||
		!strings.Contains(out, `"url":"`+setupURL+`"`) {
		t.Errorf("set_pin = %q", out)
	}
}

// Without a configured web client there are no urls, never one guessed from the request
func TestMCPNoPageURLsWithoutClientURL(t *testing.T) {
	e := newURLEnv(t, "")
	s := e.mcpSession(e.appToken("bob", "claude", auth.ScopeWrite))
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"read_document", map[string]any{"document": "guide/setup.md"}},
		{"list_tree", map[string]any{"files": true}},
		{"search", map[string]any{"query": "some text"}},
		{"create_document", map[string]any{"section": "guide", "name": "fresh"}},
	} {
		if out, failed := callTool(t, s, c.tool, c.args); failed || strings.Contains(out, "url") || strings.Contains(out, "http") {
			t.Errorf("%s = %q", c.tool, out)
		}
	}
}
