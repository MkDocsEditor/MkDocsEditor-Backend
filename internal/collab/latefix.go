package collab

import (
	"log"
	"sort"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
)

// movedReference describes where a broken reference's old path went
type movedReference struct {
	Item        *references.ItemRef
	Destination string
}

// MovedTarget returns the item a broken destination's path was moved to, and the destination
// that points there from the document at sourcePath, see docs/references.md section 7
func (s *SyncServer) MovedTarget(sourcePath string, d references.Destination) (movedReference, bool) {
	if d.Outside || !s.redirects.hasEntries() {
		return movedReference{}, false
	}
	id := s.treeManager.IDOfRelPath(d.Path)
	item, _, ok := s.ResolveRedirect(id, "")
	if !ok {
		return movedReference{}, false
	}
	return movedReference{Item: item, Destination: references.RewriteDestination(d, sourcePath, item.Path)}, true
}

// lateFixEdits finds the broken references of a text whose path is the old path of a moved item
func (s *SyncServer) lateFixEdits(text string, sourcePath string) []references.TextEdit {
	if !s.redirects.hasEntries() {
		return nil
	}
	var lookup *references.TreeLookup
	var edits []references.TextEdit
	for _, r := range references.ParseReferences(text) {
		d, ok := references.ParseDestination(sourcePath, r.DestinationText())
		if !ok || d.Outside {
			continue
		}
		if lookup == nil {
			lookup = references.NewTreeLookup(s.treeManager.Root())
		}
		if lookup.Resolve(d.Path) != nil {
			continue
		}
		moved, ok := s.MovedTarget(sourcePath, d)
		if !ok {
			continue
		}
		edits = append(edits, references.TextEdit{Start: r.Start, End: r.End, Replacement: r.EscapeReplacement(moved.Destination), Line: r.Line})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
	return edits
}

// applyReferenceFixesLocked merges the edits into the document as a change of a server actor.
// Caller holds mu.
func (ds *docSession) applyReferenceFixesLocked(text string, edits []references.TextEdit) error {
	fork, err := ds.forkForServerChangeLocked(ActorUserServer)
	if err != nil {
		return err
	}
	if err = references.SpliceEdits(fork.Path(ContentKey).Text(), text, edits); err != nil {
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
