package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki/wikitest"
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
	var actors []collab.ActorInfo
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
			if user == collab.ActorUserDisk {
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
	carol.waitFor("read-only error", func() bool { return len(carol.errors) > 0 && carol.errors[0] == collab.ErrCodeReadOnly })

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

// collabStoreMeta reads the sidecar from the disk, like a restarted server
func collabStoreMeta(t *testing.T, e *syncEnv, rel string) *collab.DocumentMeta {
	t.Helper()
	store, err := collab.NewStateStore(e.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return store.Meta(rel)
}

// A join link has to find the sender also after they left for a messenger
func TestSyncLastPositions(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	alice := e.authn.UserByUsername("alice")

	a := newTestClient(t, e, "alice")
	a.connect(id)
	sel := []map[string]string{{"anchor": "cursor-1", "head": "cursor-1"}}
	before := time.Now()
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": sel})
	time.Sleep(50 * time.Millisecond)
	// the editor loses the focus: the position stays what it was
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{}})
	time.Sleep(50 * time.Millisecond)
	a.disconnect()

	// the document is unloaded when the last connection leaves
	eventually(t, "the document is unloaded", func() bool {
		meta := collabStoreMeta(t, e, "guide/setup.md")
		return meta != nil && len(meta.LastPositions) == 1
	})

	b := newTestClient(t, e, "bob")
	b.connect(id)
	b.waitFor("the snapshot", func() bool { return b.lastPositions != nil })
	if len(b.lastPositions) != 1 {
		t.Fatalf("lastPositions = %v", b.lastPositions)
	}
	p := b.lastPositions[0].(map[string]any)
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(p["lastActiveAt"]))
	if p["accountId"] != alice.ID || p["username"] != "alice" || err != nil || at.Before(before) {
		t.Errorf("position = %v", p)
	}
	selections, ok := p["selections"].([]any)
	if !ok || len(selections) != 1 || selections[0].(map[string]any)["anchor"] != "cursor-1" {
		t.Errorf("selections = %v", p["selections"])
	}
}

// lastLeave waits for another presence-leave and returns it
func lastLeave(c *testClient, count int) map[string]any {
	c.t.Helper()
	c.waitFor("presence-leave", func() bool { return len(c.leaves) >= count })
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaves[count-1]
}

func lastPositionFor(c *testClient, accountID string) map[string]any {
	c.t.Helper()
	c.waitFor("the snapshot", func() bool { return c.lastPositions != nil })
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.lastPositions {
		if m := p.(map[string]any); m["accountId"] == accountID {
			return m
		}
	}
	return nil
}

const (
	instancePhone   = "0123456789abcdef0123456789abcdef"
	instanceBrowser = "fedcba9876543210fedcba9876543210"
)

func connectInstance(t *testing.T, e *syncEnv, user string, id string, instance string, hello ...map[string]any) *testClient {
	t.Helper()
	c := newTestClient(t, e, user)
	h := map[string]any{"instanceId": instance}
	if len(hello) > 0 {
		h = hello[0]
		h["instanceId"] = instance
	}
	c.connect(id, connectOptions{hello: h})
	return c
}

func ghostOf(c *testClient, instance string) map[string]any {
	c.t.Helper()
	c.waitFor("the snapshot", func() bool { return c.lastPositions != nil })
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.ghosts {
		if m := g.(map[string]any); m["instanceId"] == instance {
			return m
		}
	}
	return nil
}

func TestSyncGhost(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	alice := e.authn.UserByUsername("alice").ID
	sel := []map[string]string{{"anchor": "cursor-1", "head": "cursor-1"}}
	view := map[string]string{"top": "cursor-2", "bottom": "cursor-3"}

	watcher := newTestClient(t, e, "carol")
	watcher.connect(id)

	// someone who never had a position leaves no ghost
	bob := newTestClient(t, e, "bob")
	bob.connect(id)
	bob.disconnect()
	if m := lastLeave(watcher, 1); m["ghostMs"] != nil || m["accountId"] != nil {
		t.Errorf("leave without a position = %v", m)
	}

	a := connectInstance(t, e, "alice", id, instancePhone, map[string]any{"instanceColor": "#2b8a3e"})
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": sel, "viewport": view})
	waitPresence(watcher, "the position", selectionAnchor("cursor-1"))
	a.disconnect()
	m := lastLeave(watcher, 2)
	if m["accountId"] != alice || m["instanceId"] != instancePhone || m["ghostMs"] != float64(60000) || m["viewport"] == nil ||
		len(m["selections"].([]any)) != 1 || m["instanceColor"] != "#2b8a3e" || m["deviceName"] == nil {
		t.Errorf("leave = %v", m)
	}

	// a joiner gets the rest of the ghost, and lastPositions has no ghost any more
	joiner := newTestClient(t, e, "bob")
	joiner.connect(id)
	g := ghostOf(joiner, instancePhone)
	if g == nil {
		t.Fatal("no ghost")
	}
	if ms, ok := g["ghostMs"].(float64); !ok || ms <= 0 || ms > 60000 || g["viewport"] == nil || g["accountId"] != alice ||
		g["username"] != "alice" || g["instanceColor"] != "#2b8a3e" || g["deviceName"] == nil || g["color"] == nil {
		t.Errorf("ghost = %v", g)
	}
	if p := lastPositionFor(joiner, alice); p == nil || p["ghostMs"] != nil {
		t.Errorf("position = %v", p)
	}

	// coming back ends the ghost for everybody, and the client itself finds none
	back := connectInstance(t, e, "alice", id, instancePhone)
	if ghostOf(back, instancePhone) != nil {
		t.Error("the returning instance has its own ghost")
	}
	waitPresence(watcher, "the returning instance", func(m map[string]any) bool { return m["instanceId"] == instancePhone })
	late := newTestClient(t, e, "bob")
	late.connect(id)
	if g := ghostOf(late, instancePhone); g != nil {
		t.Errorf("ghost after the return = %v", g)
	}

	// back without a position: no ghost at the old spot
	before := leaveCount(watcher)
	back.disconnect()
	if m := lastLeave(watcher, before+1); m["ghostMs"] != nil {
		t.Errorf("leave without a new position = %v", m)
	}
}

// Every instance behaves the same: another tab or device of the account also gets the ghost, at
// the position of the instance that left
func TestSyncGhostForOwnAccount(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	alice := e.authn.UserByUsername("alice").ID

	// these share an auth session, like two tabs of one browser
	browser := connectInstance(t, e, "alice", id, instanceBrowser)
	phone := connectInstance(t, e, "alice", id, instancePhone)
	third := newTestClient(t, e, "alice")
	third.connect(id)
	phone.send(map[string]any{"type": collab.MsgPresence, "documentId": id,
		"selections": []map[string]string{{"anchor": "phone-1", "head": "phone-1"}},
		"viewport":   map[string]string{"top": "phone-2", "bottom": "phone-3"}})
	waitPresence(browser, "the position of the phone", viewportTop("phone-2"))
	// the app goes to the background and clears its position
	phone.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{}, "viewport": nil})
	browser.waitFor("the cleared position", func() bool {
		for _, m := range browser.presences {
			if viewportTop("phone-2")(m) {
				return false
			}
		}
		return true
	})
	// a follower publishes where it is, which must not move the ghost of the phone
	browser.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{},
		"viewport": map[string]string{"top": "follower-1", "bottom": "follower-2"}})
	waitPresence(phone, "the position of the follower", viewportTop("follower-1"))

	phone.disconnect()
	for _, c := range []*testClient{browser, third} {
		m := lastLeave(c, 1)
		viewport, _ := m["viewport"].(map[string]any)
		selections, _ := m["selections"].([]any)
		if m["accountId"] != alice || m["ghostMs"] != float64(60000) || viewport["top"] != "phone-2" ||
			len(selections) != 1 || selections[0].(map[string]any)["anchor"] != "phone-1" {
			t.Errorf("leave for %v = %v", c.you["clientId"], m)
		}
	}

	// the other instance gets the presence of the returning one
	back := connectInstance(t, e, "alice", id, instancePhone)
	waitPresence(browser, "the phone is back", func(m map[string]any) bool { return m["instanceId"] == instancePhone })
	if ghostOf(back, instanceBrowser) != nil {
		t.Error("a ghost of a connected instance")
	}
}

// A join link opened right after the sender left finds the ghost of the sender's other instance
func TestSyncGhostOfOwnOtherInstanceOnJoin(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")

	watcher := newTestClient(t, e, "bob")
	watcher.connect(id)
	phone := connectInstance(t, e, "alice", id, instancePhone)
	phone.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []map[string]string{{"anchor": "c", "head": "c"}}})
	waitPresence(watcher, "the position", selectionAnchor("c"))
	phone.disconnect()
	lastLeave(watcher, 1)

	browser := connectInstance(t, e, "alice", id, instanceBrowser)
	if g := ghostOf(browser, instancePhone); g == nil || g["ghostMs"] == nil {
		t.Errorf("ghost for the other instance = %v", g)
	}
	reloaded := connectInstance(t, e, "alice", id, instancePhone)
	if g := ghostOf(reloaded, instancePhone); g != nil {
		t.Errorf("own ghost after a reload = %v", g)
	}
}

// A ghost survives the document unloading, and the server restarting
func TestSyncGhostSurvivesUnload(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := connectInstance(t, e, "alice", id, instancePhone)
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []map[string]string{{"anchor": "c", "head": "c"}}})
	a.flush()
	eventually(t, "the position is known", func() bool {
		meta := collabStoreMeta(t, e, "guide/setup.md")
		return meta != nil
	})
	time.Sleep(50 * time.Millisecond)
	a.disconnect()
	eventually(t, "the ghost is saved", func() bool {
		meta := collabStoreMeta(t, e, "guide/setup.md")
		return meta != nil && len(meta.Ghosts) == 1
	})
	e.restart()
	b := newTestClient(t, e, "bob")
	b.connect(id)
	if g := ghostOf(b, instancePhone); g == nil {
		t.Error("no ghost after a restart")
	}
}

func TestSyncGhostExpires(t *testing.T) {
	e := newSyncEnv(t)
	e.ss.GhostLifetime = 30 * time.Millisecond
	id := e.docID("guide/setup.md")
	alice := e.authn.UserByUsername("alice").ID

	a := connectInstance(t, e, "alice", id, instancePhone)
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []map[string]string{{"anchor": "c", "head": "c"}}})
	time.Sleep(50 * time.Millisecond)
	a.disconnect()
	time.Sleep(60 * time.Millisecond)

	b := newTestClient(t, e, "bob")
	b.connect(id)
	if g := ghostOf(b, instancePhone); g != nil {
		t.Errorf("ghost after the lifetime = %v", g)
	}
	if p := lastPositionFor(b, alice); p == nil {
		t.Error("the last position is gone with the ghost")
	}
}

// A second connection of an instance, like a half-open socket after a reconnect, leaves no ghost
func TestSyncNoGhostWithAnotherConnectionOfTheInstance(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	watcher := newTestClient(t, e, "bob")
	watcher.connect(id)
	old := connectInstance(t, e, "alice", id, instancePhone)
	old.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []map[string]string{{"anchor": "c", "head": "c"}}})
	waitPresence(watcher, "the position", selectionAnchor("c"))
	connectInstance(t, e, "alice", id, instancePhone)
	old.disconnect()
	if m := lastLeave(watcher, 1); m["ghostMs"] != nil || m["instanceId"] != instancePhone {
		t.Errorf("leave = %v", m)
	}
}

func TestSyncInstanceFields(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")

	// without an instance id the client id is used, and the client name is the device name
	watcher := newTestClient(t, e, "bob")
	watcher.connect(id)
	plain := newTestClient(t, e, "alice")
	plain.connect(id, connectOptions{useTicket: true})
	if plain.you["instanceId"] != plain.you["clientId"] {
		t.Errorf("you = %v", plain.you)
	}
	waitPresence(watcher, "the client id as instance", func(m map[string]any) bool {
		return m["instanceId"] == plain.you["clientId"] && m["deviceName"] == "test"
	})

	// the name of the auth session comes first
	login, err := e.authn.LoginPassword("alice", testPasswords["alice"], auth.SessionParams{Type: auth.SessionBearer, DeviceName: "Pixel"})
	if err != nil {
		t.Fatal(err)
	}
	named := newTestClient(t, e, "alice")
	named.authorization = "Bearer " + login.Token
	named.connect(id, connectOptions{hello: map[string]any{"instanceId": instancePhone, "instanceColor": "#2b8a3e"}})
	if named.you["instanceId"] != instancePhone {
		t.Errorf("you = %v", named.you)
	}
	waitPresence(watcher, "the instance", func(m map[string]any) bool {
		return m["instanceId"] == instancePhone && m["deviceName"] == "Pixel" && m["instanceColor"] == "#2b8a3e"
	})

	// the color: absent keeps it, a value sets it, null clears it, and color stays the account's
	userColor := e.authn.UserByUsername("alice").Color
	presence := func(color any, set bool) {
		m := map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{}}
		if set {
			m["instanceColor"] = color
		}
		named.send(m)
	}
	hasColor := func(want any) func(map[string]any) bool {
		return func(m map[string]any) bool {
			return m["instanceId"] == instancePhone && m["instanceColor"] == want && m["color"] == userColor
		}
	}
	presence(nil, false)
	presence("#ABCDEF", true)
	waitPresence(watcher, "the new color", hasColor("#ABCDEF"))
	presence(nil, false)
	waitPresence(watcher, "the kept color", hasColor("#ABCDEF"))
	presence(nil, true)
	waitPresence(watcher, "the cleared color", hasColor(nil))
	if got := e.authn.UserByUsername("alice").Color; got != userColor {
		t.Errorf("the account color changed to %s", got)
	}
	for _, bad := range []any{"red", "#12345", "", 5} {
		presence(bad, true)
	}
	named.waitFor("the errors", func() bool { return len(named.errors) == 4 })

	// the format is checked in hello
	for name, hello := range map[string]map[string]any{
		"short":     {"instanceId": "abc"},
		"uppercase": {"instanceId": strings.ToUpper(instancePhone)},
		"color":     {"instanceId": instancePhone, "instanceColor": "green"},
	} {
		c := newTestClient(t, e, "alice")
		if err := c.dial(id, connectOptions{hello: hello}); err == nil {
			if code := c.waitClosed(); code != collab.CloseProtocolError {
				t.Errorf("%s: close code %d", name, code)
			}
		}
	}
}

// An instance id in use by another account is not taken, so nobody can end the ghost of someone else
func TestSyncInstanceOfAnotherAccount(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := connectInstance(t, e, "alice", id, instancePhone)
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []map[string]string{{"anchor": "c", "head": "c"}}})
	other := connectInstance(t, e, "bob", id, instancePhone)
	if other.you["instanceId"] != other.you["clientId"] {
		t.Errorf("connected instance taken: %v", other.you)
	}
	waitPresence(other, "the position", selectionAnchor("c"))
	a.disconnect()
	lastLeave(other, 1)
	thief := connectInstance(t, e, "carol", id, instancePhone)
	if thief.you["instanceId"] != thief.you["clientId"] {
		t.Errorf("ghost instance taken: %v", thief.you)
	}
	if g := ghostOf(thief, instancePhone); g == nil {
		t.Error("the ghost of another account ended")
	}
}

func TestSyncViewport(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)
	sel := []map[string]string{{"anchor": "c", "head": "c"}}

	viewportOf := func() any {
		for _, p := range b.presences {
			if p["username"] == "alice" {
				return p["viewport"]
			}
		}
		return nil
	}

	// only the known fields pass
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": sel,
		"viewport": map[string]string{"top": "t", "bottom": "b", "extra": "x"}})
	b.waitFor("the viewport", func() bool { return viewportOf() != nil })
	b.waitFor("only the known fields", func() bool {
		v, ok := viewportOf().(map[string]any)
		if !ok || len(v) != 2 || v["top"] != "t" || v["bottom"] != "b" {
			t.Errorf("viewport = %v", v)
		}
		return true
	})

	// a missing viewport keeps the last one, also with empty selections
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{}})
	b.waitFor("empty selections", func() bool {
		for _, p := range b.presences {
			if s, ok := p["selections"].([]any); ok && p["username"] == "alice" && len(s) == 0 {
				return true
			}
		}
		return false
	})
	b.waitFor("the kept viewport", func() bool {
		if viewportOf() == nil {
			t.Error("the viewport is gone")
		}
		return true
	})

	// null clears it for peers, the last position keeps it
	a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{}, "viewport": nil})
	b.waitFor("the cleared viewport", func() bool { return viewportOf() == nil })
	c := newTestClient(t, e, "carol")
	c.connect(id)
	p := lastPositionFor(c, e.authn.UserByUsername("alice").ID)
	if p == nil || p["viewport"] == nil || len(p["selections"].([]any)) != 1 || p["ghostMs"] != nil {
		t.Errorf("position = %v", p)
	}

	// invalid ones are refused
	for _, invalid := range []any{"text", map[string]string{"top": "t"}, map[string]any{"top": 1, "bottom": 2}} {
		a.mu.Lock()
		before := len(a.errors)
		a.mu.Unlock()
		a.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": []any{}, "viewport": invalid})
		a.waitFor("the error", func() bool { return len(a.errors) > before })
	}
}

func TestSyncPresence(t *testing.T) {
	start := time.Now()
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	b := newTestClient(t, e, "bob")
	b.connect(id)

	b.waitFor("snapshot with a", func() bool { return len(b.presences) == 1 })
	a.waitFor("b's join", func() bool { return len(a.presences) == 1 })

	// the snapshot and the join carry the account and the connect time
	checkPresence := func(c *testClient, username string, after time.Time) {
		t.Helper()
		c.waitFor(username+" presence", func() bool {
			for _, p := range c.presences {
				at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(p["lastActiveAt"]))
				if p["username"] == username && p["accountId"] == e.authn.UserByUsername(username).ID && err == nil && !at.Before(after) {
					return true
				}
			}
			return false
		})
	}
	checkPresence(b, "alice", start)
	checkPresence(a, "bob", start)

	sel := []map[string]string{{"anchor": "cursor-1", "head": "cursor-2"}}
	// presence fields are stamped by the server, a forged username or time is overwritten
	time.Sleep(5 * time.Millisecond)
	beforeMessage := time.Now()
	b.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "username": "mallory", "lastActiveAt": "2000-01-01T00:00:00Z", "selections": sel})
	checkPresence(a, "bob", beforeMessage)
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

	b.send(map[string]any{"type": collab.MsgPresence, "documentId": id, "selections": "not a list"})
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
	if code := anon.waitClosed(); code != collab.CloseUnauthorized {
		t.Errorf("anonymous close code %d", code)
	}

	// invalid ticket
	bad := newTestClient(t, e, "")
	if err := bad.dial(id, connectOptions{hello: map[string]any{"ticket": "nope"}}); err != nil {
		t.Fatal(err)
	}
	if code := bad.waitClosed(); code != collab.CloseUnauthorized {
		t.Errorf("bad ticket close code %d", code)
	}

	// first message is not hello
	c := newTestClient(t, e, "alice")
	if err := c.dial(id, connectOptions{noHello: true}); err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"type": collab.MsgSyncRequest})
	if code := c.waitClosed(); code != collab.CloseProtocolError {
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
	if code := thief.waitClosed(); code != collab.CloseForbidden {
		t.Errorf("foreign actor close code %d", code)
	}

	// changes of another user's actor inside a sync message
	forger := newTestClient(t, e, "bob")
	forger.connect(id)
	forger.mu.Lock()
	fork, _ := forger.doc.Fork()
	_ = fork.SetActorID(a.actorID)
	_ = fork.Path(collab.ContentKey).Text().Append("forged\n")
	_, _ = fork.Commit("forged")
	_, _ = forger.doc.Merge(fork)
	forger.flush()
	forger.mu.Unlock()
	if code := forger.waitClosed(); code != collab.CloseForbidden {
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
	if code := a.waitClosed(); code != collab.CloseHeartbeatTimout {
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
			if m["type"] == collab.MsgDocumentRenamed {
				renamed = m
			}
		}
		return renamed != nil
	})
	if renamed["newDocumentId"] != newID || renamed["documentId"] != id {
		t.Errorf("renamed = %v", renamed)
	}
	if code := a.waitClosed(); code != collab.CloseRenamed {
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
	if code := a.waitClosed(); code != collab.CloseNotFound {
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
	if code := a.waitClosed(); code != collab.CloseNotFound {
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
	base, err := collab.TextAt(old, serverHeads)
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := collab.ContentText(old)
	a.edit(func(text *automerge.Text) error { return collab.SpliceDiff(text, base, mine) })
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
	project := wikitest.New(t)
	tm := wiki.NewTreeManager(configuration.CurrentConfig.MkDocs)
	if _, err := collab.NewSyncServer(tm, newTestAuthenticator(t, t.TempDir()), filepath.Join(project, "docs", ".state"), nil); err == nil {
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
		if err := collab.ExcludeFromGit(project, filepath.Join(project, ".mkdocseditor")); err != nil {
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
	var au collab.Authorship
	if err := json.Unmarshal(rec.bytes(), &au); err != nil {
		t.Fatal(err)
	}
	if len(au.Runs) != 1 || au.Runs[0].Username != collab.ActorUserServer || au.Runs[0].End != len([]rune(setupText)) {
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
	if e.ss.Stats().LoadedDocuments == 0 {
		t.Fatal("document not loaded")
	}
	full, err := e.ss.ReplayAuthorship(e.tm.GetDocument(id))
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
		return e.ss.Stats().LoadedDocuments == 0
	})
}

// authorshipOf returns the runs of a document as "user:text" and fails on gaps
func authorshipOf(t *testing.T, e *syncEnv, id string, text string) string {
	t.Helper()
	rec := e.do(http.MethodGet, "/document/"+id+"/authorship/", "carol")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var au collab.Authorship
	if err := json.Unmarshal(rec.bytes(), &au); err != nil {
		t.Fatal(err)
	}
	runes := []rune(text)
	var got []string
	end := 0
	for _, r := range au.Runs {
		if r.Start != end || r.End > len(runes) {
			t.Fatalf("run %+v after %d for a text of %d", r, end, len(runes))
		}
		end = r.End
		got = append(got, r.Username+":"+string(runes[r.Start:r.End]))
	}
	if end != len(runes) {
		t.Fatalf("runs end at %d, text has %d code points", end, len(runes))
	}
	return strings.Join(got, "|")
}

func authorshipFile(t *testing.T, e *syncEnv, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(e.stateDir, "documents", dir, "*.authorship"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		return ""
	}
	return matches[0]
}

// A snapshot is written when an edited document unloads, also without any authorship request, and
// survives a restart. Damaged snapshots are ignored and replaced.
func TestAuthorshipSnapshotSurvivesRestartAndDamage(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.insertText(0, "Hello ")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	want := "alice:Hello |:server:" + setupText
	a.disconnect()
	eventually(t, "the snapshot", func() bool { return authorshipFile(t, e, "") != "" })
	text := "Hello " + setupText

	e.restart()
	if got := authorshipOf(t, e, id, text); got != want {
		t.Fatalf("after a restart: %q, want %q", got, want)
	}

	good, err := os.ReadFile(authorshipFile(t, e, ""))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err = json.Unmarshal(good, &snapshot); err != nil {
		t.Fatal(err)
	}
	damage := map[string]func() []byte{
		"not json":       func() []byte { return []byte("{") },
		"truncated file": func() []byte { return nil },
		"old version":    func() []byte { return replaced(t, snapshot, "version", 0) },
		"other epoch":    func() []byte { return replaced(t, snapshot, "epoch", "other") },
		"unknown heads": func() []byte {
			return replaced(t, snapshot, "heads", []string{strings.Repeat("ab", 32)})
		},
		"runs too short": func() []byte {
			return replaced(t, snapshot, "runs", []map[string]any{{"length": 1, "actor": "ff"}})
		},
	}
	for name, content := range damage {
		if err = os.WriteFile(authorshipFile(t, e, ""), content(), 0o644); err != nil {
			t.Fatal(err)
		}
		e.restart()
		if got := authorshipOf(t, e, id, text); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
		eventually(t, "the new snapshot after "+name, func() bool {
			now, err := os.ReadFile(authorshipFile(t, e, ""))
			return err == nil && bytes.Equal(now, good)
		})
	}
}

func replaced(t *testing.T, snapshot map[string]any, key string, value any) []byte {
	t.Helper()
	changed := maps.Clone(snapshot)
	changed[key] = value
	b, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Changes made after the snapshot are applied on top of it
func TestAuthorshipContinuesFromSnapshot(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.insertText(0, "A ")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	a.disconnect()
	eventually(t, "the snapshot", func() bool { return authorshipFile(t, e, "") != "" })
	first, err := os.ReadFile(authorshipFile(t, e, ""))
	if err != nil {
		t.Fatal(err)
	}

	e.restart()
	b := newTestClient(t, e, "bob")
	b.connect(id)
	b.appendText("B\n")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == b.text() })
	want := "alice:A |:server:" + setupText + "|bob:B\n"
	if got := authorshipOf(t, e, id, b.text()); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// and the concurrent requests of a new change share a job and agree
	b.insertText(0, "C ")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == b.text() })
	want = "bob:C |alice:A |:server:" + setupText + "|bob:B\n"
	text := b.text()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := authorshipOf(t, e, id, text); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
	b.disconnect()
	eventually(t, "a newer snapshot", func() bool {
		now, err := os.ReadFile(authorshipFile(t, e, ""))
		return err == nil && !bytes.Equal(now, first)
	})
}

// The snapshot goes with the document when it is moved, deleted and restored
func TestAuthorshipSnapshotFollowsTheDocument(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.insertText(0, "Hello ")
	eventually(t, "server", func() bool { return e.serverText("guide/setup.md") == a.text() })
	a.disconnect()
	eventually(t, "the snapshot", func() bool { return authorshipFile(t, e, "") != "" })
	text := "Hello " + setupText
	want := "alice:Hello |:server:" + setupText

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
	id = e.docID("handbook/setup.md")
	before, err := os.ReadFile(authorshipFile(t, e, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := authorshipOf(t, e, id, text); got != want {
		t.Errorf("renamed: %q, want %q", got, want)
	}
	if now, err := os.ReadFile(authorshipFile(t, e, "")); err != nil || !bytes.Equal(now, before) {
		t.Errorf("the snapshot changed after the rename: %v", err)
	}

	rec := e.do(http.MethodDelete, "/document/"+id+"/", "alice")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d: %s", rec.Code, rec.Body.String())
	}
	eventually(t, "the archived snapshot", func() bool {
		return authorshipFile(t, e, "") == "" && authorshipFile(t, e, "deleted") != ""
	})

	e.writeFile("handbook/setup.md", text)
	e.tm.CreateItemTree()
	if got := authorshipOf(t, e, id, text); got != want {
		t.Errorf("restored: %q, want %q", got, want)
	}
	if authorshipFile(t, e, "") == "" || authorshipFile(t, e, "deleted") != "" {
		t.Error("the snapshot was not restored")
	}
}

func viewportTop(top string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		v, _ := m["viewport"].(map[string]any)
		return v["top"] == top
	}
}

func selectionAnchor(anchor string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		s, _ := m["selections"].([]any)
		return len(s) > 0 && s[0].(map[string]any)["anchor"] == anchor
	}
}

func waitPresence(c *testClient, what string, match func(map[string]any) bool) {
	c.t.Helper()
	c.waitFor(what, func() bool {
		for _, m := range c.presences {
			if match(m) {
				return true
			}
		}
		return false
	})
}

func leaveCount(c *testClient) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.leaves)
}
