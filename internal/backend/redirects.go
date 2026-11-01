package backend

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	mutexSync "sync"
	"time"
)

const redirectsFile = "redirects.json"

// RedirectEntry records that an item moved, see docs/references.md section 7
type RedirectEntry struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	OldPath string    `json:"oldPath"`
	NewPath string    `json:"newPath"`
	// UUID and Epoch identify the Automerge state of a document, if it had one
	UUID      string `json:"uuid,omitempty"`
	Epoch     string `json:"epoch,omitempty"`
	AccountID string `json:"accountId,omitempty"`
}

// RedirectStore is the durable, append-only table of moved items in the state directory.
// It maps the ids of old paths to the items' current paths.
type RedirectStore struct {
	file string
	// idOf derives the id of a path relative to the docs folder
	idOf func(relPath string) string

	mu      mutexSync.Mutex
	entries []RedirectEntry
	// byOldID maps an old id to the indexes of its entries, oldest first
	byOldID map[string][]int
}

// NewRedirectStore loads the redirect table of the state directory
func NewRedirectStore(stateDir string, idOf func(relPath string) string) (*RedirectStore, error) {
	rs := &RedirectStore{file: filepath.Join(stateDir, redirectsFile), idOf: idOf, byOldID: map[string][]int{}}
	data, err := os.ReadFile(rs.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, &rs.entries); err != nil {
			// never overwrite a table that cannot be read: keep it aside and start a new one
			aside := rs.file + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
			log.Printf("The redirect table %s cannot be read, moving it to %s: %v", rs.file, aside, err)
			if rerr := os.Rename(rs.file, aside); rerr != nil {
				return nil, rerr
			}
			rs.entries = nil
		}
	}
	for i, e := range rs.entries {
		id := idOf(e.OldPath)
		rs.byOldID[id] = append(rs.byOldID[id], i)
	}
	return rs, nil
}

// Append adds entries and writes the table. On error nothing is added.
func (rs *RedirectStore) Append(entries ...RedirectEntry) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	all := append(append([]RedirectEntry(nil), rs.entries...), entries...)
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err = WriteFile(rs.file, data); err != nil {
		return err
	}
	for i := len(rs.entries); i < len(all); i++ {
		id := rs.idOf(all[i].OldPath)
		rs.byOldID[id] = append(rs.byOldID[id], i)
	}
	rs.entries = all
	return nil
}

// redirectResolver answers where an item is now
type redirectResolver struct {
	// stateByUUID returns the current path and epoch of a document's Automerge state, ok is
	// false if it is not active (deleted or unknown)
	stateByUUID func(uuid string) (path string, epoch string, ok bool)
	// exists reports whether an item of the type exists at the path
	exists func(itemType string, relPath string) bool
}

// resolved is the result of a redirect lookup
type resolved struct {
	Entry RedirectEntry
	Path  string
	Epoch string
}

// Resolve returns where the item with the old id is now, trying the newest entries first.
// With epoch set, only documents whose Automerge state has that epoch match.
func (rs *RedirectStore) Resolve(id string, epoch string, r redirectResolver) (resolved, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	indexes := rs.byOldID[id]
	for k := len(indexes) - 1; k >= 0; k-- {
		i := indexes[k]
		e := rs.entries[i]
		if e.UUID != "" && r.stateByUUID != nil {
			if p, ep, ok := r.stateByUUID(e.UUID); ok && (epoch == "" || ep == epoch) && r.exists(e.Type, p) {
				return resolved{Entry: e, Path: p, Epoch: ep}, true
			}
			continue
		}
		if epoch != "" {
			continue
		}
		// follow the later moves of the path
		current := e.NewPath
		for j := i + 1; j < len(rs.entries); j++ {
			if later := rs.entries[j]; later.Type == e.Type && later.OldPath == current {
				current = later.NewPath
			}
		}
		if r.exists(e.Type, current) {
			return resolved{Entry: e, Path: current}, true
		}
	}
	return resolved{}, false
}

// resolveRedirect returns the current item of an old id. With epoch set, only a document whose
// Automerge state has that epoch matches.
func (s *SyncServer) resolveRedirect(id string, epoch string) (*ItemRef, RedirectEntry, bool) {
	lookup := newTreeLookup(s.treeManager.Root())
	itemAt := func(itemType string, p string) *ItemRef {
		switch itemType {
		case TypeDocument:
			if d := lookup.documents[p]; d != nil {
				return documentRef(d)
			}
		case TypeResource:
			if r := lookup.resources[p]; r != nil {
				return resourceRef(r)
			}
		case TypeSection:
			if sec := lookup.sections[p]; sec != nil {
				return sectionRef(sec)
			}
		}
		return nil
	}
	res, ok := s.redirects.Resolve(id, epoch, redirectResolver{
		stateByUUID: func(uuid string) (string, string, bool) {
			meta := s.store.ByUUID(uuid)
			if meta == nil {
				return "", "", false
			}
			return meta.Path, meta.Epoch, true
		},
		exists: func(itemType string, p string) bool { return itemAt(itemType, p) != nil },
	})
	if !ok {
		return nil, RedirectEntry{}, false
	}
	item := itemAt(res.Entry.Type, res.Path)
	if item == nil || item.ID == id {
		return nil, RedirectEntry{}, false
	}
	return item, res.Entry, true
}

// documentRedirect decides whether a websocket client that sent hello for documentID with
// epoch must go to another document, see docs/references.md section 7. It returns the id to
// redirect to, or "".
func (s *SyncServer) documentRedirect(documentID string, epoch string) string {
	d := s.treeManager.GetDocument(documentID)
	if d == nil {
		if epoch != "" {
			if item, _, ok := s.resolveRedirect(documentID, epoch); ok && item.Type == TypeDocument {
				return item.ID
			}
		}
		if item, _, ok := s.resolveRedirect(documentID, ""); ok && item.Type == TypeDocument {
			return item.ID
		}
		return ""
	}
	if epoch == "" || s.documentEpoch(d) == epoch {
		return ""
	}
	// the client holds another lineage: the document that moved away from this path
	if item, _, ok := s.resolveRedirect(documentID, epoch); ok && item.Type == TypeDocument {
		return item.ID
	}
	return ""
}

// documentEpoch returns the epoch of a document's Automerge state, or "" if it has none
func (s *SyncServer) documentEpoch(d *Document) string {
	rel, err := s.relPath(d.Path)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	session := s.sessions[rel]
	s.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		defer session.mu.Unlock()
		if !session.closed {
			return session.meta.Epoch
		}
	}
	if meta := s.store.Meta(rel); meta != nil {
		return meta.Epoch
	}
	return ""
}
