package collab

import (
	"strings"
	"testing"

	automerge "github.com/automerge/automerge-go"
)

func benchDoc() *automerge.Doc {
	doc := automerge.New()
	_ = doc.Path(ContentKey).Set(automerge.NewText(strings.Repeat("lorem ipsum dolor sit amet\n", 700)))
	_, _ = doc.Commit("import")
	for i := range 3000 {
		_ = doc.Path(ContentKey).Text().Insert((i*37)%15000, "x")
		_, _ = doc.Commit("edit")
	}
	return doc
}

// BenchmarkAuthorship measures the full replay of a page of about 20 KB built with 3000 small edits
func BenchmarkAuthorship(b *testing.B) {
	doc := benchDoc()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := computeAuthorship(doc, func(string) (string, string) { return "", "" }); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAuthorshipIncremental measures advancing the replay state by 10 edits
func BenchmarkAuthorshipIncremental(b *testing.B) {
	doc := benchDoc()
	state, err := fullAuthorshipState(doc)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range 10 {
			_ = doc.Path(ContentKey).Text().Insert(j*100, "y")
			_, _ = doc.Commit("edit")
		}
		changes, err := doc.Changes(state.heads...)
		if err != nil {
			b.Fatal(err)
		}
		if err = state.advance(changes); err != nil {
			b.Fatal(err)
		}
		state.result(func(string) (string, string) { return "", "" })
	}
}

// computeAuthorship replays the whole history of a document
func computeAuthorship(doc *automerge.Doc, owner func(actorID string) (string, string)) (*Authorship, error) {
	state, err := fullAuthorshipState(doc)
	if err != nil {
		return nil, err
	}
	return state.result(owner), nil
}
