package collab

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
)

// Events websocket message types, see docs/sync-protocol.md section 6
const (
	MsgTreeChanged = "tree-changed"
	MsgOnline      = "online"
	// MsgAccessRequests is only sent to admins with a session
	MsgAccessRequests = "access-requests"
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

	// AccessRequestsMessage holds the number of pending access requests
	AccessRequestsMessage struct {
		Type    string `json:"type"`
		Pending int    `json:"pending"`
	}
)

const (
	// onlineDebounce collects joins and leaves before the online list is sent
	onlineDebounce = 100 * time.Millisecond
	// maxEventMessageSize limits messages from clients: they only send hello and heartbeats
	maxEventMessageSize = 64 << 10
	// accessRequestCheckInterval notices access requests that expired, which nothing announces
	accessRequestCheckInterval = time.Minute
)

// HandleEventsWebsocket serves GET /events/ws/. user and sessionID are those of the upgrade request
// if it was authenticated: else the hello message has to bring a ticket.
func (s *SyncServer) HandleEventsWebsocket(w http.ResponseWriter, r *http.Request, user *auth.User, sessionID string) error {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil
	}
	ws.SetReadLimit(maxEventMessageSize)
	metrics.WebsocketConnections.WithLabelValues(metrics.ConnectionEvents).Inc()
	conn := newWsConn(s, ws, user, sessionID)
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
	if c.seesAccessRequests() {
		// counted under the lock, so this cannot overwrite a newer number of the worker
		s.eventsMu.Lock()
		if pending, err := s.authenticator.PendingAccessRequests(); err != nil {
			log.Printf("Unable to count the access requests: %v", err)
		} else {
			c.send(&AccessRequestsMessage{Type: MsgAccessRequests, Pending: pending})
		}
		s.eventsMu.Unlock()
	}
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

// seesAccessRequests reports whether a connection may know about access requests: the same
// rule as for the /admin/ endpoints, which need a session of an admin
func (c *wsConn) seesAccessRequests() bool {
	return c.sessionID != "" && c.user != nil && c.user.IsAdmin()
}

// accessRequestsChanged schedules sending the pending access requests. It never blocks.
func (s *SyncServer) accessRequestsChanged() {
	select {
	case s.accessRequests <- struct{}{}:
	default:
	}
}

// broadcastAccessRequests tells the admins the number of pending access requests, if it changed
func (s *SyncServer) broadcastAccessRequests() {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	pending, err := s.authenticator.PendingAccessRequests()
	if err != nil {
		log.Printf("Unable to count the access requests: %v", err)
		return
	}
	if pending == s.lastPending {
		return
	}
	s.lastPending = pending
	for c := range s.eventConns {
		if c.seesAccessRequests() {
			c.send(&AccessRequestsMessage{Type: MsgAccessRequests, Pending: pending})
		}
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
			u := s.currentUser(c.user)
			users = append(users, UserInfo{AccountID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Color: u.Color, Avatar: u.Avatar})
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
	accessRequestCheck := time.NewTicker(accessRequestCheckInterval)
	defer accessRequestCheck.Stop()
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
		case <-s.accessRequests:
			s.broadcastAccessRequests()
		case <-accessRequestCheck.C:
			s.broadcastAccessRequests()
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
