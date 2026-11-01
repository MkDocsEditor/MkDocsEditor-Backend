package backend

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	mutexSync "sync"
	"sync/atomic"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	automerge "github.com/automerge/automerge-go"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

type sessionMutex = mutexSync.Mutex

// ErrDocumentInUse is returned when a document that clients are connected to should be deleted
var ErrDocumentInUse = errors.New("there are still clients connected to the document")

const (
	maxMessageSize = 64 << 20
	sendQueueSize  = 256
)

// SyncServer serves the document websocket: Automerge sync, presence and the persistence of
// the documents. See docs/sync-protocol.md.
type SyncServer struct {
	treeManager   *TreeManager
	store         *StateStore
	redirects     *RedirectStore
	actors        *ActorRegistry
	authenticator *auth.Service
	upgrader      websocket.Upgrader

	// timings, can be shortened in tests
	FlushDelay       time.Duration
	HeartbeatPeriod  time.Duration
	HeartbeatTimeout time.Duration
	HelloTimeout     time.Duration

	// OnFileWritten is called (with the session lock held) after the server wrote a document file
	OnFileWritten func(relPath string)

	// changeRecorder is told which users changed which document (the git committer)
	changeRecorder ChangeRecorder

	mu sessionMutex
	// loaded documents by path relative to the docs folder
	sessions map[string]*docSession
	closed   bool

	clientCounter atomic.Uint64
	treeChanges   chan struct{}
	stop          chan struct{}
	workerDone    chan struct{}

	// events websocket
	eventsMu      sessionMutex
	eventConns    map[*wsConn]struct{}
	eventsClosed  bool
	treeEvents    chan struct{}
	onlineChanges chan struct{}
	eventsDone    chan struct{}
}

// NewSyncServer creates the sync server. stateDir holds the Automerge state.
func NewSyncServer(treeManager *TreeManager, authenticator *auth.Service, stateDir string, allowedOrigins []string) (*SyncServer, error) {
	if err := checkStateDir(stateDir, treeManager.RootPath()); err != nil {
		return nil, err
	}
	store, err := NewStateStore(stateDir)
	if err != nil {
		return nil, err
	}
	actors, err := NewActorRegistry(authenticator)
	if err != nil {
		return nil, err
	}
	redirects, err := NewRedirectStore(stateDir, func(rel string) string {
		return treeManager.GenerateId(filepath.Join(treeManager.RootPath(), filepath.FromSlash(rel)))
	})
	if err != nil {
		return nil, err
	}
	s := &SyncServer{
		treeManager:   treeManager,
		store:         store,
		redirects:     redirects,
		actors:        actors,
		authenticator: authenticator,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     checkOrigin(allowedOrigins),
		},
		FlushDelay:       2 * time.Second,
		HeartbeatPeriod:  5 * time.Second,
		HeartbeatTimeout: 15 * time.Second,
		HelloTimeout:     10 * time.Second,
		sessions:         map[string]*docSession{},
		treeChanges:      make(chan struct{}, 1),
		stop:             make(chan struct{}),
		workerDone:       make(chan struct{}),
		eventConns:       map[*wsConn]struct{}{},
		treeEvents:       make(chan struct{}, 1),
		onlineChanges:    make(chan struct{}, 1),
		eventsDone:       make(chan struct{}),
	}
	treeManager.AddTreeChangeListener(func(_, _ *Section) {
		select {
		case s.treeChanges <- struct{}{}:
		default:
		}
		select {
		case s.treeEvents <- struct{}{}:
		default:
		}
	})
	go s.treeWorker()
	go s.eventsWorker()
	authenticator.OnRevoke(s.onRevoke)
	return s, nil
}

// sessionIDFromContext returns the id of the session that authenticated a request, or ""
func sessionIDFromContext(c echo.Context) string {
	if sess := SessionFromContext(c); sess != nil {
		return sess.ID
	}
	return ""
}

// onRevoke closes the websockets of ended sessions, disabled accounts and changed roles
func (s *SyncServer) onRevoke(e auth.RevokeEvent) {
	match := func(c *wsConn) bool {
		if c.user == nil || c.user.ID != e.AccountID {
			return false
		}
		switch {
		case e.SessionID != "":
			return c.sessionID == e.SessionID
		case e.ExceptSessionID != "":
			return c.sessionID != e.ExceptSessionID
		}
		return true
	}
	code, reason := CloseUnauthorized, "session ended"
	switch e.Reason {
	case auth.RevokeDisabled:
		reason = "account disabled"
	case auth.RevokeRole:
		code, reason = CloseForbidden, "role changed"
	}
	var conns []*wsConn
	s.mu.Lock()
	for _, session := range s.sessions {
		session.mu.Lock()
		for c := range session.conns {
			if match(c) {
				conns = append(conns, c)
			}
		}
		session.mu.Unlock()
	}
	s.mu.Unlock()
	s.eventsMu.Lock()
	for c := range s.eventConns {
		if match(c) {
			conns = append(conns, c)
		}
	}
	s.eventsMu.Unlock()
	for _, c := range conns {
		c.fail(code, ErrCodeUnauthorized, reason)
	}
}

// checkOrigin allows websocket upgrades from the configured CORS origins and from clients
// that send no Origin header (native apps). Without configured origins every origin is allowed.
func checkOrigin(allowedOrigins []string) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" || len(allowedOrigins) == 0 {
			return true
		}
		for _, allowed := range allowedOrigins {
			if allowed == "*" || strings.EqualFold(allowed, origin) {
				return true
			}
		}
		return false
	}
}

func (s *SyncServer) relPath(absPath string) (string, error) {
	rel, err := filepath.Rel(s.treeManager.RootPath(), absPath)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is outside the docs folder", absPath)
	}
	return filepath.ToSlash(rel), nil
}

// ChangeRecorder is told about the changes users make to documents. RecordChanges is called with
// the session lock held, so it must not block or call back into the sync server.
type ChangeRecorder interface {
	RecordChanges(relPath string, countsByUser map[string]int)
}

// SetChangeRecorder sets the recorder of user changes. Call it before serving requests.
func (s *SyncServer) SetChangeRecorder(r ChangeRecorder) {
	s.changeRecorder = r
}

// FlushPath writes a loaded document (path relative to the docs folder) to disk now
func (s *SyncServer) FlushPath(relPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[relPath]; session != nil {
		session.mu.Lock()
		if !session.closed {
			session.flushLocked()
		}
		session.mu.Unlock()
	}
}

// FlushAll writes all loaded documents to disk now
func (s *SyncServer) FlushAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		session.mu.Lock()
		if !session.closed {
			session.flushLocked()
		}
		session.mu.Unlock()
	}
}

// HasConnections reports whether clients are connected to a document (path relative to the docs folder)
func (s *SyncServer) HasConnections(relPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[relPath]
	if session == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return len(session.conns) > 0
}

func (s *SyncServer) notifyWritten(relPath string) {
	if s.OnFileWritten != nil {
		s.OnFileWritten(relPath)
	}
}

// actorInfo describes the user of an actor. subject is an account id or a pseudo user.
func (s *SyncServer) actorInfo(actorID string, subject string) ActorInfo {
	info := ActorInfo{ActorID: actorID, Username: subject, DisplayName: subject, Color: "#868e96"}
	switch subject {
	case ActorUserDisk:
		info.DisplayName = "External change"
	case ActorUserServer:
		info.DisplayName = "Server"
	default:
		if u := s.authenticator.UserByID(subject); u != nil {
			info.AccountID = u.ID
			info.Username = u.Username
			info.DisplayName = u.DisplayName
			info.Color = u.Color
		} else {
			info.Color = auth.ColorForUsername(subject)
		}
	}
	return info
}

// subjectUsername returns the current username of a subject (account id or pseudo user)
func (s *SyncServer) subjectUsername(subject string) (username string, accountID string) {
	if strings.HasPrefix(subject, ":") || subject == "" {
		return subject, ""
	}
	if u := s.authenticator.UserByID(subject); u != nil {
		return u.Username, u.ID
	}
	return subject, ""
}

// Actors returns the actor map
func (s *SyncServer) Actors() []ActorInfo {
	all := s.actors.All()
	out := make([]ActorInfo, 0, len(all))
	for _, id := range SortedActorIDs(all) {
		out = append(out, s.actorInfo(id, all[id].Subject))
	}
	return out
}

// CurrentText returns the live text of a loaded document. ok is false if it is not loaded.
func (s *SyncServer) CurrentText(d *Document) (string, bool) {
	rel, err := s.relPath(d.Path)
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	session := s.sessions[rel]
	s.mu.Unlock()
	if session == nil {
		return "", false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return "", false
	}
	text, err := session.text()
	return text, err == nil
}

// IsClientConnected reports whether clients are connected to the document at absPath or below it
func (s *SyncServer) IsClientConnected(absPath string) bool {
	rel, err := s.relPath(absPath)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, session := range s.sessions {
		if _, ok := movePath(p, rel, rel); ok || rel == "." {
			session.mu.Lock()
			n := len(session.conns)
			session.mu.Unlock()
			if n > 0 {
				return true
			}
		}
	}
	return false
}

// IsItemBeingEditedRecursive returns an error if a document in the section has connected clients
func (s *SyncServer) IsItemBeingEditedRecursive(section *Section) error {
	if s.IsClientConnected(section.Path) {
		return errors.New("a document within this section is currently being edited by another user")
	}
	return nil
}

// DeleteItem runs the deletion of the file or folder at absPath. Without force it refuses with
// ErrDocumentInUse while clients are connected to an affected document.
func (s *SyncServer) DeleteItem(absPath string, force bool, remove func() error) error {
	rel, err := s.relPath(absPath)
	if err != nil {
		return err
	}
	if !force && s.IsClientConnected(absPath) {
		return ErrDocumentInUse
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	affected := map[string]*docSession{}
	for p, session := range s.sessions {
		if _, ok := movePath(p, rel, rel); ok {
			affected[p] = session
			session.mu.Lock()
			session.flushLocked()
			session.mu.Unlock()
		}
	}
	if err = remove(); err != nil {
		return err
	}
	for p, session := range affected {
		s.closeDeletedLocked(p, session)
	}
	if err = s.store.Archive(rel); err != nil {
		log.Printf("Unable to archive the state of %s: %v", rel, err)
	}
	return nil
}

// closeDeletedLocked tells the clients of a deleted document and unloads it. Caller holds s.mu.
func (s *SyncServer) closeDeletedLocked(p string, session *docSession) {
	session.mu.Lock()
	for c := range session.conns {
		c.send(&MessageBase{Type: MsgDocumentDeleted, DocumentID: session.docID})
		c.close(CloseNotFound, "document deleted")
	}
	session.closed = true
	if session.flushTimer != nil {
		session.flushTimer.Stop()
	}
	session.mu.Unlock()
	delete(s.sessions, p)
}

// treeWorker reacts to tree rebuilds (file watcher, REST changes): it merges external changes of
// loaded documents and handles documents deleted on disk.
func (s *SyncServer) treeWorker() {
	defer close(s.workerDone)
	for {
		select {
		case <-s.stop:
			return
		case <-s.treeChanges:
			s.CheckDisk()
		}
	}
}

// CheckDisk compares every loaded document with its file
func (s *SyncServer) CheckDisk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, session := range s.sessions {
		session.mu.Lock()
		if session.closed {
			session.mu.Unlock()
			continue
		}
		exists, err := session.checkDiskLocked()
		session.mu.Unlock()
		if err != nil {
			log.Printf("Unable to check %s for external changes: %v", p, err)
		}
		if !exists {
			log.Printf("Document %s was deleted on disk", p)
			s.closeDeletedLocked(p, session)
			if err = s.store.Archive(p); err != nil {
				log.Printf("Unable to archive the state of %s: %v", p, err)
			}
		}
	}
}

// Close flushes all documents and disconnects all clients
func (s *SyncServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for p, session := range s.sessions {
		session.mu.Lock()
		session.flushLocked()
		session.closed = true
		for c := range session.conns {
			c.close(CloseGoingAway, "server shutting down")
		}
		session.mu.Unlock()
		delete(s.sessions, p)
	}
	s.mu.Unlock()
	close(s.stop)
	<-s.workerDone
	<-s.eventsDone
}

// HandleWebsocket serves GET /document/{id}/ws/
func (s *SyncServer) HandleWebsocket(c echo.Context, documentID string) error {
	if s.treeManager.GetDocument(documentID) == nil {
		// an old id is upgraded, so the client learns the new id after hello
		if item, _, ok := s.resolveRedirect(documentID, ""); !ok || item.Type != TypeDocument {
			return echo.ErrNotFound
		}
	}
	ws, err := s.upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		// the upgrader already wrote an error response
		return nil
	}
	ws.SetReadLimit(maxMessageSize)

	conn := newWsConn(s, ws, UserFromContext(c), sessionIDFromContext(c))
	go conn.writeLoop()
	conn.run(documentID)
	return nil
}

// join adds an authenticated connection to the session of its document, loading it if needed
func (s *SyncServer) join(conn *wsConn, d *Document) (*docSession, error) {
	rel, err := s.relPath(d.Path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("server is shutting down")
	}
	session := s.sessions[rel]
	if session == nil {
		session, err = s.loadSession(rel, d.Path, d.ID)
		if err != nil {
			return nil, err
		}
		s.sessions[rel] = session
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err = conn.attachLocked(session); err != nil {
		return nil, err
	}
	s.onlineChanged()
	return session, nil
}

// withSession runs f with the session of a document, holding its lock. A document that is not
// loaded is loaded for the call and unloaded again afterwards.
func (s *SyncServer) withSession(d *Document, f func(session *docSession) error) error {
	rel, err := s.relPath(d.Path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server is shutting down")
	}
	session := s.sessions[rel]
	temporary := session == nil
	if temporary {
		session, err = s.loadSession(rel, d.Path, d.ID)
		if err != nil {
			return err
		}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	err = f(session)
	if temporary {
		session.flushLocked()
		if session.dirty {
			// keep it loaded until the flush succeeds
			s.sessions[rel] = session
			session.scheduleFlushLocked()
		} else {
			session.closed = true
		}
	}
	return err
}

// leave removes a connection and unloads the document when it was the last one
func (s *SyncServer) leave(conn *wsConn) {
	session := conn.session
	if session == nil {
		return
	}
	session.mu.Lock()
	if _, ok := session.conns[conn]; !ok {
		session.mu.Unlock()
		return
	}
	delete(session.conns, conn)
	s.onlineChanged()
	session.broadcastLocked(&PresenceLeaveMessage{Type: MsgPresenceLeave, DocumentID: session.docID, ClientID: conn.clientID}, nil)
	remaining := len(session.conns)
	session.mu.Unlock()
	if remaining > 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.conns) > 0 || session.closed {
		return
	}
	session.flushLocked()
	if session.dirty {
		// keep it loaded so nothing is lost; the next flush retries
		session.scheduleFlushLocked()
		return
	}
	session.closed = true
	if s.sessions[session.meta.Path] == session {
		delete(s.sessions, session.meta.Path)
	}
}

// wsConn is one websocket connection to a document
type wsConn struct {
	server   *SyncServer
	ws       *websocket.Conn
	user     *auth.User
	clientID string
	// sessionID is the session the connection authenticated with, if any. user and
	// sessionID are set before the connection is added to a session or the events list.
	sessionID string

	outgoing   chan outgoingMessage
	closeOnce  mutexSync.Once
	done       chan struct{}
	writerDone chan struct{}

	// guarded by session.mu
	session    *docSession
	actorID    string
	syncState  *automerge.SyncState
	shadow     *automerge.Doc
	selections json.RawMessage
	warnedRO   bool
}

type outgoingMessage struct {
	data      []byte
	closeCode int
	reason    string
}

func newWsConn(s *SyncServer, ws *websocket.Conn, user *auth.User, sessionID string) *wsConn {
	return &wsConn{
		server:     s,
		ws:         ws,
		user:       user,
		sessionID:  sessionID,
		clientID:   fmt.Sprintf("c-%d", s.clientCounter.Add(1)),
		outgoing:   make(chan outgoingMessage, sendQueueSize),
		done:       make(chan struct{}),
		writerDone: make(chan struct{}),
	}
}

// send queues a message. A client that does not keep up is disconnected.
func (c *wsConn) send(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Unable to encode websocket message: %v", err)
		return
	}
	select {
	case <-c.done:
	case c.outgoing <- outgoingMessage{data: data}:
	default:
		log.Printf("%s: send queue full, disconnecting", c.clientID)
		c.terminate()
	}
}

// close sends a close frame after the queued messages
func (c *wsConn) close(code int, reason string) {
	select {
	case <-c.done:
	case c.outgoing <- outgoingMessage{closeCode: code, reason: reason}:
	default:
		c.terminate()
	}
}

func (c *wsConn) terminate() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

func (c *wsConn) writeLoop() {
	defer close(c.writerDone)
	ticker := time.NewTicker(c.server.HeartbeatPeriod)
	defer ticker.Stop()
	heartbeat, _ := json.Marshal(&MessageBase{Type: MsgHeartbeat})
	lastWrite := time.Now()
	for {
		select {
		case <-c.done:
			return
		case m := <-c.outgoing:
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if m.closeCode != 0 {
				_ = c.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(m.closeCode, m.reason))
				// give the client a moment to answer the close frame, then drop the connection
				time.AfterFunc(2*time.Second, c.terminate)
				return
			}
			if err := c.ws.WriteMessage(websocket.TextMessage, m.data); err != nil {
				c.terminate()
				return
			}
			lastWrite = time.Now()
		case <-ticker.C:
			if time.Since(lastWrite) >= c.server.HeartbeatPeriod-10*time.Millisecond {
				_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := c.ws.WriteMessage(websocket.TextMessage, heartbeat); err != nil {
					c.terminate()
					return
				}
				lastWrite = time.Now()
			}
		}
	}
}

func (c *wsConn) sendError(code string, message string) {
	c.send(&ErrorMessage{Type: MsgError, Code: code, Message: message})
}

func (c *wsConn) fail(closeCode int, errCode string, message string) {
	c.sendError(errCode, message)
	c.close(closeCode, message)
}

// run handles the connection until it closes
func (c *wsConn) run(documentID string) {
	defer func() {
		c.server.leave(c)
		// let the writer send the queued messages and the close frame
		c.close(CloseNormal, "")
		select {
		case <-c.writerDone:
		case <-time.After(2 * time.Second):
		}
		c.terminate()
	}()

	_ = c.ws.SetReadDeadline(time.Now().Add(c.server.HelloTimeout))
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		if c.user == nil {
			c.close(CloseUnauthorized, "no hello received")
		}
		return
	}
	var hello HelloMessage
	if err = json.Unmarshal(data, &hello); err != nil || hello.Type != MsgHello {
		c.fail(CloseProtocolError, ErrCodeBadRequest, "the first message must be hello")
		return
	}
	if hello.DocumentID != "" && hello.DocumentID != documentID {
		c.fail(CloseProtocolError, ErrCodeBadRequest, "documentId does not match the URL")
		return
	}
	if c.user == nil {
		if hello.Ticket == "" {
			c.fail(CloseUnauthorized, ErrCodeUnauthorized, "authentication required")
			return
		}
		user, sessionID, err := c.server.authenticator.RedeemTicket(hello.Ticket)
		if err != nil {
			c.fail(CloseUnauthorized, ErrCodeUnauthorized, err.Error())
			return
		}
		c.user, c.sessionID = user, sessionID
	}
	if _, err = c.server.actors.Bind(hello.ActorID, c.user.ID); err != nil {
		if errors.Is(err, ErrForeignActor) {
			c.fail(CloseForbidden, ErrCodeForeignActor, err.Error())
		} else {
			c.fail(CloseProtocolError, ErrCodeBadRequest, err.Error())
		}
		return
	}
	c.actorID = hello.ActorID

	if newID := c.server.documentRedirect(documentID, hello.Epoch); newID != "" {
		log.Printf("%s: %s connected to the old id %s, redirecting to %s", c.clientID, c.user.Username, documentID, newID)
		c.send(&DocumentRenamedMessage{Type: MsgDocumentRenamed, DocumentID: documentID, NewDocumentID: newID})
		c.close(CloseRenamed, "document renamed")
		return
	}

	d := c.server.treeManager.GetDocument(documentID)
	if d == nil {
		c.send(&MessageBase{Type: MsgDocumentDeleted, DocumentID: documentID})
		c.close(CloseNotFound, "document not found")
		return
	}
	if _, err = c.server.join(c, d); err != nil {
		log.Printf("%s: unable to open %s: %v", c.clientID, d.Path, err)
		if errors.Is(err, ErrNotUTF8) {
			c.fail(CloseNotFound, ErrCodeBadRequest, err.Error())
		} else {
			c.fail(CloseInternalError, ErrCodeInternal, "unable to open the document")
		}
		return
	}
	log.Printf("%s: %s (%s) opened %s", c.clientID, c.user.Username, hello.Client, d.Path)

	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(c.server.HeartbeatTimeout))
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			var netErr interface{ Timeout() bool }
			if errors.As(err, &netErr) && netErr.Timeout() {
				c.close(CloseHeartbeatTimout, "heartbeat timeout")
			}
			return
		}
		var base MessageBase
		if err = json.Unmarshal(data, &base); err != nil {
			c.fail(CloseProtocolError, ErrCodeBadRequest, "invalid JSON")
			return
		}
		switch base.Type {
		case MsgSyncRequest:
			var m SyncMessage
			if err = json.Unmarshal(data, &m); err != nil {
				c.fail(CloseProtocolError, ErrCodeBadRequest, "invalid sync-request")
				return
			}
			if !c.handleSync(m) {
				return
			}
		case MsgPresence:
			var m PresenceMessage
			if err = json.Unmarshal(data, &m); err != nil || !validSelections(m.Selections) {
				c.sendError(ErrCodeBadRequest, "invalid presence")
				continue
			}
			c.handlePresence(m.Selections)
		case MsgHeartbeat:
		default:
			// unknown message types are ignored for forward compatibility
		}
	}
}

// attachLocked registers the connection with the session and sends the initial messages.
// Caller holds session.mu.
func (c *wsConn) attachLocked(session *docSession) error {
	c.session = session
	text, err := session.text()
	if err != nil {
		return err
	}
	syncDoc := session.doc
	readOnly := !c.user.CanWrite()
	if readOnly {
		c.shadow, err = session.doc.Fork()
		if err != nil {
			return err
		}
		syncDoc = c.shadow
	}
	c.syncState = automerge.NewSyncState(syncDoc)
	c.selections = json.RawMessage("[]")

	c.send(&InitialContentMessage{
		Type:          MsgInitialContent,
		DocumentID:    session.docID,
		Epoch:         session.meta.Epoch,
		DocumentState: encodeBase64(session.doc.Save()),
		Content:       text,
		ReadOnly:      readOnly,
		You:           c.userInfo(),
		Actors:        session.actorInfos(),
	})
	peers := make([]PresenceMessage, 0, len(session.conns))
	for other := range session.conns {
		peers = append(peers, other.presenceLocked())
	}
	c.send(&PresenceSnapshotMessage{Type: MsgPresenceSnapshot, DocumentID: session.docID, Peers: peers})
	session.conns[c] = struct{}{}
	session.announceActorLocked(c.actorID, c.user.ID, c)
	session.broadcastLocked(c.presenceLocked(), c)
	c.syncLocked()
	return nil
}

func (c *wsConn) userInfo() UserInfo {
	return UserInfo{ClientID: c.clientID, AccountID: c.user.ID, Username: c.user.Username, DisplayName: c.user.DisplayName, Role: c.user.Role, Color: c.user.Color}
}

func (c *wsConn) presenceLocked() PresenceMessage {
	return PresenceMessage{
		Type:        MsgPresence,
		DocumentID:  c.session.docID,
		ClientID:    c.clientID,
		AccountID:   c.user.ID,
		Username:    c.user.Username,
		DisplayName: c.user.DisplayName,
		Color:       c.user.Color,
		ActorID:     c.actorID,
		Selections:  c.selections,
	}
}

// syncLocked sends all pending sync messages to this connection. Caller holds session.mu.
func (c *wsConn) syncLocked() {
	if c.shadow != nil {
		if _, err := c.shadow.Merge(c.session.doc); err != nil {
			log.Printf("%s: unable to update the reader shadow: %v", c.clientID, err)
		}
	}
	for i := 0; i < 100; i++ {
		msg, valid := c.syncState.GenerateMessage()
		if !valid {
			return
		}
		c.send(&SyncMessage{Type: MsgSyncRequest, DocumentID: c.session.docID, SyncMessage: encodeBase64(msg.Bytes())})
	}
}

// handleSync applies a sync message. It returns false if the connection must close.
func (c *wsConn) handleSync(m SyncMessage) bool {
	raw, err := base64.StdEncoding.DecodeString(m.SyncMessage)
	if err != nil {
		c.fail(CloseProtocolError, ErrCodeBadRequest, "syncMessage is not base64")
		return false
	}
	decoded, err := automerge.LoadSyncMessage(raw)
	if err != nil {
		c.fail(CloseProtocolError, ErrCodeBadRequest, "invalid sync message")
		return false
	}
	changes := decoded.Changes()

	session := c.session
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return false
	}

	if c.shadow != nil {
		// readers: changes only ever reach their private shadow
		if len(changes) > 0 && !c.warnedRO {
			c.warnedRO = true
			c.sendError(ErrCodeReadOnly, "you have read-only access, your changes are not saved")
		}
		if _, err = c.syncState.ReceiveMessage(raw); err != nil {
			c.fail(CloseProtocolError, ErrCodeBadRequest, "unable to apply the sync message")
			return false
		}
		c.syncLocked()
		return true
	}

	// every change must come from an actor of this user
	var newActors []string
	for _, ch := range changes {
		actor := ch.ActorID()
		bound, err := c.server.actors.Bind(actor, c.user.ID)
		if err != nil {
			if errors.Is(err, ErrForeignActor) {
				c.fail(CloseForbidden, ErrCodeForeignActor, "the message contains changes of another user's actor "+actor)
			} else {
				c.fail(CloseProtocolError, ErrCodeBadRequest, err.Error())
			}
			return false
		}
		if bound {
			newActors = append(newActors, actor)
		}
	}

	before := session.doc.Heads()
	if _, err = c.syncState.ReceiveMessage(raw); err != nil {
		log.Printf("%s: unable to apply sync message: %v", c.clientID, err)
		c.fail(CloseProtocolError, ErrCodeBadRequest, "unable to apply the sync message")
		return false
	}
	for _, actor := range newActors {
		session.announceActorLocked(actor, c.user.ID, nil)
	}
	if !sameHeads(before, session.doc.Heads()) {
		if _, err := session.text(); err != nil {
			log.Printf("%s: the document %s has no text content any more: %v", c.clientID, session.meta.Path, err)
		}
		session.countChangesLocked(changes)
		session.scheduleFlushLocked()
		session.broadcastSyncLocked()
	} else {
		c.syncLocked()
	}
	return true
}

func sameHeads(a, b []automerge.ChangeHash) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, h := range a {
		set[h.String()] = struct{}{}
	}
	for _, h := range b {
		if _, ok := set[h.String()]; !ok {
			return false
		}
	}
	return true
}

func (c *wsConn) handlePresence(selections json.RawMessage) {
	session := c.session
	session.mu.Lock()
	defer session.mu.Unlock()
	c.selections = selections
	session.broadcastLocked(c.presenceLocked(), c)
}
