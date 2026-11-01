package backend

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"time"
	"unicode/utf8"

	automerge "github.com/automerge/automerge-go"
)

const contentKey = "content"

// ErrNotUTF8 is returned for files that cannot be edited because they are not valid UTF-8
var ErrNotUTF8 = errors.New("the file is not valid UTF-8")

// docSession is a loaded Automerge document with its connections.
// All fields are guarded by mu.
type docSession struct {
	server *SyncServer

	mu      sessionMutex
	meta    *DocumentMeta
	absPath string
	docID   string
	doc     *automerge.Doc
	conns   map[*wsConn]struct{}

	// text and heads of the document the last time it was read from or written to disk
	diskBaseText  string
	diskBaseHeads []automerge.ChangeHash

	flushTimer *time.Timer
	dirty      bool
	closed     bool

	// authorship replay state, advanced on request
	authorship *authorshipState
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
	if err = doc.Path(contentKey).Set(automerge.NewText(text)); err != nil {
		return nil, err
	}
	if _, err = doc.Commit("import"); err != nil {
		return nil, err
	}
	return doc, nil
}

// loadSession loads the saved document of a markdown file, or creates it, and merges the
// current file content into it. The caller holds s.mu.
func (s *SyncServer) loadSession(relPath string, absPath string, docID string) (*docSession, error) {
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
			session.diskBaseText, err = textAt(session.doc, session.diskBaseHeads)
		}
	}
	if err != nil && meta != nil {
		log.Printf("The saved state of %s cannot be loaded, starting a new epoch: %v", relPath, err)
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

// textAt returns the content text of the document as of the given heads
func textAt(doc *automerge.Doc, heads []automerge.ChangeHash) (string, error) {
	if len(heads) == 0 {
		return "", errors.New("no heads")
	}
	fork, err := doc.Fork(heads...)
	if err != nil {
		return "", err
	}
	return contentText(fork)
}

// contentText returns the content text of a document
func contentText(doc *automerge.Doc) (string, error) {
	v, err := doc.Path(contentKey).Get()
	if err != nil {
		return "", err
	}
	if v.Kind() != automerge.KindText {
		return "", fmt.Errorf("%s is a %v, not text", contentKey, v.Kind())
	}
	return v.Text().Get()
}

// text returns the current content text
func (ds *docSession) text() (string, error) {
	return contentText(ds.doc)
}

// ingestExternalLocked merges a new file content into the document as a change of the disk actor,
// concurrent to everything users changed since the disk base. Caller holds mu.
func (ds *docSession) ingestExternalLocked(fileText string) error {
	if !utf8.ValidString(fileText) {
		return ErrNotUTF8
	}
	fork, err := ds.doc.Fork(ds.diskBaseHeads...)
	if err != nil {
		return err
	}
	actor, err := ds.server.actors.NewServerActor(ActorUserDisk)
	if err != nil {
		return err
	}
	if err = fork.SetActorID(actor); err != nil {
		return err
	}
	ds.announceActorLocked(actor, ActorUserDisk, nil)
	if err = spliceDiff(fork.Path(contentKey).Text(), ds.diskBaseText, fileText); err != nil {
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

// checkDiskLocked merges the file content if it changed on disk. It returns false if the file is gone.
func (ds *docSession) checkDiskLocked() (bool, error) {
	data, err := os.ReadFile(ds.absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return true, err
	}
	if string(data) != ds.diskBaseText {
		return true, ds.ingestExternalLocked(string(data))
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
		return nil
	}
	if err = WriteFile(ds.absPath, []byte(text)); err != nil {
		return err
	}
	ds.diskBaseText = text
	ds.diskBaseHeads = ds.doc.Heads()
	ds.meta.DiskBaseHeads = formatHeads(ds.diskBaseHeads)
	ds.server.notifyWritten(ds.meta.Path)
	return nil
}

// flushLocked writes the file and saves the Automerge state
func (ds *docSession) flushLocked() {
	if ds.flushTimer != nil {
		ds.flushTimer.Stop()
		ds.flushTimer = nil
	}
	if !ds.dirty {
		return
	}
	ds.fixMovedReferencesLocked()
	if err := ds.writeToDiskLocked(); err != nil {
		log.Printf("Unable to write %s: %v", ds.meta.Path, err)
	}
	if err := ds.server.store.Save(ds.meta, ds.doc.Save()); err != nil {
		log.Printf("Unable to save the state of %s: %v", ds.meta.Path, err)
		return
	}
	ds.dirty = false
}

// scheduleFlushLocked marks the session dirty and flushes it within the flush delay
func (ds *docSession) scheduleFlushLocked() {
	ds.dirty = true
	if ds.flushTimer != nil || ds.closed {
		return
	}
	ds.flushTimer = time.AfterFunc(ds.server.FlushDelay, func() {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		ds.flushTimer = nil
		if !ds.closed {
			ds.flushLocked()
		}
	})
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

// actorInfos returns the actor map for the initial content
func (ds *docSession) actorInfos() []ActorInfo {
	all := ds.server.actors.All()
	out := make([]ActorInfo, 0, len(all))
	for _, id := range SortedActorIDs(all) {
		out = append(out, ds.server.actorInfo(id, all[id].Subject))
	}
	return out
}

// countChangesLocked records the changes per user for the git commits
func (ds *docSession) countChangesLocked(changes []*automerge.Change) {
	if len(changes) == 0 {
		return
	}
	counts := map[string]int{}
	for _, ch := range changes {
		if owner := ds.server.actors.Owner(ch.ActorID()); owner != "" {
			counts[owner]++
		}
	}
	ds.meta.LastChange = time.Now().UTC()
	if r := ds.server.changeRecorder; r != nil && len(counts) > 0 {
		r.RecordChanges(ds.meta.Path, counts)
	}
}

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
