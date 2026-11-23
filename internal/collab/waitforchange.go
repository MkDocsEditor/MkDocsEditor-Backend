package collab

import (
	"context"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// waitRecheck is how often WaitForChange looks at the document again, for sessions that close
// or unload while it waits
const waitRecheck = time.Second

// WaitForChange waits until the revision of a document that people have open is no longer
// revision, or until ctx ends. It returns the current revision and whether it changed. It never
// loads a document: nobody open, also when the last connection leaves while it waits, is
// ErrNobodyOpen.
func (s *SyncServer) WaitForChange(ctx context.Context, d *wiki.Document, revision string) (string, bool, error) {
	ticker := time.NewTicker(waitRecheck)
	defer ticker.Stop()
	for {
		var (
			current string
			changed <-chan struct{}
		)
		err := s.withOpenSession(d, func(ds *docSession) error {
			current = FormatRevision(ds.doc.Heads())
			if current == revision {
				if ds.changed == nil {
					ds.changed = make(chan struct{})
					ds.changedRevision = current
				}
				changed = ds.changed
			}
			return nil
		})
		if err != nil {
			return "", false, err
		}
		if changed == nil {
			return current, true, nil
		}
		select {
		case <-changed:
		case <-ticker.C:
		case <-ctx.Done():
			return current, false, nil
		}
	}
}

// OpenRevision returns the current revision of a document that people have open. It never
// loads a document: nobody open is ErrNobodyOpen.
func (s *SyncServer) OpenRevision(d *wiki.Document) (string, error) {
	var revision string
	err := s.withOpenSession(d, func(ds *docSession) error {
		revision = FormatRevision(ds.doc.Heads())
		return nil
	})
	return revision, err
}
