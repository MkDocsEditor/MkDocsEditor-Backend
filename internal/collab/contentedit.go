package collab

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	automerge "github.com/automerge/automerge-go"
)

// Content changes over the REST API (and the MCP server): edits by unique search strings and
// writes of the full text against a base revision. Both are applied inside the live Automerge
// document as splices of an actor of the editing account, so connected clients see them like
// any other change and concurrent typing merges. See docs/content-api.md.

// Errors of the content API
var (
	// ErrUnknownRevision is returned for a revision whose changes the document does not have,
	// for example after the document started a new epoch
	ErrUnknownRevision = errors.New("the document does not know this revision")
	// ErrInvalidRevision is returned for a revision that is no list of change hashes
	ErrInvalidRevision = errors.New("invalid revision")
)

// Edit error codes, see EditError
const (
	EditNotFound  = "not-found"
	EditAmbiguous = "ambiguous"
	EditEmptyOld  = "empty-old"
)

// EditError is returned when an edit of ApplyEdits cannot be placed. No edit is applied then.
type EditError struct {
	// Code is EditNotFound, EditAmbiguous or EditEmptyOld
	Code string
	// Index is the position of the edit in the request
	Index int
	// Hint is a near miss of an edit that was not found, "" for none
	Hint string
}

func (e *EditError) Error() string {
	switch e.Code {
	case EditNotFound:
		if e.Hint != "" {
			return fmt.Sprintf("edit %d: old does not occur in the text; %s", e.Index, e.Hint)
		}
		return fmt.Sprintf("edit %d: old does not occur in the text", e.Index)
	case EditAmbiguous:
		return fmt.Sprintf("edit %d: old occurs more than once in the text, add context to make it unique", e.Index)
	}
	return fmt.Sprintf("edit %d: old is empty", e.Index)
}

// TextEdit replaces the only occurrence of Old by New
type TextEdit struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// Agent is who changes content over the API: an account, and the app token it used, if any.
type Agent struct {
	AccountID string
	// TokenID and TokenName are empty for requests of a session
	TokenID   string
	TokenName string
}

// key identifies the actor of the agent in a session
func (a Agent) key() string {
	if a.TokenID != "" {
		return "token:" + a.TokenID
	}
	return "account:" + a.AccountID
}

// apiActor is the actor of an agent in a loaded document, and its last change
type apiActor struct {
	id   string
	last automerge.ChangeHash
}

// FormatRevision returns the revision of heads: the sorted change hashes, joined by dots
func FormatRevision(heads []automerge.ChangeHash) string {
	hexHeads := formatHeads(heads)
	sort.Strings(hexHeads)
	return strings.Join(hexHeads, ".")
}

// ParseRevision reads a revision. Quotes of an ETag are accepted.
func ParseRevision(revision string) ([]automerge.ChangeHash, error) {
	revision = strings.Trim(strings.TrimPrefix(strings.TrimSpace(revision), "W/"), `"`)
	if revision == "" {
		return nil, ErrInvalidRevision
	}
	heads, err := parseHeads(strings.Split(revision, "."))
	if err != nil {
		return nil, ErrInvalidRevision
	}
	return heads, nil
}

// ReadContent returns the current text and revision of a document
func (s *SyncServer) ReadContent(d *wiki.Document) (text string, revision string, err error) {
	err = s.withSession(d, func(ds *docSession) error {
		text, err = ds.text()
		revision = FormatRevision(ds.doc.Heads())
		return err
	})
	return text, revision, err
}

// ApplyEdits applies edits in order: each Old must occur exactly once in the text left by the
// edits before it. Without baseRevision the edits apply to the current text, else to the text
// of that revision, merged with everything that changed since. Either all edits apply or none.
// It returns the new revision.
func (s *SyncServer) ApplyEdits(d *wiki.Document, edits []TextEdit, baseRevision string, agent Agent) (string, error) {
	var revision string
	err := s.withSession(d, func(ds *docSession) error {
		heads, err := ds.baseHeadsLocked(baseRevision)
		if err != nil {
			return err
		}
		fork, err := ds.agentForkLocked(agent, heads)
		if err != nil {
			return err
		}
		text := fork.Path(ContentKey).Text()
		current, err := text.Get()
		if err != nil {
			return err
		}
		splices, err := placeEdits(current, edits)
		if err != nil {
			return err
		}
		for _, sp := range splices {
			if err = text.Splice(sp.pos, sp.del, sp.insert); err != nil {
				return err
			}
		}
		if err = ds.commitAgentForkLocked(fork, agent, fmt.Sprintf("%d edits", len(edits))); err != nil {
			return err
		}
		revision = FormatRevision(ds.doc.Heads())
		return nil
	})
	return revision, err
}

// WriteContent replaces the text of the revision baseRevision by content and merges that into the
// document: the difference between the two is applied as splices on a fork at baseRevision, so
// what others changed since stays. It returns the new revision.
func (s *SyncServer) WriteContent(d *wiki.Document, content string, baseRevision string, agent Agent) (string, error) {
	if !utf8.ValidString(content) {
		return "", ErrNotUTF8
	}
	var revision string
	err := s.withSession(d, func(ds *docSession) error {
		heads, err := ds.baseHeadsLocked(baseRevision)
		if err != nil {
			return err
		}
		fork, err := ds.agentForkLocked(agent, heads)
		if err != nil {
			return err
		}
		base, err := ContentText(fork)
		if err != nil {
			return err
		}
		if err = SpliceDiff(fork.Path(ContentKey).Text(), base, content); err != nil {
			return err
		}
		if err = ds.commitAgentForkLocked(fork, agent, "write"); err != nil {
			return err
		}
		revision = FormatRevision(ds.doc.Heads())
		return nil
	})
	return revision, err
}

// baseHeadsLocked returns the heads of a base revision, nil for none (the current heads)
func (ds *docSession) baseHeadsLocked(revision string) ([]automerge.ChangeHash, error) {
	if revision == "" {
		return nil, nil
	}
	heads, err := ParseRevision(revision)
	if err != nil {
		return nil, err
	}
	for _, h := range heads {
		if _, err = ds.doc.Change(h); err != nil {
			return nil, ErrUnknownRevision
		}
	}
	return heads, nil
}

// agentForkLocked forks the document at heads (nil: now) for a change of an agent. The agent
// keeps its actor while the document is loaded and the fork has the actor's last change, so a
// loop of edits does not create an actor per edit.
func (ds *docSession) agentForkLocked(agent Agent, heads []automerge.ChangeHash) (*automerge.Doc, error) {
	fork, err := ds.doc.Fork(heads...)
	if err != nil {
		return nil, err
	}
	actor, ok := ds.apiActors[agent.key()]
	if ok {
		if _, err = fork.Change(actor.last); err != nil {
			ok = false
		}
	}
	if !ok {
		id, err := ds.server.actors.NewAgentActor(agent)
		if err != nil {
			return nil, err
		}
		ds.announceActorLocked(id, agent.AccountID, nil)
		actor = apiActor{id: id}
	}
	if err = fork.SetActorID(actor.id); err != nil {
		return nil, err
	}
	if ds.apiActors == nil {
		ds.apiActors = map[string]apiActor{}
	}
	ds.apiActors[agent.key()] = actor
	return fork, nil
}

// commitAgentForkLocked commits the changes of an agent on a fork and merges them into the
// document. A fork without changes is dropped.
func (ds *docSession) commitAgentForkLocked(fork *automerge.Doc, agent Agent, message string) error {
	before, err := ContentText(ds.doc)
	if err != nil {
		return err
	}
	hash, err := fork.Commit(message)
	if err != nil && err.Error() == "Commit is empty" {
		// the edits changed nothing
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = ds.doc.Merge(fork); err != nil {
		return err
	}
	actor := ds.apiActors[agent.key()]
	actor.last = hash
	ds.apiActors[agent.key()] = actor
	ds.touchAgentPresenceLocked(agent, actor.id)

	// the document changed even if the text is the same again (edits that undo each other)
	ds.broadcastSyncLocked()
	ds.scheduleFlushLocked()
	after, err := ContentText(ds.doc)
	if err != nil {
		return err
	}
	if after == before {
		return nil
	}
	ds.authorshipPending++
	ds.meta.LastChange = time.Now().UTC()
	ds.addEditorLocked(agent.AccountID)
	if r := ds.server.changeRecorder; r != nil {
		r.RecordChanges(ds.meta.Path, map[string]int{agent.AccountID: 1})
		if agent.TokenName != "" {
			r.RecordAgent(ds.meta.Path, agent.AccountID, agent.TokenName)
		}
	}
	return nil
}

// splice replaces del code points at pos by insert
type splice struct {
	pos    int
	del    int
	insert string
}

// placeEdits turns edits into splices. It works on a copy of the text, so it fails before
// anything changes.
func placeEdits(text string, edits []TextEdit) ([]splice, error) {
	out := make([]splice, 0, len(edits))
	for i, e := range edits {
		if e.Old == "" {
			if text != "" {
				return nil, &EditError{Code: EditEmptyOld, Index: i}
			}
			// an empty document: the empty string is its only content
			if e.New != "" {
				out = append(out, splice{pos: 0, insert: e.New})
			}
			text = e.New
			continue
		}
		at := strings.Index(text, e.Old)
		if at < 0 {
			return nil, &EditError{Code: EditNotFound, Index: i, Hint: notFoundHint(text, e.Old)}
		}
		// overlapping occurrences count too: "aa" occurs twice in "aaa"
		if strings.Contains(text[at+1:], e.Old) {
			return nil, &EditError{Code: EditAmbiguous, Index: i}
		}
		if e.Old != e.New {
			out = append(out, splice{pos: utf8.RuneCountInString(text[:at]), del: utf8.RuneCountInString(e.Old), insert: e.New})
		}
		text = text[:at] + e.New + text[at+len(e.Old):]
	}
	return out, nil
}
