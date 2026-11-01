package backend

import (
	"log"
	"sort"
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
	id := s.treeManager.idOfRelPath(d.path)
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
		d, ok := parseDestination(sourcePath, r.destinationText())
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
		edits = append(edits, textEdit{start: r.Start, end: r.End, replacement: r.escapeReplacement(moved.destination), line: r.Line})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits
}

// applyReferenceFixesLocked merges the edits into the document as a change of a server actor.
// Caller holds mu.
func (ds *docSession) applyReferenceFixesLocked(text string, edits []textEdit) error {
	fork, err := ds.forkForServerChangeLocked(ActorUserServer)
	if err != nil {
		return err
	}
	if err = spliceEdits(fork.Path(contentKey).Text(), text, edits); err != nil {
		return err
	}
	if _, err = fork.Commit("update references to moved items"); err != nil {
		return err
	}
	_, err = ds.doc.Merge(fork)
	return err
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
	if err = ds.applyReferenceFixesLocked(text, edits); err != nil {
		log.Printf("Unable to update the references to moved items in %s: %v", ds.meta.Path, err)
		return
	}
	log.Printf("Updated %d references to moved items in %s", len(edits), ds.meta.Path)
	ds.broadcastSyncLocked()
}
