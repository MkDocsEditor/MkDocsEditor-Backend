package backend

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	automerge "github.com/automerge/automerge-go"
	"github.com/labstack/echo/v4"
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
		DocumentID    string     `json:"documentId"`
		NewDocumentID string     `json:"newDocumentId"`
		Path          string     `json:"path"`
		NewPath       string     `json:"newPath"`
		Edits         []LineEdit `json:"edits"`
	}

	// MoveResult is the answer of POST /move/, for the preview and the move
	MoveResult struct {
		DryRun    bool             `json:"dryRun"`
		Type      string           `json:"type"`
		OldID     string           `json:"oldId"`
		NewID     string           `json:"newId"`
		OldPath   string           `json:"oldPath"`
		NewPath   string           `json:"newPath"`
		Item      any              `json:"item,omitempty"`
		Moved     []MovedItem      `json:"moved"`
		Documents []DocumentChange `json:"documents"`
		Config    []ConfigEdit     `json:"config"`
		Commit    string           `json:"commit,omitempty"`
	}

	// ItemsMovedMessage tells the events websocket which ids changed
	ItemsMovedMessage struct {
		Type  string      `json:"type"`
		Moves []MovedItem `json:"moves"`
	}

	// MovedResponse is the answer of GET /moved/{id}/
	MovedResponse struct {
		OldID   string    `json:"oldId"`
		Type    string    `json:"type"`
		Item    *ItemRef  `json:"item"`
		MovedAt time.Time `json:"movedAt"`
	}
)

// moveError is a rejected move with its answer
type moveError struct {
	status  int
	code    string
	message string
	details map[string]any
}

func (e *moveError) Error() string {
	return e.message
}

func badMove(reason string, message string) *moveError {
	return &moveError{status: http.StatusBadRequest, code: CodeBadRequest, message: message, details: map[string]any{"reason": reason}}
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
	if m, ok := movePath(p, op.oldRel, op.newRel); ok {
		return m
	}
	return p
}

// POST /move/
func (rs *RestService) postMove(c echo.Context) error {
	req := new(MoveRequest)
	if err := c.Bind(req); err != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid JSON", nil)
	}
	result, err := rs.performMove(c, req)
	if err != nil {
		return rs.moveFailed(c, req.ID, err)
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// moveFailed answers a failed move
func (rs *RestService) moveFailed(c echo.Context, id string, err error) error {
	var me *moveError
	switch {
	case errors.As(err, &me):
		if me.status == http.StatusNotFound {
			return rs.ReturnNotFound(c, id)
		}
		return apiError(c, me.status, me.code, me.message, me.details)
	case errors.Is(err, ErrNotFound):
		return rs.ReturnNotFound(c, id)
	}
	log.Printf("Move of %s failed: %v", id, err)
	return apiError(c, http.StatusInternalServerError, CodeInternal, "the move failed and was undone: "+err.Error(), nil)
}

// renameViaMove renames an item in place with reference rewriting and answers the renamed
// item, like the rename endpoints always did
func (rs *RestService) renameViaMove(c echo.Context, itemType string, name string) error {
	id := c.Param(urlParamId)
	lookup := newTreeLookup(rs.treeManager.Root())
	item := lookup.item(id)
	if item == nil || item.Type != itemType {
		return rs.ReturnNotFound(c, id)
	}
	currentName := path.Base(item.Path)
	if itemType == TypeDocument {
		currentName = strings.TrimSuffix(currentName, markdownFileExtension)
	}
	if name == currentName {
		return rs.getItemDescription(c, itemType)
	}
	result, err := rs.performMove(c, &MoveRequest{Type: itemType, ID: id, Name: name})
	if err != nil {
		return rs.moveFailed(c, id, err)
	}
	return c.JSONPretty(http.StatusOK, result.Item, indentationChar)
}

// performMove validates and runs a move, see docs/references.md section 5
func (rs *RestService) performMove(c echo.Context, req *MoveRequest) (*MoveResult, error) {
	if rs.sync == nil {
		return nil, errors.New("moving needs the sync server")
	}
	s := rs.sync
	tm := rs.treeManager

	// lock order as everywhere: the sync server, then the docs folder
	s.mu.Lock()
	locked := true
	unlock := func() {
		if locked {
			tm.fsLock.Unlock()
			s.mu.Unlock()
			locked = false
		}
	}
	tm.fsLock.Lock()
	defer unlock()

	lookup := newTreeLookup(tm.Root())
	op, err := rs.validateMove(lookup, req)
	if err != nil {
		return nil, err
	}
	op.user = UserFromContext(c)

	result := &MoveResult{
		DryRun:    op.dryRun,
		Type:      op.itemType,
		OldID:     op.oldID,
		NewID:     tm.GenerateId(op.newAbs),
		OldPath:   op.oldRel,
		NewPath:   op.newRel,
		Moved:     rs.movedItems(lookup, op),
		Documents: []DocumentChange{},
		Config:    []ConfigEdit{},
	}

	changes, err := s.applyMoveLocked(op, lookup, result)
	if err != nil || op.dryRun {
		return result, err
	}
	newLookup := newTreeLookup(tm.Root())
	switch op.itemType {
	case TypeSection:
		result.Item = newLookup.sections[op.newRel]
	case TypeDocument:
		result.Item = newLookup.documents[op.newRel]
	case TypeResource:
		result.Item = newLookup.resources[op.newRel]
	}
	unlock()

	s.broadcastEvent(&ItemsMovedMessage{Type: MsgItemsMoved, Moves: result.Moved})
	result.Commit = rs.commitMove(op, changes, len(result.Config) > 0)
	rs.auditMove(c, op, result)
	return result, nil
}

// validateMove checks a move request against the tree
func (rs *RestService) validateMove(lookup *treeLookup, req *MoveRequest) (*moveOp, error) {
	tm := rs.treeManager
	var oldRel, currentName string
	switch req.Type {
	case TypeSection:
		if req.ID == RootSectionID {
			return nil, badMove(MoveReasonRoot, "the root section cannot be moved")
		}
		if s := tm.GetSection(req.ID); s != nil {
			oldRel, currentName = s.RelPath, s.Name
		}
	case TypeDocument:
		if d := tm.GetDocument(req.ID); d != nil {
			oldRel, currentName = d.RelPath, d.Name
		}
	case TypeResource:
		if r := tm.GetResource(req.ID); r != nil {
			oldRel, currentName = r.RelPath, r.Name
		}
	default:
		return nil, badMove(MoveReasonInvalidType, "type must be section, document or resource")
	}
	if oldRel == "" {
		return nil, &moveError{status: http.StatusNotFound}
	}

	currentParent := path.Dir(oldRel)
	if currentParent == "." {
		currentParent = ""
	}
	parentRel := currentParent
	if req.Parent != "" {
		parent := tm.GetSection(req.Parent)
		if parent == nil {
			return nil, &moveError{status: http.StatusNotFound, message: "unknown target section " + req.Parent}
		}
		parentRel = parent.RelPath
	}

	name := currentName
	if req.Name != "" {
		name = req.Name
	}
	if err := ValidateName(name); err != nil {
		return nil, badMove(MoveReasonInvalidName, err.Error())
	}
	fileName := name
	if req.Type == TypeDocument {
		fileName += markdownFileExtension
	}
	newRel := path.Join(parentRel, fileName)

	if req.Type == TypeSection && isInside(parentRel, oldRel) {
		return nil, badMove(MoveReasonIntoItself, "a section cannot be moved into itself or a section below it")
	}
	if blocked := blockedPaths(); len(blocked) > 0 {
		for _, item := range rs.movedItems(lookup, &moveOp{itemType: req.Type, oldRel: oldRel, newRel: newRel}) {
			if slices.Contains(blocked, item.NewPath) {
				return nil, badMove(MoveReasonBlacklisted, item.NewPath+" is on the blacklist or an extra_css file")
			}
		}
	}
	if newRel == oldRel {
		return nil, badMove(MoveReasonNoChange, "the item is already there")
	}
	newAbs := filepath.Join(tm.RootPath(), filepath.FromSlash(newRel))
	oldAbs := filepath.Join(tm.RootPath(), filepath.FromSlash(oldRel))
	if exists, err := fileExists(newAbs); err != nil {
		return nil, err
	} else if exists && !sameFile(oldAbs, newAbs) {
		return nil, &moveError{status: http.StatusConflict, code: CodeConflict, message: newRel + " already exists",
			details: map[string]any{"reason": MoveReasonExists, "path": newRel}}
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

// blockedPaths are the paths that are no items: the blacklist and the extra_css files
func blockedPaths() []string {
	blocked := slices.Clone(configuration.CurrentConfig.MkDocs.Blacklist)
	if mkDocsConfig, err := readMkDocsConfig(); err == nil {
		blocked = append(blocked, mkDocsConfig.ExtraCss...)
	}
	return blocked
}

// movedItems lists the item and, for a section, everything inside it, sorted by old path
func (rs *RestService) movedItems(lookup *treeLookup, op *moveOp) []MovedItem {
	var out []MovedItem
	add := func(itemType string, rel string) {
		newRel := op.mapPath(rel)
		out = append(out, MovedItem{
			Type:    itemType,
			OldID:   rs.treeManager.GenerateId(filepath.Join(rs.treeManager.RootPath(), filepath.FromSlash(rel))),
			NewID:   rs.treeManager.GenerateId(filepath.Join(rs.treeManager.RootPath(), filepath.FromSlash(newRel))),
			OldPath: rel,
			NewPath: newRel,
		})
	}
	if op.itemType != TypeSection {
		add(op.itemType, op.oldRel)
		return out
	}
	for p := range lookup.sections {
		if isInside(p, op.oldRel) {
			add(TypeSection, p)
		}
	}
	for p := range lookup.documents {
		if isInside(p, op.oldRel) {
			add(TypeDocument, p)
		}
	}
	for p := range lookup.resources {
		if isInside(p, op.oldRel) {
			add(TypeResource, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OldPath < out[j].OldPath })
	return out
}

// plannedDocument is a document whose references change
type plannedDocument struct {
	doc     *Document
	oldRel  string
	newRel  string
	session *docSession
	// temporary sessions were loaded for the move and are unloaded afterwards
	temporary bool
	text      string
	edits     []textEdit
	fork      *automerge.Doc
}

// applyMoveLocked plans the move and, unless it is a dry run, applies it: the rename on disk,
// the Automerge state, the reference edits, mkdocs.yml and the redirects. Caller holds s.mu and
// the tree's fsLock. It returns the documents whose text changed.
func (s *SyncServer) applyMoveLocked(op *moveOp, lookup *treeLookup, result *MoveResult) ([]*plannedDocument, error) {
	// the plan: every document's references, from the live text of loaded documents
	var planned []*plannedDocument
	if op.refs {
		for _, d := range lookup.documentList {
			pd := &plannedDocument{doc: d, oldRel: d.RelPath, newRel: op.mapPath(d.RelPath), session: s.sessions[d.RelPath]}
			text, err := s.documentTextLocked(pd)
			if err != nil {
				log.Printf("Move: not updating the references of %s: %v", d.RelPath, err)
				continue
			}
			if edits := referenceEdits(text, pd.oldRel, pd.newRel, lookup, op.mapPath); len(edits) > 0 {
				pd.text, pd.edits = text, edits
				planned = append(planned, pd)
			}
		}
	}

	configFile := configuration.CurrentConfig.MkDocs.ConfigFile
	configContent, configErr := os.ReadFile(configFile)
	var newConfig string
	if configErr == nil && op.refs {
		var configChanges []ConfigEdit
		newConfig, configChanges = configEdits(string(configContent), filepath.Base(configFile), op.mapPath)
		if configChanges != nil {
			result.Config = configChanges
		}
	}

	if op.dryRun {
		for _, pd := range planned {
			result.Documents = append(result.Documents, s.documentChange(pd, lineEdits(pd.text, pd.edits)))
		}
		sortDocumentChanges(result.Documents)
		return nil, nil
	}

	// lock every affected session (sorted, so two moves cannot deadlock) and recompute its edits on
	// the current text: clients may have edited since the plan
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
		if _, ok := movePath(p, op.oldRel, op.oldRel); ok && !slices.ContainsFunc(sessions, func(pd *plannedDocument) bool { return pd.session == session }) {
			sessions = append(sessions, &plannedDocument{oldRel: p, newRel: op.mapPath(p), session: session})
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].oldRel < sessions[j].oldRel })
	for _, pd := range sessions {
		pd.session.mu.Lock()
	}
	unlockSessions := func() {
		for _, pd := range sessions {
			pd.session.mu.Unlock()
		}
	}
	fail := func(err error) ([]*plannedDocument, error) {
		unlockSessions()
		s.releaseLocked(sessions)
		return nil, err
	}

	var changed []*plannedDocument
	for _, pd := range sessions {
		if pd.session.closed {
			return fail(fmt.Errorf("%s was closed during the move", pd.oldRel))
		}
		// disk, Automerge state and text agree before anything moves
		pd.session.flushLocked()
		if pd.doc == nil {
			continue
		}
		text, err := pd.session.text()
		if err != nil {
			return fail(err)
		}
		pd.text = text
		pd.edits = referenceEdits(text, pd.oldRel, pd.newRel, lookup, op.mapPath)
		if len(pd.edits) == 0 {
			continue
		}
		if pd.fork, err = s.prepareRewrite(pd, op); err != nil {
			return fail(fmt.Errorf("unable to update the references of %s: %w", pd.oldRel, err))
		}
		changed = append(changed, pd)
	}

	// the rename: one rename(2), nothing changed before it
	if err := os.Rename(op.oldAbs, op.newAbs); err != nil {
		return fail(err)
	}
	undoRename := func() {
		if err := os.Rename(op.newAbs, op.oldAbs); err != nil {
			log.Printf("Unable to undo the move of %s: %v", op.oldRel, err)
		}
	}
	if err := s.store.Move(op.oldRel, op.newRel); err != nil {
		if uerr := s.store.Move(op.newRel, op.oldRel); uerr != nil {
			log.Printf("Unable to undo the state move of %s: %v", op.oldRel, uerr)
		}
		undoRename()
		return fail(err)
	}
	undoState := func() {
		if err := s.store.Move(op.newRel, op.oldRel); err != nil {
			log.Printf("Unable to undo the state move of %s: %v", op.oldRel, err)
		}
		undoRename()
	}
	if len(result.Config) > 0 {
		if err := WriteFile(configFile, []byte(newConfig)); err != nil {
			undoState()
			return fail(err)
		}
	}
	if err := s.redirects.Append(s.redirectEntries(op, result.Moved)...); err != nil {
		if len(result.Config) > 0 {
			if werr := WriteFile(configFile, configContent); werr != nil {
				log.Printf("Unable to restore %s: %v", configFile, werr)
			}
		}
		undoState()
		return fail(err)
	}

	// nothing can fail from here on: move the sessions and merge the reference edits
	for _, pd := range sessions {
		session := pd.session
		oldDocID := session.docID
		movedDoc := pd.newRel != pd.oldRel
		if movedDoc {
			session.meta.Path = pd.newRel
			session.absPath = filepath.Join(s.treeManager.RootPath(), filepath.FromSlash(pd.newRel))
			session.docID = s.treeManager.GenerateId(session.absPath)
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
				result.Documents = append(result.Documents, s.documentChange(pd, lineEdits(pd.text, pd.edits)))
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
	unlockSessions()
	s.releaseLocked(sessions)
	sortDocumentChanges(result.Documents)
	s.treeManager.CreateItemTree()
	return changed, nil
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
	fork, err := pd.session.doc.Fork()
	if err != nil {
		return nil, err
	}
	actor, err := s.actors.NewServerActor(ActorUserServer)
	if err != nil {
		return nil, err
	}
	if err = fork.SetActorID(actor); err != nil {
		return nil, err
	}
	if err = spliceEdits(fork.Path(contentKey).Text(), pd.text, pd.edits); err != nil {
		return nil, err
	}
	if got, err := contentText(fork); err != nil || got != applyEdits(pd.text, pd.edits) {
		return nil, fmt.Errorf("the reference updates do not apply cleanly: %v", err)
	}
	username := ""
	if op.user != nil {
		username = " by " + op.user.Username
	}
	if _, err = fork.Commit("update references after moving " + op.oldRel + " to " + op.newRel + username); err != nil {
		return nil, err
	}
	pd.session.announceActorLocked(actor, ActorUserServer, nil)
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
			pd.session.closed = true
		}
		pd.session.mu.Unlock()
	}
}

func (s *SyncServer) documentChange(pd *plannedDocument, edits []LineEdit) DocumentChange {
	root := s.treeManager.RootPath()
	return DocumentChange{
		DocumentID:    s.treeManager.GenerateId(filepath.Join(root, filepath.FromSlash(pd.oldRel))),
		NewDocumentID: s.treeManager.GenerateId(filepath.Join(root, filepath.FromSlash(pd.newRel))),
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
		if m.Type == TypeDocument {
			// the state has already moved with the file
			if meta := s.store.Meta(m.NewPath); meta != nil {
				e.UUID, e.Epoch = meta.UUID, meta.Epoch
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// commitMove commits the move with the rewritten documents and mkdocs.yml
func (rs *RestService) commitMove(op *moveOp, changed []*plannedDocument, configChanged bool) string {
	if rs.git == nil {
		return ""
	}
	verb := "Move"
	if op.rename {
		verb = "Rename"
	}
	paths := []string{op.oldRel, op.newRel}
	var listed []string
	for _, pd := range changed {
		paths = append(paths, pd.newRel)
		listed = append(listed, pd.newRel)
	}
	sort.Strings(listed)
	if configChanged {
		if rel, err := filepath.Rel(rs.treeManager.RootPath(), configuration.CurrentConfig.MkDocs.ConfigFile); err == nil {
			paths = append(paths, filepath.ToSlash(rel))
			listed = append(listed, filepath.Base(rel))
		}
	}
	body := ""
	if len(listed) > 0 {
		noun := "documents"
		if len(changed) == 1 {
			noun = "document"
		}
		body = fmt.Sprintf("Update references in %d %s:\n- %s", len(changed), noun, strings.Join(listed, "\n- "))
	}
	return rs.git.CommitTreeChangeWithBody(op.user, fmt.Sprintf("%s %s to %s", verb, op.oldRel, op.newRel), body, paths...)
}

// auditMove writes the audit log entry of a move
func (rs *RestService) auditMove(c echo.Context, op *moveOp, result *MoveResult) {
	if rs.authenticator == nil {
		return
	}
	eventType := auth.AuditItemMoved
	if op.rename {
		eventType = auth.AuditItemRenamed
	}
	actorID := ""
	if op.user != nil {
		actorID = op.user.ID
	}
	info := requestInfo(c)
	rs.authenticator.Audit(auth.AuditEvent{Type: eventType, ActorID: actorID, IP: info.IP, UserAgent: info.UserAgent,
		Details: map[string]any{
			"type":      op.itemType,
			"oldPath":   op.oldRel,
			"newPath":   op.newRel,
			"items":     len(result.Moved),
			"documents": len(result.Documents),
		}})
}
