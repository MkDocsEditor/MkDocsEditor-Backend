package activity

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// MaxSubscriptionsPerAccount limits the push subscriptions of one account. A new one beyond it
// replaces the oldest.
const MaxSubscriptionsPerAccount = 10

// Subscription is a Web Push subscription of a client
type Subscription struct {
	Endpoint  string
	AccountID string
	// SessionID is the session that subscribed: the subscription ends with it
	SessionID string
	P256DH    []byte
	Auth      []byte
	CreatedAt time.Time
	// LastPushAt is the zero time before the first successful push
	LastPushAt time.Time
}

// AddSubscription stores a subscription. An existing one with the same endpoint is replaced,
// also when it belonged to another account or session.
func (s *Store) AddSubscription(sub Subscription) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`INSERT INTO push_subscriptions (endpoint, account_id, session_id, p256dh, auth, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET account_id = excluded.account_id, session_id = excluded.session_id,
			p256dh = excluded.p256dh, auth = excluded.auth, created_at = excluded.created_at, last_push_at = 0`,
		sub.Endpoint, sub.AccountID, sub.SessionID, sub.P256DH, sub.Auth, s.now().UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM push_subscriptions WHERE account_id = ? AND endpoint NOT IN (
		SELECT endpoint FROM push_subscriptions WHERE account_id = ? ORDER BY created_at DESC, endpoint LIMIT ?)`,
		sub.AccountID, sub.AccountID, MaxSubscriptionsPerAccount); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveSubscription removes a subscription of an account. It returns false if there was none.
func (s *Store) RemoveSubscription(accountID string, endpoint string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE account_id = ? AND endpoint = ?`, accountID, endpoint)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteSubscription removes a subscription whatever account it has
func (s *Store) DeleteSubscription(endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// SubscriptionPushed remembers a successful push
func (s *Store) SubscriptionPushed(endpoint string) error {
	_, err := s.db.Exec(`UPDATE push_subscriptions SET last_push_at = ? WHERE endpoint = ?`, s.now().UnixMilli(), endpoint)
	return err
}

// Subscriptions returns the subscriptions of an account, or of all accounts with "", newest first
func (s *Store) Subscriptions(accountID string) ([]Subscription, error) {
	query := `SELECT endpoint, account_id, session_id, p256dh, auth, created_at, last_push_at FROM push_subscriptions`
	var args []any
	if accountID != "" {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	rows, err := s.db.Query(query+` ORDER BY created_at DESC, endpoint`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Subscription{}
	for rows.Next() {
		var (
			sub             Subscription
			created, pushed int64
		)
		if err = rows.Scan(&sub.Endpoint, &sub.AccountID, &sub.SessionID, &sub.P256DH, &sub.Auth, &created, &pushed); err != nil {
			return nil, err
		}
		sub.CreatedAt = time.UnixMilli(created).UTC()
		if pushed != 0 {
			sub.LastPushAt = time.UnixMilli(pushed).UTC()
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Entry returns one feed entry, nil if it does not exist
func (s *Store) Entry(id int64) (*Entry, error) {
	e, err := scanEntry(s.db.QueryRow(`SELECT `+entryColumns+` FROM feed WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Watchers tells who follows documents: family is true if one of them has a family pin, and
// accounts are those with a personal pin or an edit of one of them in the feed. The server's
// pseudo users are left out.
func (s *Store) Watchers(paths ...string) (family bool, accounts []string, err error) {
	seen := map[string]bool{}
	add := func(id string) {
		if id != Family && !strings.HasPrefix(id, ":") && !seen[id] {
			seen[id] = true
			accounts = append(accounts, id)
		}
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		rows, err := s.db.Query(`SELECT account_id FROM pins WHERE path = ?`, p)
		if err != nil {
			return false, nil, err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return false, nil, err
			}
			if id == Family {
				family = true
			}
			add(id)
		}
		_ = rows.Close()
		if err = rows.Err(); err != nil {
			return false, nil, err
		}
		rows, err = s.db.Query(`SELECT authors FROM feed WHERE path = ? AND kind = ? AND time >= ?`, p, KindEdited, s.now().Add(-Retention).UnixMilli())
		if err != nil {
			return false, nil, err
		}
		for rows.Next() {
			var (
				raw     string
				authors []string
			)
			if err = rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return false, nil, err
			}
			if err = json.Unmarshal([]byte(raw), &authors); err != nil {
				_ = rows.Close()
				return false, nil, err
			}
			for _, a := range authors {
				add(a)
			}
		}
		_ = rows.Close()
		if err = rows.Err(); err != nil {
			return false, nil, err
		}
	}
	return family, accounts, nil
}
