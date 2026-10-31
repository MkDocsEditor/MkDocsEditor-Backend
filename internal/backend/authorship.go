package backend

import (
	"unicode/utf8"

	automerge "github.com/automerge/automerge-go"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// AuthorshipRun attributes the text range [Start, End) (Unicode code points) to an actor
type AuthorshipRun struct {
	Start    int    `json:"start"`
	End      int    `json:"end"`
	ActorID  string `json:"actorId"`
	Username string `json:"username"`
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
		text, err := contentText(a.scratch)
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

// result returns the runs with usernames
func (a *authorshipState) result(owner func(actorID string) string) *Authorship {
	result := &Authorship{Heads: formatHeads(a.heads), Runs: []AuthorshipRun{}}
	pos := 0
	for _, r := range a.runs {
		if r.length == 0 {
			continue
		}
		if n := len(result.Runs); n > 0 && result.Runs[n-1].ActorID == r.actor {
			result.Runs[n-1].End += r.length
		} else {
			result.Runs = append(result.Runs, AuthorshipRun{Start: pos, End: pos + r.length, ActorID: r.actor, Username: owner(r.actor)})
		}
		pos += r.length
	}
	return result
}

// computeAuthorship replays the whole history of a document
func computeAuthorship(doc *automerge.Doc, owner func(actorID string) string) (*Authorship, error) {
	state, err := fullAuthorshipState(doc)
	if err != nil {
		return nil, err
	}
	return state.result(owner), nil
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
// (and its file content merged) if needed. While the document is loaded, the replay state is
// kept and only advanced by new changes.
func (s *SyncServer) Authorship(d *Document) (*Authorship, error) {
	var result *Authorship
	var fork *automerge.Doc
	err := s.withSession(d, func(session *docSession) error {
		if state := session.authorship; state != nil {
			changes, err := session.doc.Changes(state.heads...)
			if err == nil && len(changes) < 1000 {
				if err = state.advance(changes); err == nil {
					result = state.result(s.actors.Owner)
					return nil
				}
			}
			session.authorship = nil
		}
		var err error
		fork, err = session.doc.Fork()
		return err
	})
	if err != nil || result != nil {
		return result, err
	}

	// replaying the whole history takes a while, so it runs without holding the session lock
	state, err := fullAuthorshipState(fork)
	if err != nil {
		return nil, err
	}
	if rel, err := s.relPath(d.Path); err == nil {
		s.mu.Lock()
		if session := s.sessions[rel]; session != nil {
			session.mu.Lock()
			if !session.closed && session.authorship == nil {
				session.authorship = state
			}
			session.mu.Unlock()
		}
		s.mu.Unlock()
	}
	return state.result(s.actors.Owner), nil
}
