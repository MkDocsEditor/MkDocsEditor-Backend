package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

func TestMCPPins(t *testing.T) {
	e := newSyncEnv(t)
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	s := e.mcpSession(token)
	pins := func(out string) mcpPins {
		t.Helper()
		var p mcpPins
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return p
	}
	paths := func(list []mcpPin) []string {
		var out []string
		for _, p := range list {
			out = append(out, p.Path)
		}
		return out
	}

	out, failed := callTool(t, s, "list_pins", nil)
	if p := pins(out); failed || len(p.Family) != 0 || len(p.Personal) != 0 {
		t.Fatalf("list_pins = %q", out)
	}
	for _, c := range []struct{ doc, list string }{{"guide/setup.md", "family"}, {"index.md", "family"}, {"guide/setup.md", "personal"}} {
		if out, failed = callTool(t, s, "set_pin", map[string]any{"document": c.doc, "list": c.list, "pinned": true}); failed {
			t.Fatalf("set_pin %v = %q", c, out)
		}
	}
	p := pins(out)
	if !slices.Equal(paths(p.Family), []string{"guide/setup.md", "index.md"}) || !slices.Equal(paths(p.Personal), []string{"guide/setup.md"}) ||
		p.Family[0].PinnedBy == "" || p.Family[0].Title != "Setup" {
		t.Errorf("pins = %+v", p)
	}
	if out, failed = callTool(t, s, "order_pins", map[string]any{"list": "family", "documents": []string{"index"}}); failed ||
		!slices.Equal(paths(pins(out).Family), []string{"index.md", "guide/setup.md"}) {
		t.Errorf("order_pins = %q", out)
	}
	if out, failed = callTool(t, s, "set_pin", map[string]any{"document": "guide/setup.md", "list": "personal", "pinned": false}); failed ||
		len(pins(out).Personal) != 0 {
		t.Errorf("unpin = %q", out)
	}

	// the same as the dashboard of the account
	r := e.doToken(http.MethodGet, "/pins/", token, nil)
	var rest PinsResponse
	r.json(t, &rest)
	if len(rest.Family) != 2 || rest.Family[0].Path != "index.md" || len(rest.Personal) != 0 {
		t.Errorf("GET /pins/ = %s", r.Body.String())
	}

	// errors
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"set_pin", map[string]any{"document": "index.md", "list": "nope", "pinned": true}, "list must be"},
		{"set_pin", map[string]any{"document": "nope.md", "list": "family", "pinned": true}, "nope"},
		{"order_pins", map[string]any{"list": "family", "documents": []string{}}, "1 to 200"},
	} {
		if out, failed = callTool(t, s, c.tool, c.args); !failed || !strings.Contains(out, c.want) {
			t.Errorf("%s %v = %q, %v", c.tool, c.args, out, failed)
		}
	}

	// a read token lists pins, and has no tools to change them
	reader := e.mcpSession(e.appToken("bob", "reader", auth.ScopeRead))
	if out, failed = callTool(t, reader, "list_pins", nil); failed || len(pins(out).Family) != 2 {
		t.Errorf("list_pins with read scope = %q", out)
	}
	if names := toolNames(t, reader); slices.Contains(names, "set_pin") || slices.Contains(names, "order_pins") {
		t.Errorf("read scope tools = %v", names)
	}
}
