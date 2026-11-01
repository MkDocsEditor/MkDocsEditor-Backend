package backend

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// eventsClient collects the messages of an events websocket
type eventsClient struct {
	t         *testing.T
	ws        *websocket.Conn
	mu        sync.Mutex
	messages  []map[string]any
	closeCode int
	closed    chan struct{}
}

func dialEvents(t *testing.T, e *syncEnv, user string, useTicket bool) *eventsClient {
	t.Helper()
	header := http.Header{}
	hello := map[string]any{"type": "hello", "client": "test"}
	if useTicket {
		ticket, _, err := e.authn.IssueTicket(e.authn.UserByUsername(user), "")
		if err != nil {
			t.Fatal(err)
		}
		hello["ticket"] = ticket
	} else if user != "" {
		header.Set("Authorization", authHeader(t, e.authn, user, testPasswords[user]))
	}
	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.http.URL, "http")+"/events/ws/", header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	c := &eventsClient{t: t, ws: ws, closed: make(chan struct{})}
	if err = ws.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(c.closed)
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				c.mu.Lock()
				if ce, ok := errors.AsType[*websocket.CloseError](err); ok {
					c.closeCode = ce.Code
				}
				c.mu.Unlock()
				return
			}
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			c.mu.Lock()
			c.messages = append(c.messages, m)
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = ws.Close() })
	return c
}

// last returns the newest message of a type, or nil
func (c *eventsClient) last(msgType string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range slices.Backward(c.messages) {
		if v["type"] == msgType {
			return v
		}
	}
	return nil
}

func (c *eventsClient) count(msgType string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.messages {
		if m["type"] == msgType {
			n++
		}
	}
	return n
}

// online returns "docId:user,user;..." of the newest online message
func (c *eventsClient) online() string {
	m := c.last(MsgOnline)
	if m == nil {
		return "<none>"
	}
	var parts []string
	for _, d := range m["documents"].([]any) {
		doc := d.(map[string]any)
		var users []string
		for _, u := range doc["users"].([]any) {
			users = append(users, u.(map[string]any)["username"].(string))
		}
		parts = append(parts, doc["documentId"].(string)+":"+strings.Join(users, ","))
	}
	return strings.Join(parts, ";")
}

func TestEventsOnlineAndTree(t *testing.T) {
	e := newSyncEnv(t)
	id := e.docID("guide/setup.md")
	ev := dialEvents(t, e, "carol", false)
	eventually(t, "snapshot", func() bool { return ev.online() == "" })

	a := newTestClient(t, e, "alice")
	a.connect(id)
	eventually(t, "alice online", func() bool { return ev.online() == id+":alice" })
	b := newTestClient(t, e, "bob")
	b.connect(id)
	b2 := newTestClient(t, e, "bob") // a second device of bob
	b2.connect(id)
	eventually(t, "bob online", func() bool { return ev.online() == id+":alice,bob" })
	a.disconnect()
	eventually(t, "alice left", func() bool { return ev.online() == id+":bob" })
	b.disconnect()
	b2.disconnect()
	eventually(t, "all left", func() bool { return ev.online() == "" })

	guide := e.tm.GenerateID(e.project + "/docs/guide")
	if rec := e.doBody(http.MethodPost, "/document/", "bob", `{"parent":"`+guide+`","name":"new"}`); rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}
	eventually(t, "tree-changed after create", func() bool { return ev.count(MsgTreeChanged) >= 1 })
	n := ev.count(MsgTreeChanged)
	// a change on disk, seen by the file watcher
	e.writeFile("guide/other.md", "# Other\n")
	e.tm.CreateItemTree()
	eventually(t, "tree-changed after disk change", func() bool { return ev.count(MsgTreeChanged) > n })

	// shutdown closes the events socket with 1001
	e.stop()
	<-ev.closed
	if ev.closeCode != CloseGoingAway {
		t.Errorf("close code %d", ev.closeCode)
	}
}

func TestEventsAuth(t *testing.T) {
	e := newSyncEnv(t)
	ev := dialEvents(t, e, "bob", true)
	eventually(t, "snapshot with ticket", func() bool { return ev.last(MsgOnline) != nil })

	anon := dialEvents(t, e, "", false)
	<-anon.closed
	if anon.closeCode != CloseUnauthorized {
		t.Errorf("close code without auth %d", anon.closeCode)
	}
}

func TestEventsHeartbeatTimeout(t *testing.T) {
	e := newSyncEnv(t)
	e.ss.HeartbeatTimeout = 200 * time.Millisecond
	quiet := dialEvents(t, e, "carol", false)
	select {
	case <-quiet.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat timeout")
	}
	if quiet.closeCode != CloseHeartbeatTimout {
		t.Errorf("close code %d", quiet.closeCode)
	}
}
