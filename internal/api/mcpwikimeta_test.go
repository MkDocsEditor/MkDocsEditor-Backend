package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

func TestMCPPageMetaAndWikiConfig(t *testing.T) {
	e := newSyncEnv(t)
	e.writeFile("guide/setup.md", "---\ntitle: Getting started\ndescription: First steps\ntags: [setup, intro]\nstatus: new\n---\n# Setup\n")
	e.tm.CreateItemTree()
	s := e.mcpSession(e.appToken("bob", "reader", auth.ScopeRead))

	out, failed := callTool(t, s, "list_tree", map[string]any{"meta": true})
	if failed || !strings.HasPrefix(out, "path\ttitle\tmeta\n") ||
		!strings.Contains(out, "guide/setup.md\tGetting started\ttags: setup, intro; status: new; description: First steps\n") {
		t.Errorf("list_tree meta = %q", out)
	}
	if out, _ = callTool(t, s, "list_tree", map[string]any{"tag": "intro"}); out != "path\ttitle\nguide/setup.md\tGetting started" {
		t.Errorf("list_tree tag = %q", out)
	}
	if out, _ = callTool(t, s, "list_tree", map[string]any{"tag": "nope"}); out != "no pages" {
		t.Errorf("list_tree unknown tag = %q", out)
	}

	// the filtered config: a secret of mkdocs.yml never reaches the agent
	config := filepath.Join(e.project, "mkdocs.yml")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, append(data, []byte("markdown_extensions:\n  - admonition\nplugins:\n  - search\n  - git-sync:\n      api_token: s3cret\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, failed = callTool(t, s, "read_wiki_config", nil)
	if failed || !strings.Contains(out, `"siteName": "Test Wiki"`) || !strings.Contains(out, `"admonition"`) || !strings.Contains(out, `"git-sync"`) ||
		strings.Contains(out, "s3cret") {
		t.Errorf("read_wiki_config = %q", out)
	}
}
