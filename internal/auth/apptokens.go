package auth

import (
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
)

// App token scopes
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// App token errors
var (
	ErrInvalidTokenName   = errors.New("token names have 1 to 64 characters")
	ErrInvalidTokenScope  = errors.New("scope must be read or write")
	ErrInvalidTokenExpiry = errors.New("expiresInDays must be between 1 and 3650")
)

const maxAppTokenDays = 3650

// AppToken is a long-lived token for scripts and integrations
type AppToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	AccountID  string     `json:"-"`
}

// ValidAppToken reports whether a string has the format of an app token
func ValidAppToken(token string) bool {
	return validTokenFormat(token, PrefixAppToken)
}

const appTokenColumns = "id, account_id, name, scope, created_at, last_used_at, expires_at" //nolint:gosec // column names

func scanAppToken(row interface{ Scan(...any) error }) (*AppToken, error) {
	var t AppToken
	var created, used, expires int64
	if err := row.Scan(&t.ID, &t.AccountID, &t.Name, &t.Scope, &created, &used, &expires); err != nil {
		return nil, err
	}
	t.CreatedAt = fromMillis(created)
	if used != 0 {
		u := fromMillis(used)
		t.LastUsedAt = &u
	}
	if expires != 0 {
		e := fromMillis(expires)
		t.ExpiresAt = &e
	}
	return &t, nil
}

// CreateAppToken creates an app token; expiresInDays 0 means no expiry
func (s *Service) CreateAppToken(u *User, name, scope string, expiresInDays int, ri RequestInfo) (*AppToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, "", ErrInvalidTokenName
	}
	if scope != ScopeRead && scope != ScopeWrite {
		return nil, "", ErrInvalidTokenScope
	}
	if expiresInDays < 0 || expiresInDays > maxAppTokenDays {
		return nil, "", ErrInvalidTokenExpiry
	}
	now := s.now().UTC()
	t := &AppToken{ID: newID(), Name: name, Scope: scope, CreatedAt: now, AccountID: u.ID}
	var expires int64
	if expiresInDays > 0 {
		e := now.Add(time.Duration(expiresInDays) * 24 * time.Hour)
		t.ExpiresAt, expires = &e, toMillis(e)
	}
	token := newToken(PrefixAppToken)
	if _, err := s.db.Exec("INSERT INTO app_tokens (id, account_id, name, scope, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		t.ID, u.ID, name, scope, tokenHash(token), toMillis(now), expires); err != nil {
		return nil, "", err
	}
	s.Audit(AuditEvent{Type: AuditAppTokenCreated, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"tokenId": t.ID, "name": name, "scope": scope}})
	return t, token, nil
}

// AppTokens lists the app tokens of an account, newest first
func (s *Service) AppTokens(accountID string) ([]*AppToken, error) {
	rows, err := s.db.Query("SELECT "+appTokenColumns+" FROM app_tokens WHERE account_id = ? ORDER BY created_at DESC, rowid DESC", accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*AppToken{}
	for rows.Next() {
		t, err := scanAppToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAppToken deletes an app token of an account
func (s *Service) RevokeAppToken(u *User, id string, ri RequestInfo) error {
	res, err := s.db.Exec("DELETE FROM app_tokens WHERE id = ? AND account_id = ?", id, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.Audit(AuditEvent{Type: AuditAppTokenRevoked, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent, Details: map[string]any{"tokenId": id}})
	return nil
}

// AuthenticateAppToken returns the account and the token of an app token
func (s *Service) AuthenticateAppToken(token string) (*User, *AppToken, error) {
	if !ValidAppToken(token) {
		return nil, nil, ErrUnauthenticated
	}
	t, err := scanAppToken(s.db.QueryRow("SELECT "+appTokenColumns+" FROM app_tokens WHERE token_hash = ?", tokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, nil, err
	}
	now := s.now().UTC()
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return nil, nil, ErrUnauthenticated
	}
	u := s.UserByID(t.AccountID)
	if u == nil || u.Status != StatusActive {
		return nil, nil, ErrUnauthenticated
	}
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= sessionTouchInterval {
		_, _ = s.db.Exec("UPDATE app_tokens SET last_used_at = ? WHERE id = ?", toMillis(now), t.ID)
		t.LastUsedAt = &now
	}
	return u, t, nil
}

// settingsView is the JSON of the instance settings
type settingsView struct {
	Require2FA bool `json:"require2FA"`
}

// Settings returns the instance settings
func (s *Service) Settings() any {
	return settingsView{Require2FA: s.Require2FA()}
}
