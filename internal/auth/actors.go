package auth

import "time"

// ActorBinding binds an Automerge actor id to a subject: an account id, a pseudo user
// (":disk", ":server") or AnonymousID
type ActorBinding struct {
	Subject string
	Bound   time.Time
}

// ActorBindings returns all stored actor bindings
func (s *Service) ActorBindings() (map[string]ActorBinding, error) {
	rows, err := s.db.Query("SELECT actor_id, subject, bound_at FROM actor_bindings")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ActorBinding{}
	for rows.Next() {
		var id, subject string
		var bound int64
		if err = rows.Scan(&id, &subject, &bound); err != nil {
			return nil, err
		}
		out[id] = ActorBinding{Subject: subject, Bound: fromMillis(bound)}
	}
	return out, rows.Err()
}

// StoreActorBinding stores a new actor binding. Bindings are never changed or removed.
func (s *Service) StoreActorBinding(actorID string, b ActorBinding) error {
	_, err := s.db.Exec("INSERT INTO actor_bindings (actor_id, subject, bound_at) VALUES (?, ?, ?)", actorID, b.Subject, toMillis(b.Bound))
	return err
}
