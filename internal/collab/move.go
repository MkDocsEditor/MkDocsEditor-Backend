package collab

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	automerge "github.com/automerge/automerge-go"
)

// Reasons of rejected moves (details.reason), see docs/references.md section 11
const (
	MoveReasonInvalidName = "invalid-name"
	MoveReasonBlacklisted = "blacklisted"
	MoveReasonIntoItself  = "into-itself"
	MoveReasonRoot        = "root"
	MoveReasonNoChange    = "no-change"
	MoveReasonInvalidType = "invalid-type"
	MoveReasonExists      = "exists"
)

// Events websocket message type of moves
const MsgItemsMoved = "items-moved"

type (
	// MoveRequest is the body of POST /move/
	MoveRequest struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		Parent           string `json:"parent"`
		Name             string `json:"name"`
		DryRun           bool   `json:"dryRun"`
		UpdateReferences *bool  `json:"updateReferences"`
	}

	// MovedItem is an item whose id changes
	MovedItem struct {
		Type    string `json:"type"`
		OldID   string `json:"oldId"`
		NewID   string `json:"newId"`
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}

	// DocumentChange lists the changed lines of a document
	DocumentChange struct {
		DocumentID    string                `json:"documentId"`
		NewDocumentID string                `json:"newDocumentId"`
		Path          string                `json:"path"`
		NewPath       string                `json:"newPath"`
		Edits         []references.LineEdit `json:"edits"`
	}

	// MoveResult is the answer of POST /move/, for the preview and the move
	MoveResult struct {
		DryRun    bool                    `json:"dryRun"`
		Type      string                  `json:"type"`
		OldID     string                  `json:"oldId"`
		NewID     string                  `json:"newId"`
		OldPath   string                  `json:"oldPath"`
		NewPath   string                  `json:"newPath"`
		Item      any                     `json:"item,omitempty"`
		Moved     []MovedItem             `json:"moved"`
		Documents []DocumentChange        `json:"documents"`
		Config    []references.ConfigEdit `json:"config"`
		Commit    string                  `json:"commit,omitempty"`
	}

	// ItemsMovedMessage tells the events websocket which ids changed
	ItemsMovedMessage struct {
		Type  string      `json:"type"`
		Moves []MovedItem `json:"moves"`
	}
)

// MoveErrorKind says why a move was rejected
type MoveErrorKind int

const (
	// MoveInvalid is a move that makes no sense
	MoveInvalid MoveErrorKind = iota
	// MoveNotFound is a move of an item or into a section that does not exist
	MoveNotFound
	// MoveConflict is a move onto an existing item
	MoveConflict
)

// MoveError is a rejected move
type MoveError struct {
	Kind    MoveErrorKind
	Message string
	Details map[string]any
}

func (e *MoveError) Error() string {
	return e.Message
}

func badMove(reason string, message string) *MoveError {
	return &MoveError{Kind: MoveInvalid, Message: message, Details: map[string]any{"reason": reason}}
}

// CompletedMove tells what a move did, for whoever commits and audits it
type CompletedMove struct {
	ItemType string
	OldRel   string
	NewRel   string
	// Rename is true if the item stayed in its section
	Rename bool
	User   *auth.User
	// ChangedPaths are the documents whose references were rewritten, with their new paths
	ChangedPaths []string
}

// moveOp is a validated move
type moveOp struct {
	itemType string
	oldID    string
	oldRel   string
	newRel   string
	oldAbs   string
	newAbs   string
	rename   bool
	user     *auth.User
	dryRun   bool
	refs     bool
}

func (op *moveOp) mapPath(p string) string {
	if m, ok := wiki.MovePath(p, op.oldRel, op.newRel); ok {
		return m
	}
	return p
}

// Move validates and runs a move, see docs/references.md section 5. A move that is not a dry run
// returns what it did, so the caller can commit and audit it.
func (s *SyncServer) Move(req *MoveRequest, user *auth.User) (*MoveResult, *CompletedMove, error) {
	tm := s.treeManager

	// lock order as everywhere: the sync server, then the docs folder
	s.mu.Lock()
	locked := true
	unlock := func() {
		if locked {
			tm.UnlockFS()
			s.mu.Unlock()
			locked = false
		}
	}
	tm.LockFS()
	defer unlock()

	lookup := references.NewTreeLookup(tm.Root())
	op, err := s.validateMove(lookup, req)
	if err != nil {
		return nil, nil, err
	}
	op.user = user

	result := &MoveResult{
		DryRun:    op.dryRun,
		Type:      op.itemType,
		OldID:     op.oldID,
		NewID:     tm.GenerateID(op.newAbs),
		OldPath:   op.oldRel,
		NewPath:   op.newRel,
		Moved:     s.movedItems(lookup, op),
		Documents: []DocumentChange{},
		Config:    []references.ConfigEdit{},
	}

	changes, err := s.applyMoveLocked(op, lookup, result)
	if err != nil || op.dryRun {
		return result, nil, err
	}
	newLookup := references.NewTreeLookup(tm.Root())
	switch op.itemType {
	case wiki.TypeSection:
		result.Item = newLookup.Sections[op.newRel]
	case wiki.TypeDocument:
		result.Item = newLookup.Documents[op.newRel]
	case wiki.TypeResource:
		result.Item = newLookup.Resources[op.newRel]
	}
	unlock()

	s.broadcastEvent(&ItemsMovedMessage{Type: MsgItemsMoved, Moves: result.Moved})
	completed := &CompletedMove{ItemType: op.itemType, OldRel: op.oldRel, NewRel: op.newRel, Rename: op.rename, User: op.user}
	for _, pd := range changes {
		completed.ChangedPaths = append(completed.ChangedPaths, pd.newRel)
	}
	return result, completed, nil
}

// validateMove checks a move request against the tree
func (s *SyncServer) validateMove(lookup *references.TreeLookup, req *MoveRequest) (*moveOp, error) {
	tm := s.treeManager
	switch req.Type {
	case wiki.TypeSection, wiki.TypeDocument, wiki.TypeResource:
	default:
		return nil, badMove(MoveReasonInvalidType, "type must be section, document or resource")
	}
	if req.Type == wiki.TypeSection && req.ID == wiki.RootSectionID {
		return nil, badMove(MoveReasonRoot, "the root section cannot be moved")
	}
	item := lookup.Item(req.ID)
	if item == nil || item.Type != req.Type {
		return nil, &MoveError{Kind: MoveNotFound}
	}
	oldRel, currentName := item.Path, item.Name

	currentParent := path.Dir(oldRel)
	if currentParent == "." {
		currentParent = ""
	}
	parentRel := currentParent
	if req.Parent != "" {
		parent := tm.GetSection(req.Parent)
		if parent == nil {
			return nil, &MoveError{Kind: MoveNotFound, Message: "unknown target section " + req.Parent}
		}
		parentRel = parent.RelPath
	}

	name := currentName
	if req.Name != "" {
		name = req.Name
	}
	if err := wiki.ValidateName(name); err != nil {
		return nil, badMove(MoveReasonInvalidName, err.Error())
	}
	fileName := name
	if req.Type == wiki.TypeDocument {
		fileName += wiki.MarkdownFileExtension
	}
	newRel := path.Join(parentRel, fileName)

	if req.Type == wiki.TypeSection && wiki.IsInside(parentRel, oldRel) {
		return nil, badMove(MoveReasonIntoItself, "a section cannot be moved into itself or a section below it")
	}
	blocked := tm.BlockedPaths()
	for _, moved := range s.movedItems(lookup, &moveOp{itemType: req.Type, oldRel: oldRel, newRel: newRel}) {
		if slices.Contains(blocked, moved.NewPath) {
			return nil, badMove(MoveReasonBlacklisted, moved.NewPath+" is on the blacklist or an extra_css file")
		}
	}
	if newRel == oldRel {
		return nil, badMove(MoveReasonNoChange, "the item is already there")
	}
	newAbs := filepath.Join(tm.RootPath(), filepath.FromSlash(newRel))
	oldAbs := filepath.Join(tm.RootPath(), filepath.FromSlash(oldRel))
	if exists, err := wiki.FileExists(newAbs); err != nil {
		return nil, err
	} else if exists && !sameFile(oldAbs, newAbs) {
		return nil, &MoveError{Kind: MoveConflict, Message: newRel + " already exists",
			Details: map[string]any{"reason": MoveReasonExists, "path": newRel}}
	}

	return &moveOp{
		itemType: req.Type,
		oldID:    req.ID,
		oldRel:   oldRel,
		newRel:   newRel,
		oldAbs:   oldAbs,
		newAbs:   newAbs,
		rename:   parentRel == currentParent,
		dryRun:   req.DryRun,
		refs:     req.UpdateReferences == nil || *req.UpdateReferences,
	}, nil
}

// sameFile reports whether two paths name the same file (a change of case on a case-insensitive
// file system)
func sameFile(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

// movedItems lists the item and, for a section, everything inside it, sorted by old path
func (s *SyncServer) movedItems(lookup *references.TreeLookup, op *moveOp) []MovedItem {
	var out []MovedItem
	add := func(itemType string, rel string) {
		newRel := op.mapPath(rel)
		out = append(out, MovedItem{
			Type:    itemType,
			OldID:   s.treeManager.IDOfRelPath(rel),
			NewID:   s.treeManager.IDOfRelPath(newRel),
			OldPath: rel,
			NewPath: newRel,
		})
	}
	if op.itemType != wiki.TypeSection {
		add(op.itemType, op.oldRel)
		return out
	}
	for p := range lookup.Sections {
		if wiki.IsInside(p, op.oldRel) {
			add(wiki.TypeSection, p)
		}
	}
	for p := range lookup.Documents {
		if wiki.IsInside(p, op.oldRel) {
			add(wiki.TypeDocument, p)
		}
	}
	for p := range lookup.Resources {
		if wiki.IsInside(p, op.oldRel) {
			add(wiki.TypeResource, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OldPath < out[j].OldPath })
	return out
}

// plannedDocument is a document whose references change
type plannedDocument struct {
	doc     *wiki.Document
	oldRel  string
	newRel  string
	session *docSession
	// temporary sessions were loaded for the move and are unloaded afterwards
	temporary bool
	text      string
	edits     []references.TextEdit
	fork      *automerge.Doc
}

// moveConfig is the change of mkdocs.yml of a move
type moveConfig struct {
	file   string
	before []byte
	after  string
	edits  []references.ConfigEdit
}

// planMoveConfig computes the change of mkdocs.yml. A missing file means no change.
func (s *SyncServer) planMoveConfig(op *moveOp) *moveConfig {
	config := &moveConfig{file: s.treeManager.ConfigFile()}
	content, err := os.ReadFile(config.file)
	if err != nil || !op.refs {
		return config
	}
	config.before = content
	config.after, config.edits = references.ConfigEdits(string(content), filepath.Base(config.file), op.mapPath)
	return config
}

// planReferences finds the documents whose references change, from the live text of loaded
// documents
func (s *SyncServer) planReferences(op *moveOp, lookup *references.TreeLookup) []*plannedDocument {
	if !op.refs {
		return nil
	}
	var planned []*plannedDocument
	for _, d := range lookup.DocumentList {
		pd := &plannedDocument{doc: d, oldRel: d.RelPath, newRel: op.mapPath(d.RelPath), session: s.sessions[d.RelPath]}
		text, err := s.documentTextLocked(pd)
		if err != nil {
			log.Printf("Move: not updating the references of %s: %v", d.RelPath, err)
			continue
		}
		if edits := references.ReferenceEdits(text, pd.oldRel, pd.newRel, lookup, op.mapPath); len(edits) > 0 {
			pd.text, pd.edits = text, edits
			planned = append(planned, pd)
		}
	}
	return planned
}

// applyMoveLocked plans the move and, unless it is a dry run, applies it: the rename on disk,
// the Automerge state, the reference edits, mkdocs.yml and the redirects. Caller holds s.mu and
// the tree's fsLock. It returns the documents whose text changed.
func (s *SyncServer) applyMoveLocked(op *moveOp, lookup *references.TreeLookup, result *MoveResult) ([]*plannedDocument, error) {
	planned := s.planReferences(op, lookup)
	config := s.planMoveConfig(op)
	if config.edits != nil {
		result.Config = config.edits
	}

	if op.dryRun {
		for _, pd := range planned {
			result.Documents = append(result.Documents, s.documentChange(pd, references.LineEdits(pd.text, pd.edits)))
		}
		sortDocumentChanges(result.Documents)
		return nil, nil
	}

	sessions, err := s.moveSessions(op, planned)
	if err != nil {
		return nil, err
	}
	// sorted, so two moves cannot deadlock
	for _, pd := range sessions {
		pd.session.mu.Lock()
	}
	unlockSessions := func() {
		for _, pd := range sessions {
			pd.session.mu.Unlock()
		}
	}

	changed, err := s.prepareSessionsLocked(sessions, op, lookup)
	if err == nil {
		err = s.moveOnDisk(op, config, result.Moved)
	}
	if err != nil {
		unlockSessions()
		s.releaseLocked(sessions)
		return nil, err
	}

	// nothing can fail from here on
	for _, pd := range sessions {
		s.finishSessionLocked(pd, op, result)
	}
	unlockSessions()
	s.releaseLocked(sessions)
	sortDocumentChanges(result.Documents)
	s.treeManager.CreateItemTree()
	return changed, nil
}

// moveSessions returns the sessions of a move, sorted by path: the documents with changing
// references, loaded if needed, and the loaded documents that move. Caller holds s.mu.
func (s *SyncServer) moveSessions(op *moveOp, planned []*plannedDocument) ([]*plannedDocument, error) {
	var sessions []*plannedDocument
	for _, pd := range planned {
		if pd.session == nil {
			session, err := s.loadSession(pd.oldRel, pd.doc.Path, pd.doc.ID)
			if err != nil {
				s.releaseLocked(sessions)
				return nil, fmt.Errorf("unable to open %s: %w", pd.oldRel, err)
			}
			pd.session, pd.temporary = session, true
		}
		sessions = append(sessions, pd)
	}
	for p, session := range s.sessions {
		if wiki.IsInside(p, op.oldRel) && !slices.ContainsFunc(sessions, func(pd *plannedDocument) bool { return pd.session == session }) {
			sessions = append(sessions, &plannedDocument{oldRel: p, newRel: op.mapPath(p), session: session})
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].oldRel < sessions[j].oldRel })
	return sessions, nil
}

// prepareSessionsLocked flushes the sessions, so disk, Automerge state and text agree before
// anything moves, and prepares the reference edits on the current text: clients may have edited
// since the plan. It returns the documents with edits. Caller holds the session locks.
func (s *SyncServer) prepareSessionsLocked(sessions []*plannedDocument, op *moveOp, lookup *references.TreeLookup) ([]*plannedDocument, error) {
	var changed []*plannedDocument
	for _, pd := range sessions {
		if pd.session.closed {
			return nil, fmt.Errorf("%s was closed during the move", pd.oldRel)
		}
		pd.session.flushLocked()
		if pd.doc == nil {
			continue
		}
		text, err := pd.session.text()
		if err != nil {
			return nil, err
		}
		pd.text = text
		pd.edits = references.ReferenceEdits(text, pd.oldRel, pd.newRel, lookup, op.mapPath)
		if len(pd.edits) == 0 {
			continue
		}
		if pd.fork, err = s.prepareRewrite(pd, op); err != nil {
			return nil, fmt.Errorf("unable to update the references of %s: %w", pd.oldRel, err)
		}
		changed = append(changed, pd)
	}
	return changed, nil
}

// moveOnDisk renames the item, moves its Automerge state, writes mkdocs.yml and records the
// redirects. On failure everything that was done is undone.
func (s *SyncServer) moveOnDisk(op *moveOp, config *moveConfig, moved []MovedItem) error {
	// the rename: one rename(2), nothing changed before it
	if err := os.Rename(op.oldAbs, op.newAbs); err != nil {
		return err
	}
	undoState := func() {
		if err := s.store.Move(op.newRel, op.oldRel); err != nil {
			log.Printf("Unable to undo the state move of %s: %v", op.oldRel, err)
		}
		if err := os.Rename(op.newAbs, op.oldAbs); err != nil {
			log.Printf("Unable to undo the move of %s: %v", op.oldRel, err)
		}
	}
	if err := s.store.Move(op.oldRel, op.newRel); err != nil {
		undoState()
		return err
	}
	configChanges := len(config.edits) > 0
	if configChanges {
		if err := wiki.WriteFile(config.file, []byte(config.after)); err != nil {
			undoState()
			return err
		}
	}
	if err := s.redirects.Append(s.redirectEntries(op, moved)...); err != nil {
		if configChanges {
			if werr := wiki.WriteFile(config.file, config.before); werr != nil {
				log.Printf("Unable to restore %s: %v", config.file, werr)
			}
		}
		undoState()
		return err
	}
	return nil
}

// finishSessionLocked moves a session to the new path of its document, merges the reference edits
// and tells the clients. Caller holds s.mu and the session lock.
func (s *SyncServer) finishSessionLocked(pd *plannedDocument, op *moveOp, result *MoveResult) {
	session := pd.session
	oldDocID := session.docID
	movedDoc := pd.newRel != pd.oldRel
	if movedDoc {
		session.meta.Path = pd.newRel
		session.absPath = filepath.Join(s.treeManager.RootPath(), filepath.FromSlash(pd.newRel))
		session.docID = s.treeManager.GenerateID(session.absPath)
		if !pd.temporary {
			delete(s.sessions, pd.oldRel)
			s.sessions[pd.newRel] = session
		}
	}
	if pd.fork != nil {
		if _, err := session.doc.Merge(pd.fork); err != nil {
			log.Printf("Unable to merge the reference updates of %s: %v", pd.newRel, err)
		} else {
			session.meta.LastChange = time.Now().UTC()
			if s.changeRecorder != nil && op.user != nil {
				s.changeRecorder.RecordChanges(pd.newRel, map[string]int{op.user.ID: 1})
			}
			session.dirty = true
			result.Documents = append(result.Documents, s.documentChange(pd, references.LineEdits(pd.text, pd.edits)))
		}
	}
	// written now, so the commit holds the new text
	session.flushLocked()
	session.broadcastSyncLocked()
	if movedDoc {
		for c := range session.conns {
			c.send(&DocumentRenamedMessage{Type: MsgDocumentRenamed, DocumentID: oldDocID, NewDocumentID: session.docID})
			c.close(CloseRenamed, "document renamed")
		}
	}
}

// documentTextLocked returns the live text of a loaded document, otherwise the file content
func (s *SyncServer) documentTextLocked(pd *plannedDocument) (string, error) {
	if pd.session != nil {
		pd.session.mu.Lock()
		defer pd.session.mu.Unlock()
		if !pd.session.closed {
			return pd.session.text()
		}
	}
	data, err := os.ReadFile(pd.doc.Path)
	return string(data), err
}

// prepareRewrite applies the reference edits to a fork of the document, as a change of a new
// server actor. The fork is merged only when the whole move succeeds. Caller holds the session lock.
func (s *SyncServer) prepareRewrite(pd *plannedDocument, op *moveOp) (*automerge.Doc, error) {
	fork, err := pd.session.forkForServerChangeLocked(ActorUserServer)
	if err != nil {
		return nil, err
	}
	if err = references.SpliceEdits(fork.Path(ContentKey).Text(), pd.text, pd.edits); err != nil {
		return nil, err
	}
	got, err := ContentText(fork)
	if err != nil {
		return nil, err
	}
	if got != references.ApplyEdits(pd.text, pd.edits) {
		return nil, errors.New("the reference updates do not apply cleanly")
	}
	username := ""
	if op.user != nil {
		username = " by " + op.user.Username
	}
	if _, err = fork.Commit("update references after moving " + op.oldRel + " to " + op.newRel + username); err != nil {
		return nil, err
	}
	return fork, nil
}

// releaseLocked unloads the temporary sessions of a move. Caller holds s.mu.
func (s *SyncServer) releaseLocked(sessions []*plannedDocument) {
	for _, pd := range sessions {
		if !pd.temporary {
			continue
		}
		pd.session.mu.Lock()
		if pd.session.dirty {
			// keep it loaded until the flush succeeds
			s.sessions[pd.session.meta.Path] = pd.session
			pd.session.scheduleFlushLocked()
		} else {
			pd.session.snapshotAuthorshipLocked()
			pd.session.closed = true
		}
		pd.session.mu.Unlock()
	}
}

func (s *SyncServer) documentChange(pd *plannedDocument, edits []references.LineEdit) DocumentChange {
	root := s.treeManager.RootPath()
	return DocumentChange{
		DocumentID:    s.treeManager.GenerateID(filepath.Join(root, filepath.FromSlash(pd.oldRel))),
		NewDocumentID: s.treeManager.GenerateID(filepath.Join(root, filepath.FromSlash(pd.newRel))),
		Path:          pd.oldRel,
		NewPath:       pd.newRel,
		Edits:         edits,
	}
}

func sortDocumentChanges(changes []DocumentChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].NewPath < changes[j].NewPath })
}

// redirectEntries records the moved items, with the Automerge lineage of documents that have one
func (s *SyncServer) redirectEntries(op *moveOp, moved []MovedItem) []RedirectEntry {
	now := time.Now().UTC()
	accountID := ""
	if op.user != nil {
		accountID = op.user.ID
	}
	entries := make([]RedirectEntry, 0, len(moved))
	for _, m := range moved {
		e := RedirectEntry{Time: now, Type: m.Type, OldPath: m.OldPath, NewPath: m.NewPath, AccountID: accountID}
		if m.Type == wiki.TypeDocument {
			// the state has already moved with the file
			if meta := s.store.Meta(m.NewPath); meta != nil {
				e.UUID, e.Epoch = meta.UUID, meta.Epoch
			}
		}
		entries = append(entries, e)
	}
	return entries
}
