package backend

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withLinks gives the sync test wiki documents that link to each other
func (e *syncEnv) withLinks() {
	e.t.Helper()
	e.writeFile("index.md", "# Home\n\nSee [setup](guide/setup.md).\n")
	e.writeFile("guide/setup.md", "# Setup\n\nBack [home](../index.md).\n")
	e.tm.CreateItemTree()
}

func (e *syncEnv) postMove(user string, req MoveRequest) *MoveResult {
	e.t.Helper()
	body, _ := json.Marshal(req)
	rec := e.doBody(http.MethodPost, "/move/", user, string(body))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	var result MoveResult
	if err := json.Unmarshal(rec.bytes(), &result); err != nil {
		e.t.Fatal(err)
	}
	return &result
}

// waitRenamed waits for document-renamed and the close, and returns the new id
func waitRenamed(t *testing.T, c *testClient) string {
	t.Helper()
	var newID string
	c.waitFor("document-renamed", func() bool {
		for _, m := range c.messages {
			if m["type"] == MsgDocumentRenamed {
				newID, _ = m["newDocumentId"].(string)
			}
		}
		return newID != ""
	})
	if code := c.waitClosed(); code != CloseRenamed {
		t.Errorf("close code %d, want %d", code, CloseRenamed)
	}
	return newID
}

// A connected client receives the server's reference rewrite like an edit of another user
func TestMoveRewritesLiveDocument(t *testing.T) {
	e := newSyncEnv(t)
	e.withLinks()
	events := dialEvents(t, e, "carol", false)
	a := newTestClient(t, e, "alice")
	a.connect(e.docID("index.md"))
	a.appendText("Typed while connected.\n")
	eventually(t, "edit on the server", func() bool { return strings.Contains(e.serverText("index.md"), "Typed") })

	e.postMove("bob", MoveRequest{Type: TypeDocument, ID: e.docID("guide/setup.md"), Parent: RootSectionID, Name: "Einrichtung"})
	a.waitFor("the rewrite", func() bool { return strings.Contains(a.textLocked(), "[setup](Einrichtung.md)") })
	if got := a.text(); got != "# Home\n\nSee [setup](Einrichtung.md).\nTyped while connected.\n" {
		t.Errorf("client text: %q", got)
	}
	eventually(t, "file written", func() bool { return strings.Contains(e.readFile("index.md"), "[setup](Einrichtung.md)") })
	if got := e.readFile("Einrichtung.md"); got != "# Setup\n\nBack [home](index.md).\n" {
		t.Errorf("moved document: %q", got)
	}

	// the events websocket tells which ids changed
	eventually(t, "items-moved", func() bool { return events.last(MsgItemsMoved) != nil })
	moves, _ := events.last(MsgItemsMoved)["moves"].([]any)
	if len(moves) != 1 || moves[0].(map[string]any)["newId"] != e.docID("Einrichtung.md") {
		t.Errorf("items-moved: %v", moves)
	}
}

// The guarantee of docs/sync-protocol.md across a move: offline edits of a document that moves,
// and of a document whose references are rewritten, both merge, also when a new document is
// created at the old path.
func TestMoveOfflineEditsMerge(t *testing.T) {
	e := newSyncEnv(t)
	e.withLinks()
	oldSetupID := e.docID("guide/setup.md")

	// A holds the document that will move, B a document that links to it; both go offline
	a := newTestClient(t, e, "alice")
	a.connect(oldSetupID)
	a.disconnect()
	b := newTestClient(t, e, "bob")
	b.connect(e.docID("index.md"))
	b.disconnect()
	a.appendText("Offline in setup.\n")
	b.insertText(0, "Offline in index.\n")

	// meanwhile the section moves, a new document is created at the old path, the server restarts
	guide := e.tm.GetSection(e.tm.GenerateId(filepath.Join(e.project, "docs", "guide")))
	result := e.postMove("alice", MoveRequest{Type: TypeSection, ID: guide.ID, Parent: RootSectionID, Name: "Handbuch"})
	// setup.md's "../index.md" stays valid in a section on the same level
	if len(result.Documents) != 1 || result.Documents[0].Path != "index.md" {
		t.Fatalf("documents: %+v", result.Documents)
	}
	if rec := e.doBody(http.MethodPost, "/section/", "alice", `{"parent":"root","name":"guide"}`); rec.Code != http.StatusOK {
		t.Fatalf("create section: %d", rec.Code)
	}
	newGuide := e.tm.GetSection(e.tm.GenerateId(filepath.Join(e.project, "docs", "guide")))
	if rec := e.doBody(http.MethodPost, "/document/", "alice", `{"parent":"`+newGuide.ID+`","name":"setup"}`); rec.Code != http.StatusOK {
		t.Fatalf("create document: %d", rec.Code)
	}
	e.restart()

	// A reconnects with the old id and its epoch: the old id belongs to the new document now,
	// but the epoch says A holds the moved one
	a.dial(oldSetupID)
	newID := waitRenamed(t, a)
	if newID != e.docID("Handbuch/setup.md") {
		t.Fatalf("redirected to %s", newID)
	}
	epoch := a.epoch
	a.connect(newID)
	if a.epoch != epoch {
		t.Fatal("the epoch changed: the client would re-apply its edits as a text diff")
	}
	want := "# Setup\n\nBack [home](../index.md).\nOffline in setup.\n"
	a.waitFor("merged setup", func() bool { return a.textLocked() == want })
	eventually(t, "setup on the server", func() bool { return e.serverText("Handbuch/setup.md") == want })

	// B reconnects to the unchanged id: its offline edit and the rewrite merge
	b.connect(e.docID("index.md"))
	want = "Offline in index.\n# Home\n\nSee [setup](Handbuch/setup.md).\n"
	b.waitFor("merged index", func() bool { return b.textLocked() == want })
	eventually(t, "index on the server", func() bool { return e.serverText("index.md") == want })
	a.disconnect()
	b.disconnect()
	eventually(t, "files", func() bool {
		return e.readFile("index.md") == want && strings.HasSuffix(e.readFile("Handbuch/setup.md"), "Offline in setup.\n")
	})

	// the new document at the old path is untouched
	if got := e.readFile("guide/setup.md"); got != "" {
		t.Errorf("new document: %q", got)
	}
	// a client without an epoch for the new document's id gets the new document
	c := newTestClient(t, e, "bob")
	c.connect(oldSetupID)
	if c.text() != "" {
		t.Errorf("new document text %q", c.text())
	}
}

// A client that was offline during the move and connects to an old id that nothing else uses
// is redirected right after hello
func TestMoveRedirectsOldIDAfterHello(t *testing.T) {
	e := newSyncEnv(t)
	e.withLinks()
	oldID := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(oldID)
	a.disconnect()

	e.postMove("bob", MoveRequest{Type: TypeDocument, ID: oldID, Name: "install"})
	a.appendText("offline\n")
	if err := a.dial(oldID); err != nil {
		t.Fatalf("the old id must be upgraded: %v", err)
	}
	newID := waitRenamed(t, a)
	if newID != e.docID("guide/install.md") {
		t.Fatalf("redirected to %s", newID)
	}
	a.connect(newID)
	eventually(t, "offline edit merged", func() bool { return strings.HasSuffix(e.serverText("guide/install.md"), "offline\n") })

	// redirects survive a restart; an unknown id still answers 404 before the upgrade
	e.restart()
	if rec := e.do(http.MethodGet, "/moved/"+oldID+"/", "carol"); rec.Code != http.StatusOK {
		t.Errorf("moved after restart: %d", rec.Code)
	}
	if err := newTestClient(t, e, "alice").dial("1234"); err == nil {
		t.Error("an unknown id was upgraded")
	}
}

// A move is one git commit: a rename with history, the rewritten documents and mkdocs.yml,
// authored by the user who moved
func TestMoveGitCommit(t *testing.T) {
	e := newGitEnv(t, GitCommitterOptions{})
	e.withLinks()
	e.writeFile("guide/setup.md", "# Setup\n\nBack [home](../index.md).\n\n"+strings.Repeat("Some longer text of the page.\n", 10))
	if err := os.WriteFile(filepath.Join(e.project, "mkdocs.yml"),
		[]byte("site_name: Test Wiki\nextra_css:\n  - stylesheets/extra.css\nnav:\n  - Home: index.md\n  - Setup: guide/setup.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git0("add", "-A")
	e.git0("commit", "-q", "-m", "Links")

	e.tm.CreateItemTree()
	result := e.postMove("bob", MoveRequest{Type: TypeDocument, ID: e.docID("guide/setup.md"), Parent: RootSectionID, Name: "Einrichtung"})
	if result.Commit == "" {
		t.Fatal("no commit hash")
	}
	if head := strings.TrimSpace(e.git0("rev-parse", "--short=12", "HEAD")); head != result.Commit {
		t.Errorf("commit %s, HEAD %s", result.Commit, head)
	}
	message := e.git0("log", "-1", "--format=%an <%ae>%n%B")
	want := "bob <bob@mkdocseditor.invalid>\nMove guide/setup.md to Einrichtung.md\n\nUpdate references in 2 documents:\n- Einrichtung.md\n- index.md\n- mkdocs.yml\n"
	if strings.TrimSpace(message) != strings.TrimSpace(want) {
		t.Errorf("commit message:\n%s\nwant\n%s", message, want)
	}
	status := e.git0("show", "--name-status", "-M", "--format=", "HEAD")
	for _, line := range []string{"R", "docs/guide/setup.md\tdocs/Einrichtung.md", "M\tdocs/index.md", "M\tmkdocs.yml"} {
		if !strings.Contains(status, line) {
			t.Errorf("commit is missing %q:\n%s", line, status)
		}
	}
	if out := e.git0("status", "--porcelain"); strings.Contains(out, "docs/") || strings.Contains(out, "mkdocs.yml") {
		t.Errorf("uncommitted changes:\n%s", out)
	}
	// the history follows the move
	time.Sleep(10 * time.Millisecond)
	rec := e.do(http.MethodGet, "/document/"+e.docID("Einrichtung.md")+"/history/", "carol")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Links") {
		t.Errorf("history: %d %s", rec.Code, rec.Body.String())
	}
}
