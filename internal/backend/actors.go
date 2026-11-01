package backend

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

const (
	// pseudo users of server-made changes. Real usernames cannot contain ':'.
	ActorUserDisk   = ":disk"
	ActorUserServer = ":server"
)

// ErrForeignActor is returned when an actor id is bound to another user
var ErrForeignActor = errors.New("actor id belongs to another user")

// ErrInvalidActor is returned for actor ids that are not hex strings
var ErrInvalidActor = errors.New("invalid actor id")

// ActorStore persists actor bindings (the account database)
type ActorStore interface {
	ActorBindings() (map[string]auth.ActorBinding, error)
	StoreActorBinding(actorID string, b auth.ActorBinding) error
}

// ActorRegistry stores which subject (account id or pseudo user) each Automerge actor id
// belongs to. Bindings are never removed.
type ActorRegistry struct {
	store ActorStore

	lock   sync.Mutex
	actors map[string]auth.ActorBinding
}

// NewActorRegistry loads the actor bindings from the store
func NewActorRegistry(store ActorStore) (*ActorRegistry, error) {
	actors, err := store.ActorBindings()
	if err != nil {
		return nil, fmt.Errorf("unable to load the actor bindings: %w", err)
	}
	return &ActorRegistry{store: store, actors: actors}, nil
}

// ValidateActorID checks the format of an actor id (hex, 1 to 64 bytes)
func ValidateActorID(actorID string) error {
	b, err := hex.DecodeString(actorID)
	if err != nil || len(b) == 0 || len(b) > 64 || hex.EncodeToString(b) != actorID {
		return fmt.Errorf("%w: %q", ErrInvalidActor, actorID)
	}
	return nil
}

// Owner returns the subject an actor id is bound to, or "" if it is unbound
func (r *ActorRegistry) Owner(actorID string) string {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.actors[actorID].Subject
}

// Bind binds an unbound actor id to the subject. It returns true if the binding is new and
// ErrForeignActor if the actor id belongs to another subject.
func (r *ActorRegistry) Bind(actorID string, subject string) (bool, error) {
	if err := ValidateActorID(actorID); err != nil {
		return false, err
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	if b, ok := r.actors[actorID]; ok {
		if b.Subject != subject {
			return false, ErrForeignActor
		}
		return false, nil
	}
	b := auth.ActorBinding{Subject: subject, Bound: time.Now().UTC()}
	if err := r.store.StoreActorBinding(actorID, b); err != nil {
		return false, err
	}
	r.actors[actorID] = b
	return true, nil
}

// NewServerActor creates and binds a fresh actor id for changes made by the server
func (r *ActorRegistry) NewServerActor(pseudoUser string) (string, error) {
	id := newRandomID()
	if _, err := r.Bind(id, pseudoUser); err != nil {
		return "", err
	}
	return id, nil
}

// All returns all bindings
func (r *ActorRegistry) All() map[string]auth.ActorBinding {
	r.lock.Lock()
	defer r.lock.Unlock()
	c := make(map[string]auth.ActorBinding, len(r.actors))
	maps.Copy(c, r.actors)
	return c
}

// SortedActorIDs returns the keys of a binding map in a stable order
func SortedActorIDs(m map[string]auth.ActorBinding) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
