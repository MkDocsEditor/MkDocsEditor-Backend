package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Link kinds
const (
	LinkSetup  = "setup"
	LinkInvite = "invite"
	LinkReset  = "reset"
)

// Link states
const (
	LinkPending = "pending"
	LinkUsed    = "used"
	LinkExpired = "expired"
	LinkRevoked = "revoked"
)

// ErrInvalidLink is returned for unknown, used, expired or revoked link tokens
var ErrInvalidLink = errors.New("the link is invalid, used, expired or revoked")

// LinkUserRef names an account in a link
type LinkUserRef struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Link is a setup, invite or reset link
type Link struct {
	ID                 string       `json:"id"`
	Kind               string       `json:"kind"`
	Role               string       `json:"role,omitempty"`
	Note               string       `json:"note"`
	AccountID          *string      `json:"accountId"`
	ResetSecondFactors bool         `json:"resetSecondFactors"`
	CreatedBy          *LinkUserRef `json:"createdBy"`
	CreatedAt          time.Time    `json:"createdAt"`
	ExpiresAt          time.Time    `json:"expiresAt"`
	UsedAt             *time.Time   `json:"usedAt"`
	UsedBy             *LinkUserRef `json:"usedBy"`
	RevokedAt          *time.Time   `json:"revokedAt"`
	Status             string       `json:"status"`
}

const linkColumns = "id, kind, role, account_id, note, reset_second_factors, created_by, created_at, expires_at, used_at, used_by, revoked_at"

func (s *Service) scanLink(row interface{ Scan(...any) error }) (*Link, error) {
	var l Link
	var accountID, createdBy, usedBy string
	var reset int
	var created, expires, used, revoked int64
	if err := row.Scan(&l.ID, &l.Kind, &l.Role, &accountID, &l.Note, &reset, &createdBy, &created, &expires, &used, &usedBy, &revoked); err != nil {
		return nil, err
	}
	l.ResetSecondFactors = reset != 0
	l.CreatedAt, l.ExpiresAt = fromMillis(created), fromMillis(expires)
	if accountID != "" {
		l.AccountID = &accountID
	}
	l.CreatedBy = s.userRef(createdBy)
	l.UsedBy = s.userRef(usedBy)
	if used != 0 {
		t := fromMillis(used)
		l.UsedAt = &t
	}
	if revoked != 0 {
		t := fromMillis(revoked)
		l.RevokedAt = &t
	}
	switch {
	case l.UsedAt != nil:
		l.Status = LinkUsed
	case l.RevokedAt != nil:
		l.Status = LinkRevoked
	case !s.now().Before(l.ExpiresAt):
		l.Status = LinkExpired
	default:
		l.Status = LinkPending
	}
	return &l, nil
}

func (s *Service) userRef(id string) *LinkUserRef {
	if id == "" {
		return nil
	}
	if u := s.UserByID(id); u != nil {
		return &LinkUserRef{ID: u.ID, Username: u.Username}
	}
	return &LinkUserRef{ID: id}
}

func (s *Service) insertLink(db execer, l *Link, createdBy string) (string, error) {
	token := newToken(PrefixLink)
	accountID := ""
	if l.AccountID != nil {
		accountID = *l.AccountID
	}
	reset := 0
	if l.ResetSecondFactors {
		reset = 1
	}
	_, err := db.Exec("INSERT INTO links (id, kind, token_hash, role, account_id, note, reset_second_factors, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.ID, l.Kind, tokenHash(token), l.Role, accountID, l.Note, reset, createdBy, toMillis(l.CreatedAt), toMillis(l.ExpiresAt))
	return token, err
}

// SetupRequired reports whether the server waits for the first account
func (s *Service) SetupRequired() bool {
	if s.cfg.disabled {
		return false
	}
	s.loadCache()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.active == 0
}

// ErrSetupDone is returned when a setup link is requested while accounts exist
var ErrSetupDone = errors.New("the setup is done: an account exists")

// CreateSetupLink revokes older setup links and creates a new one. It fails when an account exists.
func (s *Service) CreateSetupLink() (*Link, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err = tx.QueryRow("SELECT COUNT(*) FROM accounts WHERE status != 'deleted'").Scan(&n); err != nil {
		return nil, "", err
	}
	if n > 0 {
		return nil, "", ErrSetupDone
	}
	now := s.now().UTC()
	if _, err = tx.Exec("UPDATE links SET revoked_at = ? WHERE kind = 'setup' AND used_at = 0 AND revoked_at = 0", toMillis(now)); err != nil {
		return nil, "", err
	}
	l := &Link{ID: newID(), Kind: LinkSetup, Role: RoleAdmin, CreatedAt: now, ExpiresAt: now.Add(setupLinkLifetime)}
	token, err := s.insertLink(tx, l, "")
	if err != nil {
		return nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditSetupLinkCreated, Details: map[string]any{"linkId": l.ID}}); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	l.Status = LinkPending
	return l, token, nil
}

// PrintSetupLink creates a setup link and prints it on stdout in the format of docs/auth.md section 7
func (s *Service) PrintSetupLink() error {
	_, token, err := s.CreateSetupLink()
	if err != nil {
		return err
	}
	var b strings.Builder
	if u := s.LinkURL(LinkSetup, token); u != nil {
		fmt.Fprintf(&b, "setup link: %s\n", *u)
	}
	fmt.Fprintf(&b, "setup token: %s\n", token)
	if _, err = os.Stdout.WriteString(b.String()); err != nil {
		return err
	}
	_ = os.Stdout.Sync()
	return nil
}

// ErrInvalidNote is returned for notes that are too long
var ErrInvalidNote = errors.New("the note has at most 200 characters")

// CreateInvite creates an invite link
func (s *Service) CreateInvite(by *User, role, note string, lifetime time.Duration, ri RequestInfo) (*Link, string, error) {
	if err := ValidateRole(role); err != nil {
		return nil, "", err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 200 {
		return nil, "", ErrInvalidNote
	}
	if lifetime <= 0 {
		lifetime = DefaultInviteLifetime
	}
	if lifetime > MaxInviteLifetime {
		lifetime = MaxInviteLifetime
	}
	now := s.now().UTC()
	l := &Link{ID: newID(), Kind: LinkInvite, Role: role, Note: note, CreatedAt: now, ExpiresAt: now.Add(lifetime)}
	token, err := s.insertLink(s.db, l, by.ID)
	if err != nil {
		return nil, "", err
	}
	s.Audit(AuditEvent{Type: AuditInviteCreated, ActorID: by.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"linkId": l.ID, "role": role, "expiresAt": l.ExpiresAt}})
	l.CreatedBy = s.userRef(by.ID)
	l.Status = LinkPending
	return l, token, nil
}

// CreateResetLink creates a reset link for an active account and revokes its older reset links
func (s *Service) CreateResetLink(by *User, accountID string, lifetime time.Duration, resetSecondFactors bool, ri RequestInfo) (*Link, string, error) {
	target := s.UserByID(accountID)
	if target == nil || target.Status == StatusDeleted {
		return nil, "", ErrNotFound
	}
	if target.Status != StatusActive {
		return nil, "", ErrAccountDisabled
	}
	if lifetime <= 0 {
		lifetime = DefaultResetLifetime
	}
	if lifetime > MaxResetLifetime {
		lifetime = MaxResetLifetime
	}
	now := s.now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec("UPDATE links SET revoked_at = ? WHERE kind = 'reset' AND account_id = ? AND used_at = 0 AND revoked_at = 0", toMillis(now), accountID); err != nil {
		return nil, "", err
	}
	l := &Link{ID: newID(), Kind: LinkReset, AccountID: &accountID, ResetSecondFactors: resetSecondFactors, CreatedAt: now, ExpiresAt: now.Add(lifetime)}
	token, err := s.insertLink(tx, l, by.ID)
	if err != nil {
		return nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditResetCreated, ActorID: by.ID, TargetID: accountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"linkId": l.ID, "resetSecondFactors": resetSecondFactors, "expiresAt": l.ExpiresAt}}); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	l.CreatedBy = s.userRef(by.ID)
	l.Status = LinkPending
	return l, token, nil
}

// pendingLink returns the pending link of a token, ErrInvalidLink otherwise
func (s *Service) pendingLink(db querier, token string) (*Link, error) {
	if !validTokenFormat(token, PrefixLink) {
		return nil, ErrInvalidLink
	}
	l, err := s.scanLink(db.QueryRow("SELECT "+linkColumns+" FROM links WHERE token_hash = ?", tokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidLink
	}
	if err != nil {
		return nil, err
	}
	if l.Status != LinkPending {
		return nil, ErrInvalidLink
	}
	if l.Kind == LinkReset {
		u := s.UserByID(*l.AccountID)
		if u == nil || u.Status != StatusActive {
			return nil, ErrInvalidLink
		}
	}
	return l, nil
}

// InspectLink returns a pending link and, for reset links, the account
func (s *Service) InspectLink(token string) (*Link, *User, error) {
	l, err := s.pendingLink(s.db, token)
	if err != nil {
		return nil, nil, err
	}
	if l.Kind == LinkSetup && !s.SetupRequired() {
		return nil, nil, ErrInvalidLink
	}
	var u *User
	if l.Kind == LinkReset {
		u = s.UserByID(*l.AccountID)
	}
	return l, u, nil
}

// NewAccount holds the fields of an account created from a setup or invite link
type NewAccount struct {
	Username    string
	DisplayName string
	Password    string
}

// RedeemLink redeems a setup, invite or reset link with a password. Setup and invite
// links create an account; reset links set the password of the account and revoke its
// sessions. It returns a session of the account.
func (s *Service) RedeemLink(token string, acc NewAccount, p SessionParams) (*LoginResult, error) {
	// check and hash the password before the write transaction: hashing takes a while
	l, target, err := s.InspectLink(token)
	if err != nil {
		return nil, err
	}
	username := acc.Username
	if l.Kind == LinkReset {
		username = target.Username
	} else if err = s.UsernameAvailable(acc.Username); err != nil {
		return nil, err
	}
	if err = CheckPassword(acc.Password, username); err != nil {
		return nil, err
	}
	h, err := HashPassword(acc.Password)
	if err != nil {
		return nil, err
	}
	return s.redeemLink(token, acc, p, func(tx *sql.Tx, l *Link, accountID string) error {
		now := toMillis(s.now())
		_, err := tx.Exec("UPDATE accounts SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?", h, now, now, accountID)
		return err
	})
}

// redeemLink runs the common part of a link redemption in one transaction: it validates
// the link, creates the account (setup, invite) or prepares the reset, calls credential
// to store the new credential, marks the link used and creates the session.
func (s *Service) redeemLink(token string, acc NewAccount, p SessionParams, credential func(tx *sql.Tx, l *Link, accountID string) error) (*LoginResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	l, err := s.pendingLink(tx, token)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	var accountID string
	auditType := AuditInviteRedeemed
	switch l.Kind {
	case LinkSetup, LinkInvite:
		if l.Kind == LinkSetup {
			var n int
			if err = tx.QueryRow("SELECT COUNT(*) FROM accounts WHERE status != 'deleted'").Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, ErrInvalidLink
			}
			auditType = AuditSetupCompleted
		}
		if err = ValidateUsername(acc.Username); err != nil {
			return nil, err
		}
		displayName, err := cleanDisplayName(acc.DisplayName, acc.Username)
		if err != nil {
			return nil, err
		}
		if taken, err := usernameTaken(tx, acc.Username, ""); err != nil {
			return nil, err
		} else if taken {
			return nil, ErrUsernameTaken
		}
		if accountID, err = insertAccount(tx, accountRecord{username: acc.Username, displayName: displayName, role: l.Role, status: StatusActive}, now); err != nil {
			return nil, err
		}
		if err = insertAudit(tx, now, AuditEvent{Type: AuditAccountCreated, ActorID: accountID, TargetID: accountID, IP: p.IP, UserAgent: p.UserAgent,
			Details: map[string]any{"username": acc.Username, "role": l.Role, "linkId": l.ID, "kind": l.Kind}}); err != nil {
			return nil, err
		}
	case LinkReset:
		auditType = AuditResetRedeemed
		accountID = *l.AccountID
		if _, err = revokeSessions(tx, accountID, ""); err != nil {
			return nil, err
		}
		if err = s.revokeCredentialsOnReset(tx, accountID, l.ResetSecondFactors); err != nil {
			return nil, err
		}
	}
	if err = credential(tx, l, accountID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE links SET used_at = ?, used_by = ? WHERE id = ?", toMillis(now), accountID, l.ID); err != nil {
		return nil, err
	}
	sess, sessionToken, err := s.createSession(tx, accountID, p, now)
	if err != nil {
		return nil, err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: auditType, ActorID: accountID, TargetID: accountID, IP: p.IP, UserAgent: p.UserAgent,
		Details: map[string]any{"linkId": l.ID, "resetSecondFactors": l.ResetSecondFactors}}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateCache()
	if l.Kind == LinkReset {
		s.notifyRevoked(RevokeEvent{AccountID: accountID, ExceptSessionID: sess.ID, Reason: RevokeSession})
	}
	u := s.UserByID(accountID)
	log.Printf("%s link redeemed by %s", l.Kind, u.Username)
	return &LoginResult{User: u, Session: sess, Token: sessionToken}, nil
}

// Links lists the links of a kind, newest first. status filters by state when not empty.
func (s *Service) Links(kind, status string) ([]*Link, error) {
	rows, err := s.db.Query("SELECT "+linkColumns+" FROM links WHERE kind = ? ORDER BY created_at DESC", kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Link{}
	for rows.Next() {
		l, err := s.scanLink(rows)
		if err != nil {
			return nil, err
		}
		if status == "" || l.Status == status {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

// RevokeLink revokes a pending link of a kind
func (s *Service) RevokeLink(kind, id string, by *User, ri RequestInfo) error {
	l, err := s.scanLink(s.db.QueryRow("SELECT "+linkColumns+" FROM links WHERE id = ? AND kind = ?", id, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if l.Status != LinkPending {
		return nil
	}
	if _, err = s.db.Exec("UPDATE links SET revoked_at = ? WHERE id = ?", toMillis(s.now()), id); err != nil {
		return err
	}
	t := AuditInviteRevoked
	target := ""
	if kind == LinkReset {
		t = AuditResetRevoked
		target = *l.AccountID
	}
	s.Audit(AuditEvent{Type: t, ActorID: by.ID, TargetID: target, IP: ri.IP, UserAgent: ri.UserAgent, Details: map[string]any{"linkId": id}})
	return nil
}
