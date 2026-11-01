package auth

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AccountDetails is what GET /account/ shows besides the user object
type AccountDetails struct {
	Email             string
	CreatedAt         time.Time
	HasPassword       bool
	TOTPEnabled       bool
	RecoveryCodesLeft int
	PasskeyCount      int
}

// AccountDetails returns the credential overview of an account
func (s *Service) AccountDetails(accountID string) (*AccountDetails, error) {
	var d AccountDetails
	var hash string
	var created int64
	err := s.db.QueryRow("SELECT email, created_at, password_hash FROM accounts WHERE id = ?", accountID).Scan(&d.Email, &created, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt, d.HasPassword = fromMillis(created), hash != ""
	if d.TOTPEnabled, err = totpEnabled(s.db, accountID); err != nil {
		return nil, err
	}
	if d.RecoveryCodesLeft, err = recoveryCodesLeft(s.db, accountID); err != nil {
		return nil, err
	}
	d.PasskeyCount = s.passkeyCount(s.db, accountID)
	return &d, nil
}

// ProfileUpdate holds the changed fields of an account; nil fields stay
type ProfileUpdate struct {
	Username    *string
	DisplayName *string
	Color       *string
	Email       *string
}

// UpdateProfile changes the username, display name, colour or email of an account. A
// former username stays reserved for the account.
func (s *Service) UpdateProfile(accountID string, upd ProfileUpdate, by *User, ri RequestInfo) (*User, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var cur User
	err = tx.QueryRow("SELECT id, username, display_name, email, color, role, status FROM accounts WHERE id = ?", accountID).
		Scan(&cur.ID, &cur.Username, &cur.DisplayName, &cur.Email, &cur.Color, &cur.Role, &cur.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if cur.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	next, err := applyProfileFields(cur, upd)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	changed := map[string]any{}
	if upd.Username != nil && *upd.Username != cur.Username {
		if err = changeUsername(tx, cur.Username, *upd.Username, accountID, now); err != nil {
			return nil, err
		}
		changed["username"] = map[string]string{"from": cur.Username, "to": *upd.Username}
		next.Username = *upd.Username
	}
	if next.DisplayName != cur.DisplayName {
		changed["displayName"] = next.DisplayName
	}
	if next.Color != cur.Color {
		changed["color"] = next.Color
	}
	if next.Email != cur.Email {
		changed["email"] = next.Email != ""
	}
	if len(changed) == 0 {
		return s.UserByID(accountID), nil
	}
	if _, err = tx.Exec("UPDATE accounts SET username = ?, username_key = ?, display_name = ?, email = ?, color = ?, updated_at = ? WHERE id = ?",
		next.Username, usernameKey(next.Username), next.DisplayName, next.Email, next.Color, toMillis(now), accountID); err != nil {
		return nil, err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAccountUpdated, ActorID: idOf(by), TargetID: accountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: changed}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateCache()
	return s.UserByID(accountID), nil
}

// applyProfileFields returns the user with the new display name, colour and email
func applyProfileFields(cur User, upd ProfileUpdate) (User, error) {
	next := cur
	if upd.DisplayName != nil {
		name, err := cleanDisplayName(*upd.DisplayName, "")
		if err != nil || name == "" {
			return User{}, ErrInvalidDisplayName
		}
		next.DisplayName = name
	}
	if upd.Color != nil {
		if !colorPattern.MatchString(*upd.Color) {
			return User{}, ErrInvalidColor
		}
		next.Color = strings.ToLower(*upd.Color)
	}
	if upd.Email != nil {
		email := strings.TrimSpace(*upd.Email)
		if err := validateEmail(email); err != nil {
			return User{}, err
		}
		next.Email = email
	}
	return next, nil
}

// changeUsername validates a new username and updates the former usernames of the account
func changeUsername(tx *sql.Tx, oldName string, newName string, accountID string, now time.Time) error {
	if err := ValidateUsername(newName); err != nil {
		return err
	}
	taken, err := usernameTaken(tx, newName, accountID)
	if err != nil {
		return err
	}
	if taken {
		return ErrUsernameTaken
	}
	oldKey, newKey := usernameKey(oldName), usernameKey(newName)
	if oldKey == newKey {
		return nil
	}
	// the old name stays reserved for this account; taking back a former name turns its alias
	// into the current name again
	if _, err = tx.Exec("INSERT OR IGNORE INTO username_aliases (username_key, account_id, created_at) VALUES (?, ?, ?)",
		oldKey, accountID, toMillis(now)); err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM username_aliases WHERE username_key = ? AND account_id = ?", newKey, accountID)
	return err
}

// ChangePassword sets a new password. With currentPassword, it must match (otherwise the
// caller has checked the step-up). All other sessions of the account end.
func (s *Service) ChangePassword(u *User, sess *Session, currentPassword *string, newPassword string, ri RequestInfo) error {
	key := usernameKey(u.Username)
	if currentPassword != nil {
		if err := s.checkAccount(key, ri); err != nil {
			return err
		}
		hash, err := s.passwordHash(s.db, u.ID)
		if err != nil {
			return err
		}
		ok := false
		if hash == "" {
			verifyDummy(*currentPassword)
		} else {
			ok, _ = VerifyPassword(hash, *currentPassword)
		}
		if !ok {
			s.fail(key)
			s.Audit(AuditEvent{Type: AuditReauthFailed, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
				Details: map[string]any{"method": "password", "action": "password-change"}})
			return ErrInvalidCredentials
		}
		s.succeed(key)
	}
	if err := CheckPassword(newPassword, u.Username); err != nil {
		return err
	}
	h, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.setPassword(u, sess, h, AuditPasswordChanged, ri)
}

// RemovePassword removes the password of an account that has a passkey
func (s *Service) RemovePassword(u *User, sess *Session, ri RequestInfo) error {
	if s.passkeyCount(s.db, u.ID) == 0 {
		return ErrLastLoginMethod
	}
	return s.setPassword(u, sess, "", AuditPasswordRemoved, ri)
}

func (s *Service) setPassword(u *User, sess *Session, hash, auditType string, ri RequestInfo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	if _, err = tx.Exec("UPDATE accounts SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?",
		hash, toMillis(now), toMillis(now), u.ID); err != nil {
		return err
	}
	except := ""
	if sess != nil {
		except = sess.ID
	}
	n, err := revokeSessions(tx, u.ID, except)
	if err != nil {
		return err
	}
	if err = revokeDeviceLinks(tx, u.ID, s.now()); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM app_tokens WHERE account_id = ?", u.ID); err != nil {
		return err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: auditType, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"revokedSessions": n}}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidateBasicCache(u.ID)
	if n > 0 {
		s.notifyRevoked(RevokeEvent{AccountID: u.ID, ExceptSessionID: except, Reason: RevokeSession})
	}
	return nil
}

// Reauthenticate confirms the user of a session again (step-up): with the password, plus a
// TOTP code when the account has TOTP, or only the code for accounts without password. It
// returns the end of the new step-up window.
func (s *Service) Reauthenticate(u *User, sess *Session, password, code string, ri RequestInfo) (time.Time, error) {
	key := usernameKey(u.Username)
	if err := s.checkAccount(key, ri); err != nil {
		return time.Time{}, err
	}
	hash, err := s.passwordHash(s.db, u.ID)
	if err != nil {
		return time.Time{}, err
	}
	totp, err := totpEnabled(s.db, u.ID)
	if err != nil {
		return time.Time{}, err
	}
	ok := hash != "" || totp
	if hash != "" {
		pwOK, _ := VerifyPassword(hash, password)
		ok = ok && pwOK
	} else if password != "" {
		verifyDummy(password)
		ok = false
	}
	if ok && totp {
		// only check (and spend) the code after the password matched
		var codeOK bool
		if codeOK, err = s.verifyTOTP(u.ID, code); err != nil {
			return time.Time{}, err
		}
		ok = codeOK
	}
	if !ok {
		s.fail(key)
		s.Audit(AuditEvent{Type: AuditReauthFailed, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
			Details: map[string]any{"method": "password", "totp": totp}})
		return time.Time{}, ErrInvalidCredentials
	}
	s.succeed(key)
	if err = s.markReauthenticated(sess); err != nil {
		return time.Time{}, err
	}
	s.Audit(AuditEvent{Type: AuditReauthSucceeded, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"method": "password", "totp": totp, "sessionId": sess.ID}})
	return ReauthenticatedUntil(sess), nil
}
