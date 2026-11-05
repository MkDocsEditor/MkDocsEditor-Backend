package collab

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
	automerge "github.com/automerge/automerge-go"
)

const (
	// authorshipSnapshotVersion invalidates all snapshots on disk. Increase it whenever a change
	// to the replay or the diff gives other runs for the same history, or the wrong runs would
	// stay forever.
	authorshipSnapshotVersion = 1

	// authorshipInlineChanges is how many new changes a request applies while it holds the
	// locks. A request of a document that is not loaded holds the lock of all documents.
	authorshipInlineChanges = 100
	// authorshipSnapshotChanges is how many new changes start a job while a document stays loaded
	authorshipSnapshotChanges = 200
	// authorshipBatchChanges is how many changes a job applies before it looks at the shutdown
	authorshipBatchChanges = 500
	// authorshipParallelJobs limits the jobs that run at once, a move can touch many documents
	authorshipParallelJobs = 2
	// authorshipAttempts limits how often a request waits for a job
	authorshipAttempts = 3
)

var errAuthorshipStopped = errors.New("server is shutting down")

// authorshipSnapshot is the replay state on disk. It has no text: the document has it at heads.
type authorshipSnapshot struct {
	Version int              `json:"version"`
	Epoch   string           `json:"epoch"`
	Heads   []string         `json:"heads"`
	Runs    []snapshottedRun `json:"runs"`
}

type snapshottedRun struct {
	Length int    `json:"length"`
	Actor  string `json:"actor"`
}

func snapshotOf(a *authorshipState, epoch string) *authorshipSnapshot {
	runs := make([]snapshottedRun, 0, len(a.runs))
	for _, r := range a.runs {
		runs = append(runs, snapshottedRun{Length: r.length, Actor: r.actor})
	}
	return &authorshipSnapshot{Version: authorshipSnapshotVersion, Epoch: epoch, Heads: formatHeads(a.heads), Runs: runs}
}

// usable checks everything that can be checked without the document
func (snapshot *authorshipSnapshot) usable(epoch string) ([]automerge.ChangeHash, error) {
	if snapshot.Version != authorshipSnapshotVersion {
		return nil, fmt.Errorf("version %d", snapshot.Version)
	}
	if snapshot.Epoch != epoch {
		return nil, errors.New("another epoch")
	}
	if len(snapshot.Heads) == 0 {
		return nil, errors.New("no heads")
	}
	return parseHeads(snapshot.Heads)
}

func (snapshot *authorshipSnapshot) runs(text string) ([]authorRun, error) {
	runs := make([]authorRun, 0, len(snapshot.Runs))
	total := 0
	for _, r := range snapshot.Runs {
		if r.Length <= 0 {
			return nil, fmt.Errorf("a run of length %d", r.Length)
		}
		total += r.Length
		runs = append(runs, authorRun{length: r.Length, actor: r.Actor})
	}
	if n := utf8.RuneCountInString(text); total != n {
		return nil, fmt.Errorf("runs of %d code points for a text of %d", total, n)
	}
	return runs, nil
}

// restoreAuthorshipState rebuilds the replay state of a snapshot. An error means the snapshot
// cannot be used for this document, and the history has to be replayed.
func restoreAuthorshipState(doc *automerge.Doc, snapshot *authorshipSnapshot, epoch string) (*authorshipState, error) {
	heads, err := snapshot.usable(epoch)
	if err != nil {
		return nil, err
	}
	scratch, err := doc.Fork(heads...)
	if err != nil {
		return nil, err
	}
	text, err := ContentText(scratch)
	if err != nil {
		// no text content yet at these heads, like in advance
		text = ""
	}
	runs, err := snapshot.runs(text)
	if err != nil {
		return nil, err
	}
	return &authorshipState{scratch: scratch, runs: runs, text: text, heads: heads}, nil
}

// readAuthorshipSnapshot returns the snapshot of a document, or nil if there is no usable one
func (s *SyncServer) readAuthorshipSnapshot(uuid string) *authorshipSnapshot {
	data, err := s.store.LoadAuthorship(uuid)
	if err != nil {
		log.Printf("Unable to read the authorship snapshot %s: %v", uuid, err)
		return nil
	}
	if data == nil {
		return nil
	}
	var snapshot authorshipSnapshot
	if err = json.Unmarshal(data, &snapshot); err != nil {
		log.Printf("Ignoring the authorship snapshot %s: %v", uuid, err)
		metrics.AuthorshipSnapshotsRejected.Inc()
		return nil
	}
	return &snapshot
}

// currentSnapshotStateLocked returns the state of the snapshot on disk if it is at the current
// heads of the document. Then neither the history nor a copy of the document is needed, so this
// is what makes opening a document cheap. Caller holds mu.
func (ds *docSession) currentSnapshotStateLocked() *authorshipState {
	snapshot := ds.server.readAuthorshipSnapshot(ds.meta.UUID)
	if snapshot == nil {
		return nil
	}
	heads, err := snapshot.usable(ds.meta.Epoch)
	if err != nil || !sameHeads(heads, ds.doc.Heads()) {
		return nil
	}
	text, err := ds.text()
	if err != nil {
		text = ""
	}
	runs, err := snapshot.runs(text)
	if err != nil {
		return nil
	}
	return &authorshipState{runs: runs, text: text, heads: heads}
}

// authorshipOrJobLocked returns the authorship if that is quick, or else the job that works on it.
// Both are nil when the server is shutting down. Caller holds mu.
func (ds *docSession) authorshipOrJobLocked() (*Authorship, *authorshipJob) {
	if ds.authorship == nil {
		ds.authorship = ds.currentSnapshotStateLocked()
	}
	if state := ds.authorship; state != nil {
		if sameHeads(state.heads, ds.doc.Heads()) {
			return state.result(ds.server.actorOwner), nil
		}
		// a state from a snapshot has no scratch document until a job built one
		if state.scratch != nil {
			changes, err := ds.doc.Changes(state.heads...)
			if err == nil && len(changes) <= authorshipInlineChanges {
				if err = state.advance(changes); err == nil {
					return state.result(ds.server.actorOwner), nil
				}
			}
			if err != nil {
				log.Printf("Unable to advance the authorship of %s: %v", ds.meta.Path, err)
				ds.authorship = nil
			}
		}
	}
	job, _ := ds.startAuthorshipJobLocked(ds.doc.Save())
	return nil, job
}

// authorshipJob brings the snapshot of one document up to date
type authorshipJob struct {
	done chan struct{}
	// valid when done is closed
	result *Authorship
	err    error
}

// startAuthorshipJobLocked starts a job for the given save of the document. A running job of the
// document is returned instead, it was started with an older save. Caller holds mu.
func (ds *docSession) startAuthorshipJobLocked(saved []byte) (*authorshipJob, bool) {
	s := ds.server
	s.authorshipMu.Lock()
	defer s.authorshipMu.Unlock()
	if job := s.authorshipJobs[ds.meta.UUID]; job != nil {
		return job, false
	}
	if s.authorshipStopped {
		return nil, false
	}
	job := &authorshipJob{done: make(chan struct{})}
	s.authorshipJobs[ds.meta.UUID] = job
	s.authorshipWorkers.Add(1)
	ds.authorshipPending = 0
	go s.runAuthorshipJob(job, ds, ds.meta.UUID, ds.meta.Epoch, saved)
	return job, true
}

// snapshotAuthorshipLocked starts a job if the document changed since the last one. A document
// that is being unloaded has its state saved here, so the next request does not pay for it.
// Caller holds mu.
func (ds *docSession) snapshotAuthorshipLocked() {
	if ds.authorshipPending > 0 {
		ds.startAuthorshipJobLocked(ds.doc.Save())
	}
}

func (s *SyncServer) runAuthorshipJob(job *authorshipJob, ds *docSession, uuid string, epoch string, saved []byte) {
	defer s.authorshipWorkers.Done()
	state, err := s.replayFromSnapshot(uuid, epoch, saved)
	if err == nil {
		job.result = state.result(s.actorOwner)
		ds.mu.Lock()
		if !ds.closed {
			ds.authorship = state
		}
		ds.mu.Unlock()
	}
	job.err = err
	s.authorshipMu.Lock()
	delete(s.authorshipJobs, uuid)
	s.authorshipMu.Unlock()
	close(job.done)
}

// replayFromSnapshot continues from the snapshot on disk, or else replays the whole history, and
// writes the new snapshot. It also writes what it got so far when the server shuts down, so a long
// replay does not start over.
func (s *SyncServer) replayFromSnapshot(uuid string, epoch string, saved []byte) (*authorshipState, error) {
	select {
	case s.authorshipSlots <- struct{}{}:
		defer func() { <-s.authorshipSlots }()
	case <-s.stop:
		return nil, errAuthorshipStopped
	}
	start := time.Now()
	doc, err := automerge.Load(saved)
	if err != nil {
		return nil, err
	}

	mode := metrics.AuthorshipReplayFull
	state := newAuthorshipState()
	var changes []*automerge.Change
	if snapshot := s.readAuthorshipSnapshot(uuid); snapshot != nil {
		restored, err := restoreAuthorshipState(doc, snapshot, epoch)
		if err == nil {
			changes, err = doc.Changes(restored.heads...)
		}
		if err == nil {
			mode, state = metrics.AuthorshipReplayResume, restored
		} else {
			log.Printf("Replaying the history instead of using the authorship snapshot %s: %v", uuid, err)
			metrics.AuthorshipSnapshotsRejected.Inc()
		}
	}
	if mode == metrics.AuthorshipReplayFull {
		if changes, err = doc.Changes(); err != nil {
			return nil, err
		}
	}

	for len(changes) > 0 {
		select {
		case <-s.stop:
			s.writeAuthorshipSnapshot(uuid, epoch, state)
			return nil, errAuthorshipStopped
		default:
		}
		batch := changes[:min(authorshipBatchChanges, len(changes))]
		changes = changes[len(batch):]
		if err = state.advance(batch); err != nil {
			return nil, err
		}
	}
	s.writeAuthorshipSnapshot(uuid, epoch, state)
	metrics.AuthorshipReplays.WithLabelValues(mode).Inc()
	metrics.AuthorshipReplayDuration.WithLabelValues(mode).Observe(time.Since(start).Seconds())
	return state, nil
}

func (s *SyncServer) writeAuthorshipSnapshot(uuid string, epoch string, state *authorshipState) {
	if len(state.heads) == 0 {
		return
	}
	data, err := json.Marshal(snapshotOf(state, epoch))
	if err == nil {
		err = s.store.SaveAuthorship(uuid, data)
	}
	if err != nil {
		log.Printf("Unable to save the authorship snapshot %s: %v", uuid, err)
	}
}
