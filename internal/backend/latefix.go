package backend

import (
	"html"
	"log"
	"path/filepath"
	"sort"
	"strings"
)

// movedReference describes where a broken reference's old path went
type movedReference struct {
	item        *ItemRef
	destination string
}

// movedTarget returns the item a broken destination's path was moved to, and the destination
// that points there from the document at sourcePath, see docs/references.md section 7
func (s *SyncServer) movedTarget(sourcePath string, d destination) (movedReference, bool) {
	if d.outside || !s.redirects.hasEntries() {
		return movedReference{}, false
	}
	id := s.treeManager.GenerateID(filepath.Join(s.treeManager.RootPath(), filepath.FromSlash(d.path)))
	item, _, ok := s.resolveRedirect(id, "")
	if !ok {
		return movedReference{}, false
	}
	return movedReference{item: item, destination: rewriteDestination(d, sourcePath, item.Path)}, true
}

// lateFixEdits finds the broken references of a text whose path is the old path of a moved item
func (s *SyncServer) lateFixEdits(text string, sourcePath string) []textEdit {
	if !s.redirects.hasEntries() {
		return nil
	}
	var lookup *treeLookup
	var edits []textEdit
	for _, r := range parseReferences(text) {
		raw := r.Raw
		if r.HTML {
			raw = html.UnescapeString(raw)
		}
		d, ok := parseDestination(sourcePath, raw)
		if !ok || d.outside {
			continue
		}
		if lookup == nil {
			lookup = newTreeLookup(s.treeManager.Root())
		}
		if lookup.resolve(d.path) != nil {
			continue
		}
		moved, ok := s.movedTarget(sourcePath, d)
		if !ok {
			continue
		}
		replacement := moved.destination
		if r.HTML && strings.Contains(r.Raw, "&amp;") {
			replacement = strings.ReplaceAll(replacement, "&", "&amp;")
		}
		edits = append(edits, textEdit{start: r.Start, end: r.End, replacement: replacement, line: r.Line})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits
}

// fixMovedReferencesLocked rewrites references to old paths of moved items, as a change of a
// server actor, before the document is written to disk. Offline clients can bring such
// references after a move. Caller holds mu.
func (ds *docSession) fixMovedReferencesLocked() {
	text, err := ds.text()
	if err != nil {
		return
	}
	edits := ds.server.lateFixEdits(text, ds.meta.Path)
	if len(edits) == 0 {
		return
	}
	fork, err := ds.doc.Fork()
	if err == nil {
		var actor string
		if actor, err = ds.server.actors.NewServerActor(ActorUserServer); err == nil {
			err = fork.SetActorID(actor)
			if err == nil {
				ds.announceActorLocked(actor, ActorUserServer, nil)
			}
		}
	}
	if err == nil {
		err = spliceEdits(fork.Path(contentKey).Text(), text, edits)
	}
	if err == nil {
		_, err = fork.Commit("update references to moved items")
	}
	if err == nil {
		_, err = ds.doc.Merge(fork)
	}
	if err != nil {
		log.Printf("Unable to update the references to moved items in %s: %v", ds.meta.Path, err)
		return
	}
	log.Printf("Updated %d references to moved items in %s", len(edits), ds.meta.Path)
	ds.broadcastSyncLocked()
}
