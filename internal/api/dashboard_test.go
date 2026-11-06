package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

func (e *syncEnv) pins(method string, path string, user string, wantStatus int) *PinsResponse {
	e.t.Helper()
	rec := e.do(method, path, user)
	if rec.Code != wantStatus {
		e.t.Fatalf("%s %s as %s: %d %s", method, path, user, rec.Code, rec.Body.String())
	}
	if wantStatus != http.StatusOK {
		return nil
	}
	var pins PinsResponse
	if err := json.Unmarshal(rec.bytes(), &pins); err != nil {
		e.t.Fatal(err)
	}
	return &pins
}

func (e *syncEnv) feed(user string, query string) *FeedResponse {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/feed/"+query, user)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("feed: %d %s", rec.Code, rec.Body.String())
	}
	var feed FeedResponse
	if err := json.Unmarshal(rec.bytes(), &feed); err != nil {
		e.t.Fatal(err)
	}
	return &feed
}

func pinPaths(pins []PinResponse) string {
	var out []string
	for _, p := range pins {
		out = append(out, p.Path)
	}
	return strings.Join(out, ",")
}

func TestPinsRolesAndEvents(t *testing.T) {
	e := newSyncEnv(t)
	setup := e.docID("guide/setup.md")
	index := e.docID("index.md")
	carolEvents := dialEvents(t, e, "carol", false)
	bobEvents := dialEvents(t, e, "bob", false)

	pins := e.pins(http.MethodPut, "/pins/family/"+setup+"/", "bob", http.StatusOK)
	if len(pins.Family) != 1 || pins.Family[0].DocumentID != setup || pins.Family[0].Name != "setup" ||
		pins.Family[0].Path != "guide/setup.md" || pins.Family[0].PinnedBy.Username != "bob" || pins.Family[0].PinnedAt == "" {
		t.Fatalf("pins = %+v", pins)
	}
	eventually(t, "pins-changed for everyone", func() bool {
		return carolEvents.count(collab.MsgPinsChanged) == 1 && bobEvents.count(collab.MsgPinsChanged) == 1
	})
	// idempotent, no event
	e.pins(http.MethodPut, "/pins/family/"+setup+"/", "alice", http.StatusOK)

	// readers have personal pins, not family pins
	e.pins(http.MethodPut, "/pins/family/"+index+"/", "carol", http.StatusForbidden)
	pins = e.pins(http.MethodPut, "/pins/personal/"+index+"/", "carol", http.StatusOK)
	if pinPaths(pins.Family) != "guide/setup.md" || pinPaths(pins.Personal) != "index.md" {
		t.Fatalf("pins of carol = %+v", pins)
	}
	eventually(t, "pins-changed for carol", func() bool { return carolEvents.count(collab.MsgPinsChanged) == 2 })
	if pins := e.pins(http.MethodGet, "/pins/", "bob", http.StatusOK); pinPaths(pins.Personal) != "" {
		t.Errorf("bob sees personal pins of carol: %+v", pins)
	}
	e.pins(http.MethodPut, "/pins/personal/nope/", "carol", http.StatusNotFound)

	// pins follow a move of their section
	e.postMove("alice", collab.MoveRequest{Type: wiki.TypeSection, ID: e.docID("guide"), Name: "manual"})
	pins = e.pins(http.MethodGet, "/pins/", "carol", http.StatusOK)
	if pinPaths(pins.Family) != "manual/setup.md" || pins.Family[0].DocumentID != e.docID("manual/setup.md") {
		t.Fatalf("after the move: %+v", pins)
	}
	eventually(t, "pins-changed after the move", func() bool {
		return carolEvents.count(collab.MsgPinsChanged) == 3 && bobEvents.count(collab.MsgPinsChanged) == 2
	})

	// a delete removes them
	e.pins(http.MethodPut, "/pins/personal/"+e.docID("manual/setup.md")+"/", "carol", http.StatusOK)
	if rec := e.do(http.MethodDelete, "/section/"+e.docID("manual")+"/", "alice"); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	pins = e.pins(http.MethodGet, "/pins/", "carol", http.StatusOK)
	if pinPaths(pins.Family) != "" || pinPaths(pins.Personal) != "index.md" {
		t.Fatalf("after the delete: %+v", pins)
	}

	pins = e.pins(http.MethodDelete, "/pins/personal/"+index+"/", "carol", http.StatusOK)
	if len(pins.Personal) != 0 {
		t.Errorf("after the unpin: %+v", pins)
	}
}

func TestFeedRecordsEditsAndItems(t *testing.T) {
	e := newSyncEnv(t)
	setup := e.docID("guide/setup.md")
	ev := dialEvents(t, e, "carol", false)

	a := newTestClient(t, e, "alice")
	a.connect(setup)
	a.appendText("\n## Install\n\nRun it.\n")
	b := newTestClient(t, e, "bob")
	b.connect(setup)
	b.waitFor("alice's text", func() bool { return strings.Contains(b.textLocked(), "Run it.") })
	b.appendText("Twice.\n")

	var feed *FeedResponse
	eventually(t, "one edit of alice and bob", func() bool {
		feed = e.feed("carol", "")
		if len(feed.Entries) != 1 || len(feed.Entries[0].Authors) != 2 {
			return false
		}
		return feed.Entries[0].Summary != nil && feed.Entries[0].Summary.LinesAdded == 5
	})
	entry := feed.Entries[0]
	if entry.Kind != "edited" || entry.ItemType != "document" || entry.DocumentID != setup || entry.Path != "guide/setup.md" ||
		entry.Authors[0].Username != "alice" || entry.Authors[1].Username != "bob" || entry.Authors[0].AccountID == "" ||
		strings.Join(entry.Summary.Headings, ",") != "Setup,Install" || entry.OldPath != "" || feed.Next != nil {
		t.Errorf("entry = %+v %+v", entry, entry.Summary)
	}
	eventually(t, "feed-changed", func() bool { return ev.count(collab.MsgFeedChanged) >= 1 })

	// a change of the file while the document is open joins the editing session
	e.writeFile("guide/setup.md", e.readFile("guide/setup.md")+"From outside.\n")
	e.ss.CheckDisk()
	eventually(t, "the external change", func() bool {
		feed = e.feed("carol", "")
		return len(feed.Entries) == 1 && len(feed.Entries[0].Authors) == 3 && feed.Entries[0].Summary.LinesAdded == 6
	})
	if ext := feed.Entries[0].Authors[2]; ext.DisplayName != "External change" || ext.AccountID != "" || ext.Username != "" || ext.Avatar != nil || ext.Color != pseudoAuthorColor {
		t.Errorf("external author = %+v", ext)
	}
	a.disconnect()
	b.disconnect()

	// created, moved and deleted, newest first
	rec := e.doBody(http.MethodPost, "/document/", "bob", `{"parent":"root","name":"notes"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	e.postMove("bob", collab.MoveRequest{Type: wiki.TypeDocument, ID: e.docID("notes.md"), Name: "todo"})
	if rec := e.do(http.MethodDelete, "/document/"+e.docID("todo.md")+"/", "alice"); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	var kinds string
	eventually(t, "the item entries", func() bool {
		feed = e.feed("carol", "")
		var k []string
		for _, entry := range feed.Entries {
			k = append(k, entry.Kind+" "+entry.Path)
		}
		kinds = strings.Join(k, ", ")
		return len(feed.Entries) == 4
	})
	if kinds != "deleted todo.md, moved todo.md, created notes.md, edited guide/setup.md" {
		t.Errorf("feed = %s", kinds)
	}
	if moved := feed.Entries[1]; moved.OldPath != "notes.md" || moved.Authors[0].Username != "bob" || moved.DocumentID != "" || moved.Summary != nil {
		t.Errorf("moved = %+v", moved)
	}

	// pages
	page := e.feed("carol", "?limit=2")
	if len(page.Entries) != 2 || page.Next == nil {
		t.Fatalf("page = %+v", page)
	}
	rest := e.feed("carol", "?limit=200&before="+*page.Next)
	if len(rest.Entries) != 2 || rest.Next != nil || rest.Entries[0].ID != feed.Entries[2].ID {
		t.Errorf("rest = %+v", rest)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x", "?before=%21"} {
		if rec := e.do(http.MethodGet, "/feed/"+q, "carol"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}

// Reference updates of a move are no edits
func TestFeedLeavesOutReferenceUpdates(t *testing.T) {
	e := newSyncEnv(t)
	e.withLinks()
	e.postMove("alice", collab.MoveRequest{Type: wiki.TypeDocument, ID: e.docID("guide/setup.md"), Name: "install"})
	e.ss.Activity().Wait()
	feed := e.feed("carol", "")
	if len(feed.Entries) != 1 || feed.Entries[0].Kind != "moved" {
		t.Errorf("feed = %+v", feed.Entries)
	}
	if !strings.Contains(e.readFile("index.md"), "guide/install.md") {
		t.Error("the reference was not updated")
	}
}
