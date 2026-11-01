package backend

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/labstack/echo/v4"
)

// Events websocket message types, see docs/sync-protocol.md section 6
const (
	MsgTreeChanged = "tree-changed"
	MsgOnline      = "online"
)

type (
	// OnlineMessage lists who has which document open
	OnlineMessage struct {
		Type      string           `json:"type"`
		Documents []OnlineDocument `json:"documents"`
	}

	OnlineDocument struct {
		DocumentID string     `json:"documentId"`
		Users      []UserInfo `json:"users"`
	}
)

const (
	// onlineDebounce collects joins and leaves before the online list is sent
	onlineDebounce = 100 * time.Millisecond
	// maxEventMessageSize limits messages from clients: they only send hello and heartbeats
	maxEventMessageSize = 64 << 10
)

// HandleEventsWebsocket serves GET /events/ws/
func (s *SyncServer) HandleEventsWebsocket(c echo.Context) error {
	ws, err := s.upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return nil
	}
	ws.SetReadLimit(maxEventMessageSize)
	conn := newWsConn(s, ws, UserFromContext(c), sessionIDFromContext(c))
	go conn.writeLoop()
	conn.runEvents()
	return nil
}

// runEvents handles an events connection until it closes
func (c *wsConn) runEvents() {
	s := c.server
	defer func() {
		s.eventsMu.Lock()
		delete(s.eventConns, c)
		s.eventsMu.Unlock()
		c.shutdown()
	}()

	hello := c.readHello()
	if hello == nil || !c.authenticate(hello) {
		return
	}

	s.eventsMu.Lock()
	if s.eventsClosed {
		s.eventsMu.Unlock()
		c.close(CloseGoingAway, "server shutting down")
		return
	}
	s.eventConns[c] = struct{}{}
	s.eventsMu.Unlock()
	c.send(s.onlineSnapshot())
	log.Printf("%s: %s (%s) subscribed to events", c.clientID, c.user.Username, hello.Client)

	for {
		data, ok := c.readMessage()
		if !ok {
			return
		}
		// heartbeats keep the connection alive, other types are ignored
		var base MessageBase
		if err := json.Unmarshal(data, &base); err != nil {
			c.fail(CloseProtocolError, ErrCodeBadRequest, "invalid JSON")
			return
		}
	}
}

// broadcastEvent sends a message to every events connection
func (s *SyncServer) broadcastEvent(msg any) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	for c := range s.eventConns {
		c.send(msg)
	}
}

// onlineChanged schedules sending the online list. It never blocks.
func (s *SyncServer) onlineChanged() {
	select {
	case s.onlineChanges <- struct{}{}:
	default:
	}
}

// onlineSnapshot lists the users connected to each loaded document
func (s *SyncServer) onlineSnapshot() *OnlineMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := &OnlineMessage{Type: MsgOnline, Documents: []OnlineDocument{}}
	for _, session := range s.sessions {
		session.mu.Lock()
		seen := map[string]bool{}
		var users []UserInfo
		for c := range session.conns {
			if seen[c.user.ID] {
				continue
			}
			seen[c.user.ID] = true
			users = append(users, UserInfo{AccountID: c.user.ID, Username: c.user.Username, DisplayName: c.user.DisplayName, Color: c.user.Color})
		}
		docID := session.docID
		session.mu.Unlock()
		if len(users) == 0 {
			continue
		}
		sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
		msg.Documents = append(msg.Documents, OnlineDocument{DocumentID: docID, Users: users})
	}
	sort.Slice(msg.Documents, func(i, j int) bool { return msg.Documents[i].DocumentID < msg.Documents[j].DocumentID })
	return msg
}

// eventsWorker sends tree changes and the online list to the events connections
func (s *SyncServer) eventsWorker() {
	defer close(s.eventsDone)
	var timer <-chan time.Time
	for {
		select {
		case <-s.stop:
			s.eventsMu.Lock()
			s.eventsClosed = true
			for c := range s.eventConns {
				c.close(CloseGoingAway, "server shutting down")
			}
			s.eventsMu.Unlock()
			return
		case <-s.treeEvents:
			s.broadcastEvent(&MessageBase{Type: MsgTreeChanged})
		case <-s.onlineChanges:
			if timer == nil {
				timer = time.After(onlineDebounce)
			}
		case <-timer:
			timer = nil
			s.broadcastEvent(s.onlineSnapshot())
		}
	}
}
