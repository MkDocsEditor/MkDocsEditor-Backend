package collab

import (
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	automerge "github.com/automerge/automerge-go"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// AuthorshipRun attributes the text range [Start, End) (Unicode code points) to an actor
type AuthorshipRun struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	ActorID   string `json:"actorId"`
	AccountID string `json:"accountId"`
	Username  string `json:"username"`
}

// Authorship is the response of GET /document/{id}/authorship/
type Authorship struct {
	Heads []string        `json:"heads"`
	Runs  []AuthorshipRun `json:"runs"`
}

type authorRun struct {
	length int
	actor  string
}

// authorshipState replays the history of a document and keeps the author runs of its text.
// It can be advanced with later changes.
type authorshipState struct {
	scratch *automerge.Doc
	runs    []authorRun
	text    string
	heads   []automerge.ChangeHash
}

func newAuthorshipState() *authorshipState {
	return &authorshipState{scratch: automerge.New()}
}

// advance applies changes (in causal order) and attributes the text they insert
func (a *authorshipState) advance(changes []*automerge.Change) error {
	for _, ch := range changes {
		// Apply with change objects of another document does not replay step by step in
		// automerge-go, loading the encoded change does
		if err := a.scratch.LoadIncremental(ch.Save()); err != nil {
			return err
		}
		text, err := ContentText(a.scratch)
		if err != nil {
			// no text content yet (or not any more)
			text = ""
		}
		if text != a.text {
			a.runs = applyAuthorDiff(a.runs, a.text, text, ch.ActorID())
			a.text = text
		}
	}
	a.heads = a.scratch.Heads()
	return nil
}

// result returns the runs with the users. owner returns the current username and the
// account id of an actor.
func (a *authorshipState) result(owner func(actorID string) (string, string)) *Authorship {
	result := &Authorship{Heads: formatHeads(a.heads), Runs: []AuthorshipRun{}}
	pos := 0
	for _, r := range a.runs {
		if r.length == 0 {
			continue
		}
		if n := len(result.Runs); n > 0 && result.Runs[n-1].ActorID == r.actor {
			result.Runs[n-1].End += r.length
		} else {
			username, accountID := owner(r.actor)
			result.Runs = append(result.Runs, AuthorshipRun{Start: pos, End: pos + r.length, ActorID: r.actor, AccountID: accountID, Username: username})
		}
		pos += r.length
	}
	return result
}

func fullAuthorshipState(doc *automerge.Doc) (*authorshipState, error) {
	changes, err := doc.Changes()
	if err != nil {
		return nil, err
	}
	state := newAuthorshipState()
	if err = state.advance(changes); err != nil {
		return nil, err
	}
	return state, nil
}

// applyAuthorDiff updates the author runs for a text change from oldText to newText by actor
func applyAuthorDiff(runs []authorRun, oldText string, newText string, actor string) []authorRun {
	diffs := dmp.DiffMain(oldText, newText, false)
	out := make([]authorRun, 0, len(runs)+2)
	i, offset := 0, 0 // current run and position inside it

	take := func(n int, keep bool) {
		for n > 0 && i < len(runs) {
			avail := runs[i].length - offset
			k := min(avail, n)
			if keep {
				out = appendRun(out, authorRun{length: k, actor: runs[i].actor})
			}
			n -= k
			offset += k
			if offset == runs[i].length {
				i++
				offset = 0
			}
		}
	}
	for _, d := range diffs {
		n := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			take(n, true)
		case diffmatchpatch.DiffDelete:
			take(n, false)
		case diffmatchpatch.DiffInsert:
			out = appendRun(out, authorRun{length: n, actor: actor})
		}
	}
	return out
}

func appendRun(runs []authorRun, r authorRun) []authorRun {
	if r.length == 0 {
		return runs
	}
	if n := len(runs); n > 0 && runs[n-1].actor == r.actor {
		runs[n-1].length += r.length
		return runs
	}
	return append(runs, r)
}

// Authorship returns the per-character authorship of a document. The document is loaded
// (and its file content merged) if needed. A few new changes are applied right here, anything
// longer runs in a job without holding the locks, see authorship_snapshot.go.
func (s *SyncServer) Authorship(d *wiki.Document) (*Authorship, error) {
	var last *Authorship
	for range authorshipAttempts {
		var result *Authorship
		var job *authorshipJob
		err := s.withSession(d, func(session *docSession) error {
			result, job = session.authorshipOrJobLocked()
			return nil
		})
		if err != nil || result != nil {
			return result, err
		}
		if job == nil {
			return nil, errAuthorshipStopped
		}
		<-job.done
		if job.err != nil {
			return nil, job.err
		}
		// new changes may have arrived while it ran, so look again
		last = job.result
	}
	// the document changes faster than this can catch up. The result of the job is a little
	// behind, which its heads tell.
	return last, nil
}

// ReplayAuthorship computes the authorship of a document from its whole history, without the
// state Authorship keeps. The result must always equal that of Authorship.
func (s *SyncServer) ReplayAuthorship(d *wiki.Document) (*Authorship, error) {
	var result *Authorship
	err := s.withSession(d, func(session *docSession) error {
		state, err := fullAuthorshipState(session.doc)
		if err != nil {
			return err
		}
		result = state.result(s.actorOwner)
		return nil
	})
	return result, err
}

// actorOwner returns the current username and the account id of an actor
func (s *SyncServer) actorOwner(actorID string) (string, string) {
	return s.subjectUsername(s.actors.Owner(actorID))
}
