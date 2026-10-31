package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

const (
	// TicketLifetime is how long a websocket ticket stays valid
	TicketLifetime = 60 * time.Second
	// how long a successful legacy basic auth check is remembered, so not every request pays for hashing
	verificationCacheLifetime = 10 * time.Minute
)

type ticket struct {
	user      *User
	sessionID string
	expires   time.Time
}

type cachedVerification struct {
	digest  []byte
	hash    string
	expires time.Time
}

// IssueTicket creates a single-use websocket ticket for the given user. sessionID is the
// session the ticket is issued for (empty for other authentication methods); the ticket is
// refused when that session has ended.
func (s *Service) IssueTicket(user *User, sessionID string) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	value := base64.RawURLEncoding.EncodeToString(b)
	now := s.now()
	expires := now.Add(TicketLifetime)

	s.ticketsMu.Lock()
	defer s.ticketsMu.Unlock()
	for k, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, k)
		}
	}
	s.tickets[value] = ticket{user: user, sessionID: sessionID, expires: expires}
	return value, expires, nil
}

// ErrInvalidTicket is returned for unknown, used or expired tickets
var ErrInvalidTicket = errors.New("invalid or expired ticket")

// RedeemTicket returns the user and the session id of a ticket and invalidates it. The
// user is read again, so a disabled account or a changed role is taken into account.
func (s *Service) RedeemTicket(value string) (*User, string, error) {
	s.ticketsMu.Lock()
	t, ok := s.tickets[value]
	delete(s.tickets, value)
	s.ticketsMu.Unlock()
	if !ok || !s.now().Before(t.expires) {
		return nil, "", ErrInvalidTicket
	}
	if t.user.ID == AnonymousID {
		return t.user, "", nil
	}
	u := s.UserByID(t.user.ID)
	if u == nil || u.Status != StatusActive {
		return nil, "", ErrInvalidTicket
	}
	if t.sessionID != "" {
		if sess, err := s.SessionByID(t.sessionID); err != nil || sess.AccountID != u.ID || !s.now().Before(sess.IdleExpiresAt) {
			return nil, "", ErrInvalidTicket
		}
	}
	return u, t.sessionID, nil
}

// AuthenticateBasic checks a username and password for HTTP basic auth. It returns the
// user, or nil if the credentials are wrong or the account is not active.
func (s *Service) AuthenticateBasic(username, password string) *User {
	u := s.UserByUsername(username)
	if u == nil || u.Status != StatusActive || usernameKey(u.Username) != usernameKey(username) {
		verifyDummy(password)
		return nil
	}
	var hash string
	if err := s.db.QueryRow("SELECT password_hash FROM accounts WHERE id = ?", u.ID).Scan(&hash); err != nil || hash == "" {
		verifyDummy(password)
		return nil
	}

	// a keyed digest of the verified password: repeated requests of the same client skip
	// the expensive password hash
	mac := hmac.New(sha256.New, s.basicKey)
	mac.Write([]byte(u.ID + "\x00" + password))
	digest := mac.Sum(nil)
	s.basicMu.Lock()
	cached, hit := s.basicCache[u.ID]
	s.basicMu.Unlock()
	if hit && cached.hash == hash && s.now().Before(cached.expires) && hmac.Equal(cached.digest, digest) {
		return u
	}

	ok, rehash := VerifyPassword(hash, password)
	if !ok {
		return nil
	}
	// accounts with a second factor cannot use their password for basic auth
	if len(s.secondFactorMethods(u.ID)) > 0 {
		return nil
	}
	if rehash {
		if newHash, err := s.rehashPassword(u.ID, hash, password); err == nil {
			hash = newHash
		}
	}
	s.basicMu.Lock()
	s.basicCache[u.ID] = cachedVerification{digest: digest, hash: hash, expires: s.now().Add(verificationCacheLifetime)}
	s.basicMu.Unlock()
	return u
}

// rehashPassword replaces an old hash (bcrypt, older Argon2id parameters) after a
// successful verification. It returns the new hash.
func (s *Service) rehashPassword(accountID, oldHash, password string) (string, error) {
	newHash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	// only replace the hash that was verified, never a password changed in the meantime
	_, err = s.db.Exec("UPDATE accounts SET password_hash = ? WHERE id = ? AND password_hash = ?", newHash, accountID, oldHash)
	if err != nil {
		return "", err
	}
	return newHash, nil
}
