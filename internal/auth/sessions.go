package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Session types
const (
	SessionCookie = "cookie"
	SessionBearer = "bearer"
)

// Token prefixes, see docs/auth.md section 3
const (
	PrefixSession   = "mds_"
	PrefixAppToken  = "mdp_"
	PrefixLink      = "mdl_"
	PrefixChallenge = "mdc_"
	PrefixDevice    = "mdd_"
)

// Session is a login of one device
type Session struct {
	ID            string    `json:"id"`
	AccountID     string    `json:"-"`
	Type          string    `json:"type"`
	DeviceName    string    `json:"deviceName"`
	UserAgent     string    `json:"userAgent"`
	IP            string    `json:"ip"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	IdleExpiresAt time.Time `json:"idleExpiresAt"`
	ReauthAt      time.Time `json:"-"`
	Current       bool      `json:"current"`
}

// RequestInfo describes the client of a request, for audit events and rate limits
type RequestInfo struct {
	IP        string
	UserAgent string
}

// SessionParams describe a new session
type SessionParams struct {
	Type       string
	DeviceName string
	RequestInfo
}

// ErrUnauthenticated is returned for unknown, expired or revoked tokens
var ErrUnauthenticated = errors.New("missing, invalid or expired credentials")

// newToken returns a random token with 256 bits and the given prefix
func newToken(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// tokenHash is the stored form of a high-entropy token
func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// validTokenFormat checks the prefix and length of a token before it is looked up
func validTokenFormat(token, prefix string) bool {
	if len(token) != len(prefix)+43 || !strings.HasPrefix(token, prefix) {
		return false
	}
	for _, c := range token[len(prefix):] {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// ValidSessionToken reports whether a string has the format of a session token
func ValidSessionToken(token string) bool {
	return validTokenFormat(token, PrefixSession)
}

func deviceNameFromUserAgent(ua string) string {
	browser := ""
	switch {
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.Contains(ua, "okhttp"):
		browser = "Android app"
	}
	system := ""
	switch {
	case strings.Contains(ua, "Android"):
		system = "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		system = "iOS"
	case strings.Contains(ua, "Windows"):
		system = "Windows"
	case strings.Contains(ua, "Mac OS X"):
		system = "macOS"
	case strings.Contains(ua, "Linux"):
		system = "Linux"
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return system
	}
	return "Unknown device"
}

func cleanDeviceName(name, ua string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name))
	if name == "" {
		return deviceNameFromUserAgent(ua)
	}
	return truncate(name, 64)
}

// createSession inserts a session and returns it with its token
func (s *Service) createSession(db execer, accountID string, p SessionParams, now time.Time) (*Session, string, error) {
	if p.Type != SessionBearer {
		p.Type = SessionCookie
	}
	token := newToken(PrefixSession)
	sess := &Session{
		ID:         newID(),
		AccountID:  accountID,
		Type:       p.Type,
		DeviceName: cleanDeviceName(p.DeviceName, p.UserAgent),
		UserAgent:  truncate(p.UserAgent, 256),
		IP:         p.IP,
		CreatedAt:  now,
		LastUsedAt: now,
		ExpiresAt:  now.Add(s.cfg.maxLifetime),
		ReauthAt:   now,
	}
	sess.IdleExpiresAt = s.idleExpiry(sess)
	_, err := db.Exec(`INSERT INTO sessions (id, account_id, token_hash, type, device_name, user_agent, ip, created_at, last_used_at, expires_at, reauth_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, sess.ID, accountID, tokenHash(token), sess.Type, sess.DeviceName, sess.UserAgent, sess.IP,
		toMillis(now), toMillis(now), toMillis(sess.ExpiresAt), toMillis(now))
	if err != nil {
		return nil, "", err
	}
	if _, err = db.Exec("UPDATE accounts SET last_login_at = ? WHERE id = ?", toMillis(now), accountID); err != nil {
		return nil, "", err
	}
	return sess, token, nil
}

func (s *Service) idleExpiry(sess *Session) time.Time {
	idle := sess.LastUsedAt.Add(s.cfg.idleTimeout)
	if idle.After(sess.ExpiresAt) {
		return sess.ExpiresAt
	}
	return idle
}

const sessionColumns = "id, account_id, type, device_name, user_agent, ip, created_at, last_used_at, expires_at, reauth_at"

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	var created, used, expires, reauth int64
	if err := row.Scan(&sess.ID, &sess.AccountID, &sess.Type, &sess.DeviceName, &sess.UserAgent, &sess.IP, &created, &used, &expires, &reauth); err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.LastUsedAt, sess.ExpiresAt, sess.ReauthAt = fromMillis(created), fromMillis(used), fromMillis(expires), fromMillis(reauth)
	return &sess, nil
}

// AuthenticateSession returns the account and the session of a session token. It extends
// the idle expiry. ip is recorded with the last use.
func (s *Service) AuthenticateSession(token string, ip string) (*User, *Session, error) {
	if !ValidSessionToken(token) {
		return nil, nil, ErrUnauthenticated
	}
	sess, err := scanSession(s.db.QueryRow("SELECT "+sessionColumns+" FROM sessions WHERE token_hash = ?", tokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, nil, err
	}
	now := s.now().UTC()
	sess.IdleExpiresAt = s.idleExpiry(sess)
	if !now.Before(sess.IdleExpiresAt) {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE id = ?", sess.ID)
		return nil, nil, ErrUnauthenticated
	}
	u := s.UserByID(sess.AccountID)
	if u == nil || u.Status != StatusActive {
		return nil, nil, ErrUnauthenticated
	}
	if now.Sub(sess.LastUsedAt) >= sessionTouchInterval || (ip != "" && ip != sess.IP && now.Sub(sess.LastUsedAt) >= time.Minute) {
		sess.LastUsedAt = now
		if ip != "" {
			sess.IP = ip
		}
		sess.IdleExpiresAt = s.idleExpiry(sess)
		_, _ = s.db.Exec("UPDATE sessions SET last_used_at = ?, ip = ? WHERE id = ?", toMillis(now), sess.IP, sess.ID)
	}
	return u, sess, nil
}

// Sessions lists the active sessions of an account, newest use first
func (s *Service) Sessions(accountID string) ([]*Session, error) {
	now := s.now()
	rows, err := s.db.Query("SELECT "+sessionColumns+" FROM sessions WHERE account_id = ? ORDER BY last_used_at DESC", accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sess.IdleExpiresAt = s.idleExpiry(sess)
		if now.Before(sess.IdleExpiresAt) {
			out = append(out, sess)
		}
	}
	return out, rows.Err()
}

// SessionByID returns a session, ErrNotFound if it does not exist
func (s *Service) SessionByID(id string) (*Session, error) {
	sess, err := scanSession(s.db.QueryRow("SELECT "+sessionColumns+" FROM sessions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.IdleExpiresAt = s.idleExpiry(sess)
	return sess, nil
}

// RevokeSession ends a session. With accountID set, the session must belong to that account.
func (s *Service) RevokeSession(accountID, sessionID string, by *User, ri RequestInfo) error {
	sess, err := s.SessionByID(sessionID)
	if err != nil {
		return err
	}
	if accountID != "" && sess.AccountID != accountID {
		return ErrNotFound
	}
	if _, err = s.db.Exec("DELETE FROM sessions WHERE id = ?", sessionID); err != nil {
		return err
	}
	actor := ""
	if by != nil {
		actor = by.ID
	}
	s.Audit(AuditEvent{Type: AuditSessionRevoked, ActorID: actor, TargetID: sess.AccountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"sessionId": sessionID, "deviceName": sess.DeviceName}})
	s.notifyRevoked(RevokeEvent{AccountID: sess.AccountID, SessionID: sessionID, Reason: RevokeSession})
	return nil
}

// Logout ends the current session
func (s *Service) Logout(sess *Session, ri RequestInfo) {
	if _, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", sess.ID); err != nil {
		return
	}
	s.Audit(AuditEvent{Type: AuditLogout, ActorID: sess.AccountID, TargetID: sess.AccountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"sessionId": sess.ID}})
	s.notifyRevoked(RevokeEvent{AccountID: sess.AccountID, SessionID: sess.ID, Reason: RevokeSession})
}

// RevokeSessions ends all sessions of an account except one (may be empty) and returns
// how many were ended
func (s *Service) RevokeSessions(accountID, exceptSessionID string, by *User, ri RequestInfo) (int, error) {
	n, err := revokeSessions(s.db, accountID, exceptSessionID)
	if err != nil || n == 0 {
		return n, err
	}
	actor := ""
	if by != nil {
		actor = by.ID
	}
	s.Audit(AuditEvent{Type: AuditSessionRevoked, ActorID: actor, TargetID: accountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"count": n, "exceptSessionId": exceptSessionID}})
	s.notifyRevoked(RevokeEvent{AccountID: accountID, ExceptSessionID: exceptSessionID, Reason: RevokeSession})
	return n, nil
}

func revokeSessions(db execer, accountID, exceptSessionID string) (int, error) {
	res, err := db.Exec("DELETE FROM sessions WHERE account_id = ? AND id != ?", accountID, exceptSessionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReauthenticatedUntil returns the end of the step-up window of a session
func ReauthenticatedUntil(sess *Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	return sess.ReauthAt.Add(ReauthWindow)
}

// RecentlyAuthenticated reports whether the session is within the step-up window
func (s *Service) RecentlyAuthenticated(sess *Session) bool {
	return sess != nil && s.now().Before(ReauthenticatedUntil(sess))
}

// RecentlyCreated reports whether the session was created within the step-up window. Such a
// session may register a passkey without step-up (docs/auth.md, step-up).
func (s *Service) RecentlyCreated(sess *Session) bool {
	return sess != nil && s.now().Before(sess.CreatedAt.Add(ReauthWindow))
}

// markReauthenticated starts a new step-up window for a session
func (s *Service) markReauthenticated(sess *Session) error {
	now := s.now().UTC()
	if _, err := s.db.Exec("UPDATE sessions SET reauth_at = ? WHERE id = ?", toMillis(now), sess.ID); err != nil {
		return err
	}
	sess.ReauthAt = now
	return nil
}

// pruneExpired deletes expired sessions, links and audit events
func (s *Service) pruneExpired() {
	now := s.now()
	_, _ = s.db.Exec("DELETE FROM sessions WHERE expires_at <= ? OR last_used_at <= ?", toMillis(now), toMillis(now.Add(-s.cfg.idleTimeout)))
	_, _ = s.db.Exec("DELETE FROM audit_events WHERE time < ?", toMillis(now.Add(-s.cfg.auditRetention)))
	_, _ = s.db.Exec("DELETE FROM app_tokens WHERE expires_at != 0 AND expires_at <= ?", toMillis(now))
	_, _ = s.db.Exec("DELETE FROM device_links WHERE expires_at <= ?", toMillis(now.Add(-deviceLinkKeep)))
}

// Revoke reasons
const (
	RevokeSession  = "session"
	RevokeDisabled = "disabled"
	RevokeRole     = "role"
)

// RevokeEvent tells listeners (the websocket server) that sessions ended or an account changed
type RevokeEvent struct {
	AccountID string
	// SessionID is set when one session ended
	SessionID string
	// ExceptSessionID is set when all sessions but one ended
	ExceptSessionID string
	Reason          string
}

// OnRevoke registers a listener for ended sessions and account changes. It is called
// synchronously and must not block.
func (s *Service) OnRevoke(f func(RevokeEvent)) {
	s.listenersMu.Lock()
	s.listeners = append(s.listeners, f)
	s.listenersMu.Unlock()
}

func (s *Service) notifyRevoked(e RevokeEvent) {
	s.listenersMu.Lock()
	listeners := append([]func(RevokeEvent){}, s.listeners...)
	s.listenersMu.Unlock()
	for _, f := range listeners {
		f(e)
	}
}
