package collab

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	automerge "github.com/automerge/automerge-go"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Where people are, for agents: clients send their position as code point indexes too (at in
// presence), because automerge-go cannot resolve Automerge cursors. See docs/sync-protocol.md
// section 6, Indexes.

// maxPresenceIndexesSize is the largest at of a presence that is kept
const maxPresenceIndexesSize = 4096

type (
	// PresenceIndexes is the position of a client as code point indexes of the text at Revision
	PresenceIndexes struct {
		Revision   string           `json:"revision"`
		Selections []IndexSelection `json:"selections"`
		Viewport   *FocusViewport   `json:"viewport,omitempty"`
	}

	// IndexSelection is a selection as indexes, Anchor == Head a cursor
	IndexSelection struct {
		Anchor int `json:"anchor"`
		Head   int `json:"head"`
	}

	// ViewerPosition is where a connection of a person is, mapped to the current text
	ViewerPosition struct {
		AccountID  string
		DeviceName string
		LastActive time.Time
		// Known is false if the client sent no indexes: the position is unknown
		Known bool
		// Approximate is true if the revision of the indexes was unknown and the indexes were
		// applied to the current text
		Approximate bool
		Selections  []IndexSelection
		Viewport    *FocusViewport
	}
)

// parsePresenceIndexes returns the at of a presence message, nil if it is missing, too large or
// invalid: a presence without valid indexes clears them
func parsePresenceIndexes(data []byte) *PresenceIndexes {
	var m struct {
		At json.RawMessage `json:"at"`
	}
	if json.Unmarshal(data, &m) != nil || len(m.At) == 0 || len(m.At) > maxPresenceIndexesSize ||
		bytes.Equal(bytes.TrimSpace(m.At), jsonNull) {
		return nil
	}
	var at PresenceIndexes
	if json.Unmarshal(m.At, &at) != nil || len(at.Selections) > 100 {
		return nil
	}
	if _, err := ParseRevision(at.Revision); err != nil {
		return nil
	}
	for _, s := range at.Selections {
		if s.Anchor < 0 || s.Head < 0 {
			return nil
		}
	}
	if v := at.Viewport; v != nil && (v.Top < 0 || v.Bottom < v.Top) {
		return nil
	}
	return &at
}

// ViewerPositions returns the current text and revision of a document and where the connections
// of people are in it, the most recently active first. It never loads a document: nobody open
// is ErrNobodyOpen.
func (s *SyncServer) ViewerPositions(d *wiki.Document) (string, string, []ViewerPosition, error) {
	var (
		content, revision string
		out               []ViewerPosition
	)
	err := s.withOpenSession(d, func(ds *docSession) error {
		var err error
		if content, err = ContentText(ds.doc); err != nil {
			return err
		}
		revision = FormatRevision(ds.doc.Heads())
		length := utf8.RuneCountInString(content)
		// one diff per revision, several connections usually share one
		maps := map[string]*indexMap{}
		for c := range ds.conns {
			p := ViewerPosition{AccountID: c.user.ID, DeviceName: c.deviceName, LastActive: c.lastActive}
			if at := c.at; at != nil {
				p.Known = true
				m, ok := maps[at.Revision]
				if !ok {
					m = ds.indexMapLocked(at.Revision, content)
					maps[at.Revision] = m
				}
				p.Approximate = m == nil
				p.Selections = make([]IndexSelection, len(at.Selections))
				for i, sel := range at.Selections {
					p.Selections[i] = IndexSelection{Anchor: m.mapIndex(sel.Anchor, length), Head: m.mapIndex(sel.Head, length)}
				}
				if v := at.Viewport; v != nil {
					top, bottom := m.mapIndex(v.Top, length), m.mapIndex(v.Bottom, length)
					if length > 0 {
						top, bottom = min(top, length-1), min(bottom, length-1)
					}
					p.Viewport = &FocusViewport{Top: top, Bottom: max(bottom, top)}
				}
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return "", "", nil, err
	}
	slices.SortStableFunc(out, func(a, b ViewerPosition) int { return b.LastActive.Compare(a.LastActive) })
	return content, revision, out, nil
}

// indexMap maps code point indexes of an older text to the current one: the diffs between
// them. nil maps by clamping only.
type indexMap struct {
	diffs []diffmatchpatch.Diff
}

// indexMapLocked returns the map from the text at revision to current, nil if the session does
// not have every head of revision. Caller holds ds.mu.
func (ds *docSession) indexMapLocked(revision string, current string) *indexMap {
	heads, err := ds.baseHeadsLocked(revision)
	if err != nil || heads == nil {
		return nil
	}
	var old string
	// FormatRevision sorts: clients may send the heads in any order
	if FormatRevision(heads) == FormatRevision(ds.doc.Heads()) {
		old = current
	} else {
		var fork *automerge.Doc
		if fork, err = ds.doc.Fork(heads...); err != nil {
			return nil
		}
		if old, err = ContentText(fork); err != nil {
			return nil
		}
	}
	if old == current {
		return &indexMap{}
	}
	return &indexMap{diffs: dmp.DiffCleanupSemantic(dmp.DiffMain(old, current, false))}
}

// mapIndex maps an index of the old text to the current text of length length. An index in
// deleted text goes to the place of the deletion. Text inserted at an index comes before it,
// like an Automerge cursor, which points at the character after it.
func (m *indexMap) mapIndex(index int, length int) int {
	if m == nil || m.diffs == nil {
		return min(index, length)
	}
	oldPos, newPos := 0, 0
	for _, d := range m.diffs {
		n := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			if index < oldPos+n {
				return newPos + index - oldPos
			}
			oldPos += n
			newPos += n
		case diffmatchpatch.DiffDelete:
			if index < oldPos+n {
				return newPos
			}
			oldPos += n
		case diffmatchpatch.DiffInsert:
			newPos += n
		}
	}
	return min(newPos+index-oldPos, length)
}
