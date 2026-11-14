package auth

import "time"

// ActorBinding binds an Automerge actor id to a subject: an account id, a pseudo user
// (":disk", ":server") or AnonymousID
type ActorBinding struct {
	Subject string
	Bound   time.Time
	// AppTokenID and AppTokenName are set for the actors of edits made with an app token of
	// the account (agents). The name is kept, so it stays known after the token is revoked.
	AppTokenID   string
	AppTokenName string
}

// ActorBindings returns all stored actor bindings
func (s *Service) ActorBindings() (map[string]ActorBinding, error) {
	rows, err := s.db.Query("SELECT actor_id, subject, bound_at, app_token_id, app_token_name FROM actor_bindings")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ActorBinding{}
	for rows.Next() {
		var id string
		var b ActorBinding
		var bound int64
		if err = rows.Scan(&id, &b.Subject, &bound, &b.AppTokenID, &b.AppTokenName); err != nil {
			return nil, err
		}
		b.Bound = fromMillis(bound)
		out[id] = b
	}
	return out, rows.Err()
}

// StoreActorBinding stores a new actor binding. Bindings are never changed or removed.
func (s *Service) StoreActorBinding(actorID string, b ActorBinding) error {
	_, err := s.db.Exec("INSERT INTO actor_bindings (actor_id, subject, bound_at, app_token_id, app_token_name) VALUES (?, ?, ?, ?, ?)",
		actorID, b.Subject, toMillis(b.Bound), b.AppTokenID, b.AppTokenName)
	return err
}
