package collab

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	automerge "github.com/automerge/automerge-go"
)

func testOwner(string) (string, string) { return "", "" }

func actorDoc(t *testing.T, doc *automerge.Doc, actor string) *automerge.Doc {
	t.Helper()
	fork, err := doc.Fork()
	if err != nil {
		t.Fatal(err)
	}
	if err = fork.SetActorID(actor); err != nil {
		t.Fatal(err)
	}
	return fork
}

// concurrentHistory has two offline actors edit concurrently, while a server gets their changes
// late and one actor at a time. It returns the document of the server and its authorship state,
// which went through a snapshot after everything that arrived.
func concurrentHistory(t *testing.T, rng *rand.Rand, words func(actor string, i int) string) (*automerge.Doc, *authorshipState) {
	t.Helper()
	server := automerge.New()
	if err := server.SetActorID("00"); err != nil {
		t.Fatal(err)
	}
	if err := server.Path(ContentKey).Set(automerge.NewText("# Title\n\nSome text of this page.\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Commit("import"); err != nil {
		t.Fatal(err)
	}
	state, err := fullAuthorshipState(server)
	if err != nil {
		t.Fatal(err)
	}
	a, b := actorDoc(t, server, "aa"), actorDoc(t, server, "bb")
	arrive := func(from *automerge.Doc) {
		t.Helper()
		if _, err := server.Merge(from); err != nil {
			t.Fatal(err)
		}
		restored, err := restoreAuthorshipState(server, snapshotOf(state, "epoch"), "epoch")
		if err != nil {
			t.Fatal(err)
		}
		changes, err := server.Changes(restored.heads...)
		if err != nil {
			t.Fatal(err)
		}
		if err = restored.advance(changes); err != nil {
			t.Fatal(err)
		}
		state = restored
	}
	for i := range 60 {
		doc, actor := a, "aa"
		if rng.IntN(2) == 0 {
			doc, actor = b, "bb"
		}
		text := doc.Path(ContentKey).Text()
		length, err := text.Len(), error(nil)
		if rng.IntN(5) == 0 && length > 4 {
			err = text.Delete(rng.IntN(length-2), 2)
		} else {
			err = text.Insert(rng.IntN(length+1), words(actor, i))
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = doc.Commit("edit"); err != nil {
			t.Fatal(err)
		}
		if rng.IntN(4) == 0 {
			arrive(doc)
		}
		if rng.IntN(8) == 0 {
			if _, err = a.Merge(b); err != nil {
				t.Fatal(err)
			}
			if _, err = b.Merge(a); err != nil {
				t.Fatal(err)
			}
		}
	}
	arrive(a)
	arrive(b)
	return server, state
}

// A snapshot taken after every arrival gives exactly what a replay of the saved document gives,
// also where a text diff has more than one reading. Automerge keeps the changes in the order they
// arrived, in memory and in a save. If an update of automerge-go breaks this test, snapshots
// depend on when they were taken.
func TestAuthorshipSnapshotMatchesFullReplayWithConcurrentEdits(t *testing.T) {
	for name, words := range map[string]func(rng *rand.Rand, actor string, i int) string{
		"unique texts":        func(_ *rand.Rand, actor string, i int) string { return fmt.Sprintf("<%s%03d>", actor, i) },
		"repeated characters": func(rng *rand.Rand, _ string, _ int) string { return strings.Repeat("a", 1+rng.IntN(3)) },
	} {
		t.Run(name, func(t *testing.T) {
			for seed := range uint64(30) {
				rng := rand.New(rand.NewPCG(seed, 1))
				doc, state := concurrentHistory(t, rng, func(actor string, i int) string { return words(rng, actor, i) })
				loaded, err := automerge.Load(doc.Save())
				if err != nil {
					t.Fatal(err)
				}
				full, err := fullAuthorshipState(loaded)
				if err != nil {
					t.Fatal(err)
				}
				if !sameHeads(state.heads, doc.Heads()) {
					t.Fatalf("seed %d: the state is not at the heads of the document", seed)
				}
				if got, want := fmt.Sprint(state.result(testOwner).Runs), fmt.Sprint(full.result(testOwner).Runs); got != want {
					t.Fatalf("seed %d:\n got %s\nwant %s", seed, got, want)
				}
			}
		})
	}
}

func snapshotTestServer(t *testing.T, doc *automerge.Doc) (*SyncServer, *DocumentMeta) {
	t.Helper()
	store, err := NewStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta := &DocumentMeta{UUID: "doc", Path: "doc.md", Epoch: "epoch"}
	if err = store.Save(meta, doc.Save()); err != nil {
		t.Fatal(err)
	}
	return &SyncServer{store: store, stop: make(chan struct{}), authorshipSlots: make(chan struct{}, 1)}, meta
}

// A replay stopped by a shutdown keeps what it got, and the next one continues from there
func TestAuthorshipReplayContinuesAfterShutdown(t *testing.T) {
	doc := automerge.New()
	if err := doc.Path(ContentKey).Set(automerge.NewText("start\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Commit("import"); err != nil {
		t.Fatal(err)
	}
	for i := range authorshipBatchChanges + 50 {
		if err := doc.Path(ContentKey).Text().Insert(0, fmt.Sprintf("<%d>", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := doc.Commit("edit"); err != nil {
			t.Fatal(err)
		}
	}
	s, meta := snapshotTestServer(t, doc)

	// stopped before the first batch: nothing to keep
	close(s.stop)
	if _, err := s.replayFromSnapshot(meta.UUID, meta.Epoch, doc.Save()); err == nil {
		t.Fatal("no error from a stopped replay")
	}
	if snapshot := s.readAuthorshipSnapshot(meta.UUID); snapshot != nil {
		t.Fatalf("snapshot without any change: %+v", snapshot)
	}

	// a snapshot in the middle of the history, like a stopped replay leaves it
	s.stop = make(chan struct{})
	changes, err := doc.Changes()
	if err != nil {
		t.Fatal(err)
	}
	partial := newAuthorshipState()
	if err = partial.advance(changes[:authorshipBatchChanges]); err != nil {
		t.Fatal(err)
	}
	s.writeAuthorshipSnapshot(meta.UUID, meta.Epoch, partial)

	resumed, err := s.replayFromSnapshot(meta.UUID, meta.Epoch, doc.Save())
	if err != nil {
		t.Fatal(err)
	}
	if !sameHeads(resumed.heads, doc.Heads()) {
		t.Error("the replay did not reach the heads of the document")
	}
	full, err := fullAuthorshipState(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(resumed.result(testOwner).Runs), fmt.Sprint(full.result(testOwner).Runs); got != want {
		t.Errorf("resumed %s, full %s", got, want)
	}
	if snapshot := s.readAuthorshipSnapshot(meta.UUID); snapshot == nil || !slices.Equal(snapshot.Heads, formatHeads(resumed.heads)) {
		t.Errorf("snapshot not at the new heads: %+v", snapshot)
	}
}

// A job that finishes after the document was deleted leaves no file behind
func TestAuthorshipSnapshotOfDeletedDocumentIsNotWritten(t *testing.T) {
	doc := automerge.New()
	if err := doc.Path(ContentKey).Set(automerge.NewText("text")); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Commit("import"); err != nil {
		t.Fatal(err)
	}
	s, meta := snapshotTestServer(t, doc)
	state, err := fullAuthorshipState(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.Archive(meta.Path); err != nil {
		t.Fatal(err)
	}
	s.writeAuthorshipSnapshot(meta.UUID, meta.Epoch, state)
	if snapshot := s.readAuthorshipSnapshot(meta.UUID); snapshot != nil {
		t.Errorf("snapshot written for a deleted document: %+v", snapshot)
	}
}
