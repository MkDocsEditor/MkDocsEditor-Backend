package backend

import (
	automerge "github.com/automerge/automerge-go"
)

// computeAuthorship replays the whole history of a document
func computeAuthorship(doc *automerge.Doc, owner func(actorID string) (string, string)) (*Authorship, error) {
	state, err := fullAuthorshipState(doc)
	if err != nil {
		return nil, err
	}
	return state.result(owner), nil
}
