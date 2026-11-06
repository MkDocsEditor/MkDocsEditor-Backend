package collab

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	automerge "github.com/automerge/automerge-go"
)

const ContentKey = "content"

const (
	// lastPositionKeep is how long a join link can find the last position of an account
	lastPositionKeep = 30 * 24 * time.Hour
	// defaultGhostLifetime is how long the cursor of a user stays after they left
	defaultGhostLifetime = 60 * time.Second
	// maxLastPositionSize limits what one account can make the server store per document
	maxLastPositionSize = 4 << 10
)

// ErrNotUTF8 is returned for files that cannot be edited because they are not valid UTF-8
var ErrNotUTF8 = errors.New("the file is not valid UTF-8")

// docSession is a loaded Automerge document with its connections.
// All fields are guarded by mu.
type docSession struct {
	server *SyncServer

	mu   sync.Mutex
	meta *DocumentMeta
	// positionsDirty means meta.LastPositions has changes that are not on disk yet
	positionsDirty bool
	absPath        string
	docID          string
	doc            *automerge.Doc
	conns          map[*wsConn]struct{}

	// text and heads of the document the last time it was read from or written to disk
	diskBaseText  string
	diskBaseHeads []automerge.ChangeHash

	flushTimer *time.Timer
	dirty      bool
	closed     bool

	// editors are the accounts whose changes are not on disk yet, in the order of their first
	// change, for the feed
	editors []string

	// authorship replay state, advanced on request
	authorship *authorshipState
	// authorshipPending counts the changes since the last authorship job started
	authorshipPending int
}

// newDocument creates a new Automerge document with the given text, made by a server actor
func (s *SyncServer) newDocument(text string) (*automerge.Doc, error) {
	actor, err := s.actors.NewServerActor(ActorUserServer)
	if err != nil {
		return nil, err
	}
	doc := automerge.New()
	if err = doc.SetActorID(actor); err != nil {
		return nil, err
	}
	if err = doc.Path(ContentKey).Set(automerge.NewText(text)); err != nil {
		return nil, err
	}
	if _, err = doc.Commit("import"); err != nil {
		return nil, err
	}
	return doc, nil
}

// loadSession loads a document into memory and records how that went. The caller holds s.mu.
func (s *SyncServer) loadSession(relPath string, absPath string, docID string) (*docSession, error) {
	start := time.Now()
	session, err := s.openSession(relPath, absPath, docID)
	metrics.DocumentLoadDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.DocumentLoads.WithLabelValues(metrics.ResultError).Inc()
		s.logSyncEvent(SyncEvent{Kind: SyncEventLoadFailed, Path: relPath, DurationMs: milliseconds(start), Reason: err.Error()})
		return nil, err
	}
	metrics.DocumentLoads.WithLabelValues(metrics.ResultOK).Inc()
	s.logSyncEvent(SyncEvent{Kind: SyncEventLoaded, Path: relPath, Heads: len(session.doc.Heads()), DurationMs: milliseconds(start)})
	return session, nil
}

// openSession loads the saved document of a markdown file, or creates it, and merges the
// current file content into it. The caller holds s.mu.
func (s *SyncServer) openSession(relPath string, absPath string, docID string) (*docSession, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	fileText := string(data)
	if !utf8.ValidString(fileText) {
		return nil, ErrNotUTF8
	}

	if _, err = s.store.RestoreDeleted(relPath); err != nil {
		log.Printf("Unable to restore the state of deleted document %s: %v", relPath, err)
	}

	session := &docSession{
		server:  s,
		absPath: absPath,
		docID:   docID,
		conns:   map[*wsConn]struct{}{},
	}

	meta, saved, err := s.store.Load(relPath)
	if err == nil && meta != nil {
		session.doc, err = automerge.Load(saved)
		if err == nil {
			session.meta = meta
			session.diskBaseHeads, err = parseHeads(meta.DiskBaseHeads)
		}
		if err == nil {
			session.diskBaseText, err = TextAt(session.doc, session.diskBaseHeads)
		}
	}
	if err != nil && meta != nil {
		log.Printf("The saved state of %s cannot be loaded, starting a new epoch: %v", relPath, err)
		metrics.DocumentStateCorrupt.Inc()
		s.logSyncEvent(SyncEvent{Kind: SyncEventStateCorrupt, Path: relPath, Reason: err.Error()})
		if cerr := s.store.MarkCorrupt(meta); cerr != nil {
			return nil, cerr
		}
		meta = nil
	}

	if meta == nil || session.meta == nil {
		doc, err := s.newDocument(fileText)
		if err != nil {
			return nil, err
		}
		session.doc = doc
		session.meta = &DocumentMeta{UUID: newRandomID(), Path: relPath, Epoch: newRandomID()}
		session.diskBaseText = fileText
		session.diskBaseHeads = doc.Heads()
		session.meta.DiskBaseHeads = formatHeads(session.diskBaseHeads)
		if err = s.store.Save(session.meta, doc.Save()); err != nil {
			return nil, err
		}
		return session, nil
	}

	// the file may have changed while the document was not loaded
	if fileText != session.diskBaseText {
		if err = session.ingestExternalLocked(fileText); err != nil {
			return nil, err
		}
		session.dirty = true
	}
	// or the last write of the file failed
	if text, err := session.text(); err == nil && text != session.diskBaseText {
		session.scheduleFlushLocked()
	}
	return session, nil
}

func parseHeads(hexHeads []string) ([]automerge.ChangeHash, error) {
	heads := make([]automerge.ChangeHash, 0, len(hexHeads))
	for _, h := range hexHeads {
		ch, err := automerge.NewChangeHash(h)
		if err != nil {
			return nil, err
		}
		heads = append(heads, ch)
	}
	return heads, nil
}

func formatHeads(heads []automerge.ChangeHash) []string {
	out := make([]string, 0, len(heads))
	for _, h := range heads {
		out = append(out, h.String())
	}
	return out
}

// TextAt returns the content text of the document as of the given heads
func TextAt(doc *automerge.Doc, heads []automerge.ChangeHash) (string, error) {
	if len(heads) == 0 {
		return "", errors.New("no heads")
	}
	fork, err := doc.Fork(heads...)
	if err != nil {
		return "", err
	}
	return ContentText(fork)
}

// ContentText returns the content text of a document
func ContentText(doc *automerge.Doc) (string, error) {
	v, err := doc.Path(ContentKey).Get()
	if err != nil {
		return "", err
	}
	if v.Kind() != automerge.KindText {
		return "", fmt.Errorf("%s is a %v, not text", ContentKey, v.Kind())
	}
	return v.Text().Get()
}

// text returns the current content text
func (ds *docSession) text() (string, error) {
	return ContentText(ds.doc)
}

// ingestExternalLocked merges a new file content into the document as a change of the disk actor,
// concurrent to everything users changed since the disk base. Caller holds mu.
func (ds *docSession) ingestExternalLocked(fileText string) (err error) {
	start := time.Now()
	defer func() {
		result := metrics.ResultOK
		event := SyncEvent{Kind: SyncEventExternalChange, Path: ds.meta.Path, Heads: len(ds.doc.Heads()), DurationMs: milliseconds(start)}
		if err != nil {
			result = metrics.ResultError
			event.Reason = err.Error()
		}
		metrics.ExternalChanges.WithLabelValues(result).Inc()
		ds.server.logSyncEvent(event)
	}()
	if !utf8.ValidString(fileText) {
		return ErrNotUTF8
	}
	fork, err := ds.forkForServerChangeLocked(ActorUserDisk, ds.diskBaseHeads...)
	if err != nil {
		return err
	}
	if err = SpliceDiff(fork.Path(ContentKey).Text(), ds.diskBaseText, fileText); err != nil {
		return err
	}
	if _, err = fork.Commit("external change"); err != nil {
		return err
	}
	if _, err = ds.doc.Merge(fork); err != nil {
		return err
	}
	ds.diskBaseText = fileText
	ds.diskBaseHeads = fork.Heads()
	ds.meta.DiskBaseHeads = formatHeads(ds.diskBaseHeads)
	log.Printf("Merged an external change of %s", ds.meta.Path)
	ds.broadcastSyncLocked()
	ds.scheduleFlushLocked()
	return nil
}

// forkForServerChangeLocked forks the document, at the given heads if any, for a change made by
// the server: the fork has a new actor of the pseudo user, which is announced to the clients.
// Caller holds mu.
func (ds *docSession) forkForServerChangeLocked(pseudoUser string, heads ...automerge.ChangeHash) (*automerge.Doc, error) {
	fork, err := ds.doc.Fork(heads...)
	if err != nil {
		return nil, err
	}
	actor, err := ds.server.actors.NewServerActor(pseudoUser)
	if err != nil {
		return nil, err
	}
	if err = fork.SetActorID(actor); err != nil {
		return nil, err
	}
	ds.announceActorLocked(actor, pseudoUser, nil)
	return fork, nil
}

// checkDiskLocked merges the file content if it changed on disk. It returns false if the file is gone.
func (ds *docSession) checkDiskLocked() (bool, error) {
	data, err := os.ReadFile(ds.absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return true, err
	}
	if fileText := string(data); fileText != ds.diskBaseText {
		before := ds.diskBaseText
		if err = ds.ingestExternalLocked(fileText); err != nil {
			return true, err
		}
		ds.server.activity.RecordEdit(ds.meta.Path, before, fileText, []string{ActorUserDisk})
	}
	return true, nil
}

// writeToDiskLocked writes the document text to the file. It first merges the file content if the
// file changed since the disk base, so it never overwrites text that is not in the document.
func (ds *docSession) writeToDiskLocked() error {
	exists, err := ds.checkDiskLocked()
	if err != nil {
		return err
	}
	if !exists {
		// deleted on disk: the tree listener handles this, never recreate the file
		return nil
	}
	text, err := ds.text()
	if err != nil {
		return err
	}
	if text == ds.diskBaseText {
		ds.editors = nil
		return nil
	}
	if err = wiki.WriteFile(ds.absPath, []byte(text)); err != nil {
		return err
	}
	// changes of the server alone (reference updates) are no edits
	ds.server.activity.RecordEdit(ds.meta.Path, ds.diskBaseText, text, ds.editors)
	ds.editors = nil
	ds.diskBaseText = text
	ds.diskBaseHeads = ds.doc.Heads()
	ds.meta.DiskBaseHeads = formatHeads(ds.diskBaseHeads)
	ds.server.notifyWritten(ds.meta.Path)
	return nil
}

// flushLocked writes the file and saves the Automerge state. The session stays dirty when one of
// them fails, and the flush is tried again after the flush delay.
func (ds *docSession) flushLocked() {
	if ds.flushTimer != nil {
		ds.flushTimer.Stop()
		ds.flushTimer = nil
	}
	if !ds.dirty {
		return
	}
	start := time.Now()
	ds.fixMovedReferencesLocked()
	failed := false
	if err := ds.writeToDiskLocked(); err != nil {
		log.Printf("Unable to write %s: %v", ds.meta.Path, err)
		failed = true
	}
	state := ds.doc.Save()
	if err := ds.server.store.Save(ds.meta, state); err != nil {
		log.Printf("Unable to save the state of %s: %v", ds.meta.Path, err)
		failed = true
	} else {
		ds.positionsDirty = false
	}
	metrics.DocumentFlushDuration.Observe(time.Since(start).Seconds())
	if failed {
		metrics.DocumentFlushes.WithLabelValues(metrics.ResultError).Inc()
		ds.server.logSyncEvent(SyncEvent{Kind: SyncEventFlushFailed, Path: ds.meta.Path, DurationMs: milliseconds(start)})
		ds.scheduleFlushLocked()
		return
	}
	metrics.DocumentFlushes.WithLabelValues(metrics.ResultOK).Inc()
	metrics.DocumentStateSize.Observe(float64(len(state)))
	ds.server.logSyncEvent(SyncEvent{Kind: SyncEventFlushed, Path: ds.meta.Path, Bytes: len(state), Heads: len(ds.doc.Heads()), DurationMs: milliseconds(start)})
	ds.dirty = false
	// at least one, changes that are not from clients are not counted one by one
	ds.authorshipPending++
	if ds.authorshipPending >= authorshipSnapshotChanges {
		ds.startAuthorshipJobLocked(state)
	}
}

// rememberPositionLocked keeps the selections and the viewport of an account. They reach the
// disk with the next flush or when a connection leaves, so moving a cursor causes no write.
func (ds *docSession) rememberPositionLocked(accountID string, selections json.RawMessage, view json.RawMessage, at time.Time) {
	p := ds.meta.LastPositions[accountID]
	changed := false
	if len(selections) <= maxLastPositionSize && hasSelections(selections) && !bytes.Equal(selections, p.Selections) {
		p.Selections = selections
		changed = true
	}
	if view != nil && !bytes.Equal(view, jsonNull) && len(view) <= maxLastPositionSize && !bytes.Equal(view, p.Viewport) {
		p.Viewport = view
		changed = true
	}
	if !changed {
		return
	}
	if ds.meta.LastPositions == nil {
		ds.meta.LastPositions = map[string]LastPosition{}
	}
	for id, old := range ds.meta.LastPositions {
		if at.Sub(old.Time) > lastPositionKeep {
			delete(ds.meta.LastPositions, id)
		}
	}
	p.Time = at
	ds.meta.LastPositions[accountID] = p
	ds.positionsDirty = true
}

// resolveInstanceLocked sets the instance id of a joining connection. Without one it is the client
// id. Nobody can pose as an instance of another account, because an id in use by another account
// is not taken. Presence cannot be forged, and this also covers a browser with a new login.
func (ds *docSession) resolveInstanceLocked(conn *wsConn, now time.Time) {
	ds.dropExpiredGhostsLocked(now)
	id := conn.instanceID
	taken := id == ""
	if g, ok := ds.meta.Ghosts[id]; ok && g.AccountID != conn.user.ID {
		taken = true
	}
	for other := range ds.conns {
		if other.instanceID == id && other.user.ID != conn.user.ID {
			taken = true
		}
	}
	if taken {
		id = conn.clientID
	}
	conn.instanceID = id
	if _, ok := ds.meta.Ghosts[id]; ok {
		// back again: the presence of this connection ends the ghost for the clients
		delete(ds.meta.Ghosts, id)
		ds.positionsDirty = true
	}
}

// instanceLeftLocked returns the leave message of a connection, which already is out of conns. It
// has a ghost if that was the last connection of its client instance, and the connection had a
// position.
func (ds *docSession) instanceLeftLocked(conn *wsConn, now time.Time) *PresenceLeaveMessage {
	msg := &PresenceLeaveMessage{Type: MsgPresenceLeave, DocumentID: ds.docID, ClientID: conn.clientID, InstanceID: conn.instanceID}
	for other := range ds.conns {
		if other.instanceID == conn.instanceID {
			return msg
		}
	}
	if conn.ownSelections == nil && conn.ownViewport == nil {
		return msg
	}
	ds.dropExpiredGhostsLocked(now)
	if ds.meta.Ghosts == nil {
		ds.meta.Ghosts = map[string]InstanceGhost{}
	}
	ds.meta.Ghosts[conn.instanceID] = InstanceGhost{
		AccountID: conn.user.ID, DeviceName: conn.deviceName, InstanceColor: conn.instanceColor,
		Selections: selectionsOrEmpty(conn.ownSelections), Viewport: conn.ownViewport, LeftAt: now,
	}
	ds.positionsDirty = true
	msg.AccountID, msg.Selections, msg.Viewport = conn.user.ID, selectionsOrEmpty(conn.ownSelections), conn.ownViewport
	msg.DeviceName, msg.InstanceColor = conn.deviceName, conn.instanceColor
	msg.Avatar = ds.server.currentUser(conn.user).Avatar
	msg.GhostMs = ds.server.GhostLifetime.Milliseconds()
	return msg
}

func (ds *docSession) dropExpiredGhostsLocked(now time.Time) {
	for id, g := range ds.meta.Ghosts {
		if ghostRemaining(g, ds.server.GhostLifetime, now) == 0 {
			delete(ds.meta.Ghosts, id)
			ds.positionsDirty = true
		}
	}
}

// ghostsLocked lists the current ghosts of existing accounts, the most recent first
func (ds *docSession) ghostsLocked(now time.Time) []GhostInfo {
	ds.dropExpiredGhostsLocked(now)
	out := make([]GhostInfo, 0, len(ds.meta.Ghosts))
	for id, g := range ds.meta.Ghosts {
		u := ds.server.authenticator.UserByID(g.AccountID)
		if u == nil || u.Status == auth.StatusDeleted {
			continue
		}
		out = append(out, GhostInfo{InstanceID: id, AccountID: g.AccountID, Username: u.Username, DisplayName: u.DisplayName, Color: u.Color, Avatar: u.Avatar,
			InstanceColor: g.InstanceColor, DeviceName: g.DeviceName, Selections: g.Selections, Viewport: g.Viewport,
			GhostMs: max(1, ghostRemaining(g, ds.server.GhostLifetime, now).Milliseconds())})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GhostMs != out[j].GhostMs {
			return out[i].GhostMs > out[j].GhostMs
		}
		return out[i].InstanceID < out[j].InstanceID
	})
	return out
}

// ghostRemaining returns how long a ghost lasts
func ghostRemaining(g InstanceGhost, lifetime time.Duration, now time.Time) time.Duration {
	return max(0, g.LeftAt.Add(lifetime).Sub(now))
}

func selectionsOrEmpty(selections json.RawMessage) json.RawMessage {
	if selections == nil {
		return json.RawMessage("[]")
	}
	return selections
}

// savePositionsLocked writes the remembered positions if they changed
func (ds *docSession) savePositionsLocked() {
	if !ds.positionsDirty || ds.closed {
		return
	}
	if err := ds.server.store.SaveMeta(ds.meta); err != nil {
		log.Printf("Unable to save the last positions of %s: %v", ds.meta.Path, err)
		return
	}
	ds.positionsDirty = false
}

// lastPositionsLocked lists the remembered positions of existing accounts, newest first
func (ds *docSession) lastPositionsLocked() []LastPositionInfo {
	out := make([]LastPositionInfo, 0, len(ds.meta.LastPositions))
	now := time.Now()
	for id, p := range ds.meta.LastPositions {
		if now.Sub(p.Time) > lastPositionKeep {
			continue
		}
		u := ds.server.authenticator.UserByID(id)
		if u == nil || u.Status == auth.StatusDeleted {
			continue
		}
		info := LastPositionInfo{AccountID: id, Username: u.Username, DisplayName: u.DisplayName, Color: u.Color, Avatar: u.Avatar,
			Selections: selectionsOrEmpty(p.Selections), Viewport: p.Viewport, LastActiveAt: p.Time.UTC().Format(time.RFC3339Nano)}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActiveAt != out[j].LastActiveAt {
			return out[i].LastActiveAt > out[j].LastActiveAt
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out
}

// scheduleFlushLocked marks the session dirty and flushes it within the flush delay
func (ds *docSession) scheduleFlushLocked() {
	ds.dirty = true
	if ds.flushTimer != nil || ds.closed {
		return
	}
	ds.flushTimer = time.AfterFunc(ds.server.FlushDelay, ds.flushAfterDelay)
}

// flushAfterDelay runs a scheduled flush and unloads a document that nobody is connected to
// any more
func (ds *docSession) flushAfterDelay() {
	s := ds.server
	s.mu.Lock()
	defer s.mu.Unlock()
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.flushTimer = nil
	if ds.closed {
		return
	}
	ds.flushLocked()
	if !ds.dirty && len(ds.conns) == 0 {
		ds.snapshotAuthorshipLocked()
		ds.closed = true
		if s.sessions[ds.meta.Path] == ds {
			delete(s.sessions, ds.meta.Path)
		}
		s.logSyncEvent(SyncEvent{Kind: SyncEventUnloaded, Path: ds.meta.Path})
	}
}

// broadcastSyncLocked sends pending sync messages to every connection
func (ds *docSession) broadcastSyncLocked() {
	for c := range ds.conns {
		c.syncLocked()
	}
}

// broadcastLocked sends a message to every connection except one
func (ds *docSession) broadcastLocked(msg any, except *wsConn) {
	for c := range ds.conns {
		if c != except {
			c.send(msg)
		}
	}
}

// announceActorLocked tells the connections (except one) which user an actor belongs to
func (ds *docSession) announceActorLocked(actorID string, subject string, except *wsConn) {
	info := ds.server.actorInfo(actorID, subject)
	info.Type = MsgActor
	info.DocumentID = ds.docID
	ds.broadcastLocked(&info, except)
}

// countChangesLocked records the changes per user for the git commits
func (ds *docSession) countChangesLocked(changes []*automerge.Change) {
	if len(changes) == 0 {
		return
	}
	ds.authorshipPending += len(changes)
	counts := map[string]int{}
	for _, ch := range changes {
		if owner := ds.server.actors.Owner(ch.ActorID()); owner != "" {
			counts[owner]++
			ds.addEditorLocked(owner)
		}
	}
	ds.meta.LastChange = time.Now().UTC()
	if r := ds.server.changeRecorder; r != nil && len(counts) > 0 {
		r.RecordChanges(ds.meta.Path, counts)
	}
}

// addEditorLocked remembers an account that changed the document, for the feed. The server's
// pseudo users are left out.
func (ds *docSession) addEditorLocked(subject string) {
	if strings.HasPrefix(subject, ":") || slices.Contains(ds.editors, subject) {
		return
	}
	ds.editors = append(ds.editors, subject)
}

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// ReplaceText changes the text of a document to text, as splices of a new server actor, so
// connected clients merge it like any other change. The change counts for user in git.
func (s *SyncServer) ReplaceText(d *wiki.Document, text string, user *auth.User, message string) error {
	return s.withSession(d, func(ds *docSession) error {
		current, err := ds.text()
		if err != nil {
			return err
		}
		if current == text {
			return nil
		}
		fork, err := ds.forkForServerChangeLocked(ActorUserServer)
		if err != nil {
			return err
		}
		if err = SpliceDiff(fork.Path(ContentKey).Text(), current, text); err != nil {
			return err
		}
		if _, err = fork.Commit(message + " by " + user.Username); err != nil {
			return err
		}
		if _, err = ds.doc.Merge(fork); err != nil {
			return err
		}
		ds.meta.LastChange = time.Now().UTC()
		ds.addEditorLocked(user.ID)
		if s.changeRecorder != nil {
			s.changeRecorder.RecordChanges(ds.meta.Path, map[string]int{user.ID: 1})
		}
		ds.broadcastSyncLocked()
		ds.scheduleFlushLocked()
		return nil
	})
}
