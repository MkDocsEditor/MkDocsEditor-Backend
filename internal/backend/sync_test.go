package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	automerge "github.com/automerge/automerge-go"
)

const setupText = "# Setup\n\nSome text.\n"

func TestSyncTwoClientsConverge(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")

	a := newTestClient(t, e, "alice")
	a.connect(id)
	if a.text() != setupText || a.readOnly {
		t.Fatalf("initial text %q readOnly=%v", a.text(), a.readOnly)
	}
	b := newTestClient(t, e, "bob")
	b.connect(id, connectOptions{useTicket: true})
	if b.epoch != a.epoch {
		t.Fatal("clients got different epochs")
	}

	a.insertText(0, "A ")
	b.appendText("B äöü 😀\n")
	want := func(s string) bool {
		return strings.HasPrefix(s, "A # Setup") && strings.HasSuffix(s, "B äöü 😀\n")
	}
	a.waitFor("b's edit", func() bool { return want(a.textLocked()) })
	b.waitFor("a's edit", func() bool { return want(b.textLocked()) })
	if a.text() != b.text() {
		t.Fatalf("diverged: %q vs %q", a.text(), b.text())
	}
	eventually(t, "server text", func() bool { return e.serverText("guide/setup.md") == a.text() })
	eventually(t, "disk write", func() bool { return e.readFile("guide/setup.md") == a.text() })

	// the actor map knows both clients
	a.waitFor("b's actor", func() bool { return a.actors[b.actorID] == "bob" })
	rec := e.do(http.MethodGet, "/actors/", "alice")
	var actors []ActorInfo
	_ = json.Unmarshal(rec.bytes(), &actors)
	found := map[string]string{}
	for _, info := range actors {
		found[info.ActorID] = info.Username
	}
	if found[a.actorID] != "alice" || found[b.actorID] != "bob" {
		t.Errorf("GET /actors/ = %s", rec.Body.String())
	}

	// REST content returns the live text
	rec = e.do(http.MethodGet, "/document/"+id+"/content/", "bob")
	if rec.Body.String() != a.text() {
		t.Errorf("REST content %q", rec.Body.String())
	}

	a.disconnect()
	b.disconnect()
	eventually(t, "unload", func() bool {
		_, loaded := e.ss.CurrentText(e.tm.GetDocument(id))
		return !loaded
	})
	if e.readFile("guide/setup.md") != a.text() {
		t.Error("disk content lost after unload")
	}
}

// do sends an authenticated REST request to the env
func (e *syncEnv) do(method, path, user string) *httpRecorder {
	req, _ := http.NewRequest(method, e.http.URL+path, nil)
	req.Header.Set("Authorization", authHeader(e.t, e.authn, user, testPasswords[user]))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := &httpRecorder{Code: resp.StatusCode}
	_, _ = r.Body.ReadFrom(resp.Body)
	return r
}

type httpRecorder struct {
	Code int
	Body bytes.Buffer
}

func (b *httpRecorder) bytes() []byte { return b.Body.Bytes() }

// The guarantee of docs/sync-protocol.md: A edits offline, B edits, the server restarts,
// the file changes on disk; after A reconnects all three edits are everywhere.
func TestSyncOfflineEditsSurviveRestartAndDiskChange(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")

	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.settle()
	a.disconnect()

	// A goes offline and edits
	a.appendText("offline edit by A\n")

	// meanwhile B edits
	b := newTestClient(t, e, "bob")
	b.connect(id)
	b.insertText(0, "B was here. ")
	eventually(t, "b's edit on the server", func() bool { return strings.HasPrefix(e.serverText("guide/setup.md"), "B was here. ") })
	b.disconnect()

	// the server restarts
	e.restart()

	// an external change arrives on disk (git pull)
	disk := e.readFile("guide/setup.md")
	if !strings.HasPrefix(disk, "B was here. ") {
		t.Fatalf("b's edit not on disk before restart: %q", disk)
	}
	e.writeFile("guide/setup.md", strings.Replace(disk, "Some text.", "Some text from git.", 1))

	// A reconnects with its old document
	oldEpoch := a.epoch
	a.connect(id)
	if a.epoch != oldEpoch {
		t.Fatal("the epoch changed across a restart")
	}
	has3 := func(s string) bool {
		return strings.HasPrefix(s, "B was here. ") && strings.Contains(s, "Some text from git.") && strings.Contains(s, "offline edit by A\n")
	}
	a.waitFor("all three edits on A", func() bool { return has3(a.textLocked()) })
	eventually(t, "all three edits on the server", func() bool { return has3(e.serverText("guide/setup.md")) })
	eventually(t, "all three edits on disk", func() bool { return has3(e.readFile("guide/setup.md")) })

	b.connect(id)
	b.waitFor("all three edits on B", func() bool { return has3(b.textLocked()) })
	a.waitFor("convergence", func() bool { return a.textLocked() == b.textLocked() })
	if strings.Count(a.text(), "Some text") != 1 {
		t.Errorf("text duplicated: %q", a.text())
	}
}

func TestSyncExternalChangeWhileConnected(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)

	a.appendText("typing...\n")
	eventually(t, "a's edit on the server", func() bool { return strings.HasSuffix(e.serverText("guide/setup.md"), "typing...\n") })
	// an edit on disk that does not contain a's latest edit yet (the flush may not have happened)
	e.writeFile("guide/setup.md", "# Setup from disk\n\nSome text.\n")
	e.ss.CheckDisk()

	want := func(s string) bool {
		return strings.HasPrefix(s, "# Setup from disk\n") && strings.HasSuffix(s, "typing...\n")
	}
	a.waitFor("disk change on A", func() bool { return want(a.textLocked()) })
	eventually(t, "merged text on disk", func() bool { return want(e.readFile("guide/setup.md")) })

	// the disk change is attributed to the disk pseudo user
	a.waitFor("disk actor", func() bool {
		for _, user := range a.actors {
			if user == ActorUserDisk {
				return true
			}
		}
		return false
	})
}

func TestSyncReaderChangesAreDropped(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	carol := newTestClient(t, e, "carol")
	carol.connect(id)
	if !carol.readOnly {
		t.Fatal("reader not marked read-only")
	}
	carol.appendText("vandalism\n")
	carol.waitFor("read-only error", func() bool { return len(carol.errors) > 0 && carol.errors[0] == ErrCodeReadOnly })

	// the reader still receives edits of others
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.insertText(0, "Edited. ")
	carol.waitFor("alice's edit", func() bool { return strings.HasPrefix(carol.textLocked(), "Edited. ") })
	a.settle()
	if strings.Contains(e.serverText("guide/setup.md"), "vandalism") || strings.Contains(a.text(), "vandalism") {
		t.Error("reader change reached the document")
	}
}

func TestSyncPresence(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)

	b.waitFor("snapshot with a", func() bool { return len(b.presences) == 1 })
	a.waitFor("b's join", func() bool { return len(a.presences) == 1 })

	sel := []map[string]string{{"anchor": "cursor-1", "head": "cursor-2"}}
	// presence fields are stamped by the server, a forged username is overwritten
	b.send(map[string]any{"type": MsgPresence, "documentId": id, "username": "mallory", "selections": sel})
	a.waitFor("b's selection", func() bool {
		for _, p := range a.presences {
			if s, ok := p["selections"].([]any); ok && len(s) == 1 {
				return true
			}
		}
		return false
	})
	a.waitFor("stamped presence", func() bool {
		for _, p := range a.presences {
			if p["username"] != "bob" || p["actorId"] != b.actorID || p["color"] == "" {
				t.Errorf("presence = %v", p)
			}
		}
		return true
	})

	b.send(map[string]any{"type": MsgPresence, "documentId": id, "selections": "not a list"})
	b.waitFor("error", func() bool { return len(b.errors) > 0 })

	b.disconnect()
	a.waitFor("presence-leave", func() bool { return len(a.presences) == 0 })
}

func TestSyncAuthAndProtocolErrors(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")

	// no credentials, no ticket
	anon := newTestClient(t, e, "")
	if err := anon.dial(id); err != nil {
		t.Fatal(err)
	}
	if code := anon.waitClosed(); code != CloseUnauthorized {
		t.Errorf("anonymous close code %d", code)
	}

	// invalid ticket
	bad := newTestClient(t, e, "")
	if err := bad.dial(id, connectOptions{hello: map[string]any{"ticket": "nope"}}); err != nil {
		t.Fatal(err)
	}
	if code := bad.waitClosed(); code != CloseUnauthorized {
		t.Errorf("bad ticket close code %d", code)
	}

	// first message is not hello
	c := newTestClient(t, e, "alice")
	if err := c.dial(id, connectOptions{noHello: true}); err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"type": MsgSyncRequest})
	if code := c.waitClosed(); code != CloseProtocolError {
		t.Errorf("no hello close code %d", code)
	}

	// an actor id of another user
	a := newTestClient(t, e, "alice")
	a.connect(id)
	thief := newTestClient(t, e, "bob")
	thief.actorID = a.actorID
	if err := thief.dial(id); err != nil {
		t.Fatal(err)
	}
	if code := thief.waitClosed(); code != CloseForbidden {
		t.Errorf("foreign actor close code %d", code)
	}

	// changes of another user's actor inside a sync message
	forger := newTestClient(t, e, "bob")
	forger.connect(id)
	forger.mu.Lock()
	fork, _ := forger.doc.Fork()
	_ = fork.SetActorID(a.actorID)
	_ = fork.Path(contentKey).Text().Append("forged\n")
	_, _ = fork.Commit("forged")
	_, _ = forger.doc.Merge(fork)
	forger.flush()
	forger.mu.Unlock()
	if code := forger.waitClosed(); code != CloseForbidden {
		t.Errorf("forged change close code %d", code)
	}
	a.settle()
	if strings.Contains(a.text(), "forged") {
		t.Error("forged change was applied")
	}

	// unknown document
	u := newTestClient(t, e, "alice")
	if err := u.dial("unknown"); err == nil {
		t.Error("websocket to an unknown document opened")
	}
}

func TestSyncHeartbeatTimeout(t *testing.T) {
	e := newSyncEnv(t)
	e.ss.HeartbeatTimeout = 300 * time.Millisecond
	e.ss.HeartbeatPeriod = 100 * time.Millisecond
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	// the server sends heartbeats, the client sends nothing
	if code := a.waitClosed(); code != CloseHeartbeatTimout {
		t.Errorf("close code %d", code)
	}
}

func TestSyncRenameAndDelete(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.appendText("before rename\n")
	eventually(t, "edit on server", func() bool { return strings.HasSuffix(e.serverText("guide/setup.md"), "before rename\n") })

	// rename the section above the document
	guide := e.tm.GetSection(e.tm.GenerateID(filepath.Join(e.project, "docs", "guide")))
	req, _ := http.NewRequest(http.MethodPut, e.http.URL+"/section/"+guide.ID+"/", strings.NewReader(`{"name":"handbook"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader(t, e.authn, "alice", "alice-pw"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	newID := e.docID("handbook/setup.md")
	var renamed map[string]any
	a.waitFor("document-renamed", func() bool {
		for _, m := range a.messages {
			if m["type"] == MsgDocumentRenamed {
				renamed = m
			}
		}
		return renamed != nil
	})
	if renamed["newDocumentId"] != newID || renamed["documentId"] != id {
		t.Errorf("renamed = %v", renamed)
	}
	if code := a.waitClosed(); code != CloseRenamed {
		t.Errorf("close code %d", code)
	}
	if !strings.HasSuffix(e.readFile("handbook/setup.md"), "before rename\n") {
		t.Error("edit lost in rename")
	}

	// reconnect to the new id with the same epoch: the state moved along
	epoch := a.epoch
	a.connect(newID)
	if a.epoch != epoch {
		t.Error("epoch changed after rename")
	}
	a.appendText("after rename\n")
	eventually(t, "edit after rename", func() bool { return strings.HasSuffix(e.serverText("handbook/setup.md"), "after rename\n") })

	// deleting a document with connected clients needs force
	if rec := e.do(http.MethodDelete, "/document/"+newID+"/", "bob"); rec.Code != http.StatusConflict {
		t.Fatalf("delete without force: %d", rec.Code)
	}
	if rec := e.do(http.MethodDelete, "/document/"+newID+"/?force=true", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("delete with force: %d", rec.Code)
	}
	if code := a.waitClosed(); code != CloseNotFound {
		t.Errorf("close code after delete %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.project, "docs", "handbook", "setup.md")); !os.IsNotExist(err) {
		t.Error("file still exists")
	}

	// the file comes back (git checkout): the archived document is restored with its epoch
	e.writeFile("handbook/setup.md", "restored\n")
	e.tm.CreateItemTree()
	a.connect(newID)
	if a.epoch != epoch {
		t.Error("the restored document has a new epoch")
	}
	if a.text() != "restored\n" {
		t.Errorf("restored text %q", a.text())
	}
}

func TestSyncDeletedOnDisk(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	if err := os.Remove(filepath.Join(e.project, "docs", "guide", "setup.md")); err != nil {
		t.Fatal(err)
	}
	e.tm.CreateItemTree()
	if code := a.waitClosed(); code != CloseNotFound {
		t.Errorf("close code %d", code)
	}
}

func TestSyncLostStateStartsNewEpoch(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.settle()
	a.disconnect()
	a.mu.Lock()
	serverHeads := a.doc.Heads()
	a.mu.Unlock()
	a.appendText("offline\n")

	e.stop()
	if err := os.RemoveAll(e.stateDir); err != nil {
		t.Fatal(err)
	}
	e.start()

	a.mu.Lock()
	old := a.doc
	oldEpoch := a.epoch
	a.doc = nil
	a.mu.Unlock()
	a.connect(id)
	if a.epoch == oldEpoch {
		t.Fatal("epoch did not change after the state was lost")
	}
	// the client re-applies its unsynced edits as a text diff (docs/sync-protocol.md, Epochs)
	base, err := textAt(old, serverHeads)
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := contentText(old)
	a.edit(func(text *automerge.Text) error { return spliceDiff(text, base, mine) })
	eventually(t, "re-applied edit on the server", func() bool {
		return e.serverText("guide/setup.md") == setupText+"offline\n"
	})
}

func TestSyncCorruptStateIsKept(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.disconnect()
	e.stop()

	files, _ := filepath.Glob(filepath.Join(e.stateDir, "documents", "*.automerge"))
	if len(files) != 1 {
		t.Fatalf("state files: %v", files)
	}
	if err := os.WriteFile(files[0], []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.start()
	b := newTestClient(t, e, "bob")
	b.connect(id)
	if b.text() != setupText {
		t.Errorf("text %q", b.text())
	}
	corrupt, _ := filepath.Glob(filepath.Join(e.stateDir, "documents", "*.corrupt-*"))
	if len(corrupt) != 2 {
		t.Errorf("corrupt files kept: %v", corrupt)
	}
}

func TestSyncConcurrentClients(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("index.md")
	clients := []*testClient{newTestClient(t, e, "alice"), newTestClient(t, e, "bob"), newTestClient(t, e, "alice")}
	for _, c := range clients {
		c.connect(id)
	}
	for range 10 {
		for i, c := range clients {
			c.appendText(string(rune('a'+i)) + "\n")
			time.Sleep(5 * time.Millisecond)
		}
	}
	want := 30
	for _, c := range clients {
		c.waitFor("all edits", func() bool { return strings.Count(c.textLocked(), "\n") == want+1 })
	}
	for _, c := range clients[1:] {
		if c.text() != clients[0].text() {
			t.Fatal("clients diverged")
		}
	}
	eventually(t, "server", func() bool { return e.serverText("index.md") == clients[0].text() })
}

func TestStateDirInsideDocsIsRefused(t *testing.T) {
	project := newTestWiki(t)
	tm := NewTreeManager(configuration.CurrentConfig.MkDocs)
	if _, err := NewSyncServer(tm, newTestAuthenticator(t, t.TempDir()), filepath.Join(project, "docs", ".state"), nil); err == nil {
		t.Error("state dir inside docs accepted")
	}
}

func TestExcludeFromGit(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".git", "info", "exclude"), []byte("*.tmp"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ExcludeFromGit(project, filepath.Join(project, ".mkdocseditor")); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(project, ".git", "info", "exclude"))
	if string(b) != "*.tmp\n# MkDocsEditor backend state\n/.mkdocseditor/\n" {
		t.Errorf("exclude = %q", b)
	}
}

func TestAuthorship(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")

	// not loaded yet: everything was imported by the server
	rec := e.do(http.MethodGet, "/document/"+id+"/authorship/", "carol")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var au Authorship
	if err := json.Unmarshal(rec.bytes(), &au); err != nil {
		t.Fatal(err)
	}
	if len(au.Runs) != 1 || au.Runs[0].Username != ActorUserServer || au.Runs[0].End != len([]rune(setupText)) {
		t.Fatalf("initial runs: %+v", au.Runs)
	}

	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)
	// computed and kept while loaded, the requests below only advance it
	if rec = e.do(http.MethodGet, "/document/"+id+"/authorship/", "carol"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	a.insertText(0, "Ä😀 ") // 3 code points at the start
	b.waitFor("a's edit", func() bool { return strings.HasPrefix(b.textLocked(), "Ä😀 ") })
	b.appendText("bob\n") // at the end
	a.waitFor("b's edit", func() bool { return strings.HasSuffix(a.textLocked(), "bob\n") })
	b.edit(func(text *automerge.Text) error { return text.Delete(5, 3) }) // delete "Set" of "# Setup"
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == b.text() })

	rec = e.do(http.MethodGet, "/document/"+id+"/authorship/", "carol")
	if err := json.Unmarshal(rec.bytes(), &au); err != nil {
		t.Fatal(err)
	}
	text := []rune(b.text())
	var got []string
	end := 0
	for _, r := range au.Runs {
		if r.Start != end {
			t.Fatalf("gap before %+v", r)
		}
		end = r.End
		got = append(got, r.Username+":"+string(text[r.Start:r.End]))
	}
	if end != len(text) {
		t.Fatalf("runs end at %d, text has %d code points", end, len(text))
	}
	want := []string{"alice:Ä😀 ", ":server:# up\n\nSome text.\n", "bob:bob\n"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("authorship = %q, want %q", got, want)
	}

	// served from the cache while nothing changed
	rec2 := e.do(http.MethodGet, "/document/"+id+"/authorship/", "carol")
	if rec2.Body.String() != rec.Body.String() {
		t.Error("cached authorship differs")
	}

	// the advanced state matches a full replay
	a.insertText(1, "a")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	rec = e.do(http.MethodGet, "/document/"+id+"/authorship/", "carol")
	if err := json.Unmarshal(rec.bytes(), &au); err != nil {
		t.Fatal(err)
	}
	e.ss.mu.Lock()
	session := e.ss.sessions["guide/setup.md"]
	e.ss.mu.Unlock()
	if session == nil {
		t.Fatal("document not loaded")
	}
	session.mu.Lock()
	full, err := computeAuthorship(session.doc, e.ss.actorOwner)
	session.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(au) != fmt.Sprint(*full) {
		t.Errorf("incremental %+v, full %+v", au, *full)
	}
}

// A failed write of the file is tried again, and the document is unloaded afterwards when nobody
// is connected any more
func TestSyncFailedWriteIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only folders")
	}
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	folder := filepath.Join(e.project, "docs", "guide")
	before := e.readFile("guide/setup.md")

	a := newTestClient(t, e, "alice")
	a.connect(id)
	if err := os.Chmod(folder, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o755) })
	a.appendText("more")
	a.flush()
	a.waitFor("the change", func() bool { return e.serverText("guide/setup.md") == before+"more" })
	time.Sleep(3 * e.ss.FlushDelay)
	if got := e.readFile("guide/setup.md"); got != before {
		t.Fatalf("the file changed in a read-only folder: %q", got)
	}
	a.disconnect()

	if err := os.Chmod(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the change on disk", func() bool { return e.readFile("guide/setup.md") == before+"more" })
	eventually(t, "the document to be unloaded", func() bool {
		e.ss.mu.Lock()
		defer e.ss.mu.Unlock()
		return len(e.ss.sessions) == 0
	})
}
