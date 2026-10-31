package backend

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	mutexSync "sync"
	"time"
)

const (
	// pseudo users of server-made changes. Real usernames cannot contain ':'.
	ActorUserDisk   = ":disk"
	ActorUserServer = ":server"

	actorsFileName = "actors.json"
)

// ErrForeignActor is returned when an actor id is bound to another user
var ErrForeignActor = errors.New("actor id belongs to another user")

// ErrInvalidActor is returned for actor ids that are not hex strings
var ErrInvalidActor = errors.New("invalid actor id")

// ActorBinding binds an Automerge actor id to a user
type ActorBinding struct {
	Username string    `json:"username"`
	Bound    time.Time `json:"bound"`
}

// ActorRegistry stores which user each Automerge actor id belongs to. Bindings are never removed.
type ActorRegistry struct {
	file string

	lock   mutexSync.Mutex
	actors map[string]ActorBinding
}

// NewActorRegistry loads the actor bindings from the state directory
func NewActorRegistry(stateDir string) (*ActorRegistry, error) {
	r := &ActorRegistry{
		file:   filepath.Join(stateDir, actorsFileName),
		actors: map[string]ActorBinding{},
	}
	data, err := os.ReadFile(r.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return nil, err
	}
	if err = json.Unmarshal(data, &r.actors); err != nil {
		return nil, fmt.Errorf("%s: %w", r.file, err)
	}
	return r, nil
}

// ValidateActorID checks the format of an actor id (hex, 1 to 64 bytes)
func ValidateActorID(actorID string) error {
	b, err := hex.DecodeString(actorID)
	if err != nil || len(b) == 0 || len(b) > 64 || hex.EncodeToString(b) != actorID {
		return fmt.Errorf("%w: %q", ErrInvalidActor, actorID)
	}
	return nil
}

// Owner returns the username an actor id is bound to, or "" if it is unbound
func (r *ActorRegistry) Owner(actorID string) string {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.actors[actorID].Username
}

// Bind binds an unbound actor id to the user. It returns true if the binding is new and
// ErrForeignActor if the actor id belongs to another user.
func (r *ActorRegistry) Bind(actorID string, username string) (bool, error) {
	if err := ValidateActorID(actorID); err != nil {
		return false, err
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	if b, ok := r.actors[actorID]; ok {
		if b.Username != username {
			return false, ErrForeignActor
		}
		return false, nil
	}
	r.actors[actorID] = ActorBinding{Username: username, Bound: time.Now().UTC()}
	if err := r.saveLocked(); err != nil {
		delete(r.actors, actorID)
		return false, err
	}
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

// All returns all bindings sorted by actor id
func (r *ActorRegistry) All() map[string]ActorBinding {
	r.lock.Lock()
	defer r.lock.Unlock()
	c := make(map[string]ActorBinding, len(r.actors))
	for k, v := range r.actors {
		c[k] = v
	}
	return c
}

// SortedActorIDs returns the keys of a binding map in a stable order
func SortedActorIDs(m map[string]ActorBinding) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (r *ActorRegistry) saveLocked() error {
	data, err := json.MarshalIndent(r.actors, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(r.file, data)
}
