package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrAdminGuard is returned when an admin action would lock the instance out: changing
// oneself, or the last active admin
type ErrAdminGuard struct {
	Reason string
}

func (e *ErrAdminGuard) Error() string {
	return e.Reason
}

// AdminUser is an account as the admin API lists it
type AdminUser struct {
	User
	Email        string    `json:"email"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	LastLoginAt  time.Time `json:"lastLoginAt"`
	HasPassword  bool      `json:"hasPassword"`
	TOTPEnabled  bool      `json:"totpEnabled"`
	PasskeyCount int       `json:"passkeyCount"`
	SessionCount int       `json:"sessionCount"`
}

// MarshalJSON writes lastLoginAt as null for accounts that never logged in
func (u AdminUser) MarshalJSON() ([]byte, error) {
	type plain AdminUser
	var last *time.Time
	if !u.LastLoginAt.IsZero() {
		last = &u.LastLoginAt
	}
	return json.Marshal(struct {
		plain
		LastLoginAt *time.Time `json:"lastLoginAt"`
	}{plain(u), last})
}

const adminUserQuery = `SELECT a.id, a.username, a.display_name, a.email, a.color, a.role, a.status, a.created_at, a.last_login_at, a.password_hash != '',
	COALESCE((SELECT enabled FROM totp WHERE account_id = a.id), 0),
	(SELECT COUNT(*) FROM passkeys WHERE account_id = a.id),
	(SELECT COUNT(*) FROM sessions WHERE account_id = a.id AND expires_at > ? AND last_used_at > ?)
	FROM accounts a`

func (s *Service) scanAdminUser(row interface{ Scan(...any) error }) (*AdminUser, error) {
	var u AdminUser
	var created, last int64
	var totp int
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.Color, &u.Role, &u.Status, &created, &last, &u.HasPassword,
		&totp, &u.PasskeyCount, &u.SessionCount); err != nil {
		return nil, err
	}
	u.User.Email, u.User.Status = u.Email, u.Status
	u.CreatedAt, u.LastLoginAt, u.TOTPEnabled = fromMillis(created), fromMillis(last), totp == 1
	return &u, nil
}

func (s *Service) sessionWindow() (int64, int64) {
	now := s.now()
	return toMillis(now), toMillis(now.Add(-s.cfg.idleTimeout))
}

// AdminUsers lists the accounts, filtered by status when statuses is not empty
func (s *Service) AdminUsers(statuses []string) ([]*AdminUser, error) {
	now, idle := s.sessionWindow()
	rows, err := s.db.Query(adminUserQuery+" ORDER BY a.username_key", now, idle)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	want := map[string]bool{}
	for _, st := range statuses {
		want[st] = true
	}
	out := []*AdminUser{}
	for rows.Next() {
		u, err := s.scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		if len(want) == 0 || want[u.Status] {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// AdminUser returns one account for the admin API
func (s *Service) AdminUser(id string) (*AdminUser, error) {
	now, idle := s.sessionWindow()
	u, err := s.scanAdminUser(s.db.QueryRow(adminUserQuery+" WHERE a.id = ?", now, idle, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// activeAdmins counts the active admins except one account
func activeAdmins(db querier, exceptID string) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM accounts WHERE role = 'admin' AND status = 'active' AND id != ?", exceptID).Scan(&n)
	return n, err
}

// AdminUpdate holds the fields an admin changes; nil fields stay
type AdminUpdate struct {
	Role   *string
	Status *string
	ProfileUpdate
}

// UpdateUser applies an admin change to an account. Role changes close the user's
// websockets (4003); disabling revokes the sessions, app tokens and reset links.
func (s *Service) UpdateUser(by *User, id string, upd AdminUpdate, ri RequestInfo) (*AdminUser, error) {
	target, err := s.AdminUser(id)
	if err != nil {
		return nil, err
	}
	if target.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	if upd.Role != nil {
		if err = ValidateRole(*upd.Role); err != nil {
			return nil, err
		}
	}
	if upd.Status != nil && *upd.Status != StatusActive && *upd.Status != StatusDisabled {
		return nil, errors.New("status must be active or disabled")
	}
	roleChange := upd.Role != nil && *upd.Role != target.Role
	statusChange := upd.Status != nil && *upd.Status != target.Status
	disabling := statusChange && *upd.Status == StatusDisabled
	if target.Role == RoleAdmin && (roleChange || disabling) {
		if err = s.guardAdmin(by, id); err != nil {
			return nil, err
		}
	}
	if upd.ProfileUpdate != (ProfileUpdate{}) {
		if _, err = s.UpdateProfile(id, upd.ProfileUpdate, by, ri); err != nil {
			return nil, err
		}
	}
	if roleChange || statusChange {
		if err = s.applyRoleAndStatus(by, target, upd, roleChange, statusChange, ri); err != nil {
			return nil, err
		}
		s.invalidateCache()
		s.invalidateBasicCache(id)
		switch {
		case disabling:
			s.notifyRevoked(RevokeEvent{AccountID: id, Reason: RevokeDisabled})
		case roleChange:
			s.notifyRevoked(RevokeEvent{AccountID: id, Reason: RevokeRole})
		}
	}
	return s.AdminUser(id)
}

// guardAdmin refuses to demote or disable oneself and the last active admin
func (s *Service) guardAdmin(by *User, id string) error {
	if id == by.ID {
		return &ErrAdminGuard{Reason: "admins cannot demote or disable themselves"}
	}
	n, err := activeAdmins(s.db, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return &ErrAdminGuard{Reason: "the last active admin cannot be demoted or disabled"}
	}
	return nil
}

// applyRoleAndStatus writes a change of role and status with its audit events in one transaction
func (s *Service) applyRoleAndStatus(by *User, target *AdminUser, upd AdminUpdate, roleChange, statusChange bool, ri RequestInfo) error {
	id := target.ID
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	if roleChange {
		if _, err = tx.Exec("UPDATE accounts SET role = ?, updated_at = ? WHERE id = ?", *upd.Role, toMillis(now), id); err != nil {
			return err
		}
		if err = insertAudit(tx, now, AuditEvent{Type: AuditAccountRoleChanged, ActorID: by.ID, TargetID: id, IP: ri.IP, UserAgent: ri.UserAgent,
			Details: map[string]any{"from": target.Role, "to": *upd.Role}}); err != nil {
			return err
		}
	}
	if statusChange {
		if _, err = tx.Exec("UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?", *upd.Status, toMillis(now), id); err != nil {
			return err
		}
		t := AuditAccountEnabled
		if *upd.Status == StatusDisabled {
			t = AuditAccountDisabled
			if err = revokeAccess(tx, id, now); err != nil {
				return err
			}
		}
		if err = insertAudit(tx, now, AuditEvent{Type: t, ActorID: by.ID, TargetID: id, IP: ri.IP, UserAgent: ri.UserAgent}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// revokeAccess ends the sessions and app tokens of an account and revokes its reset links
func revokeAccess(tx *sql.Tx, accountID string, now time.Time) error {
	if _, err := revokeSessions(tx, accountID, ""); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM app_tokens WHERE account_id = ?", accountID); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE links SET revoked_at = ? WHERE kind = 'reset' AND account_id = ? AND used_at = 0 AND revoked_at = 0", toMillis(now), accountID); err != nil {
		return err
	}
	return revokeDeviceLinks(tx, accountID, now)
}

// DeleteUser turns an account into a tombstone: id, username, display name and colour stay
// for authorship and history, all credentials go
func (s *Service) DeleteUser(by *User, id string, ri RequestInfo) error {
	target, err := s.AdminUser(id)
	if err != nil {
		return err
	}
	if target.Status == StatusDeleted {
		return ErrNotFound
	}
	if id == by.ID {
		return &ErrAdminGuard{Reason: "admins cannot delete themselves"}
	}
	if target.Role == RoleAdmin && target.Status == StatusActive {
		n, err := activeAdmins(s.db, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return &ErrAdminGuard{Reason: "the last active admin cannot be deleted"}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	if err = revokeAccess(tx, id, now); err != nil {
		return err
	}
	if err = deleteTOTP(tx, id); err != nil {
		return err
	}
	for _, q := range []string{"DELETE FROM passkeys WHERE account_id = ?", "DELETE FROM webauthn_handles WHERE account_id = ?"} {
		if _, err = tx.Exec(q, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE accounts SET status = 'deleted', password_hash = '', email = '', deleted_at = ?, updated_at = ? WHERE id = ?",
		toMillis(now), toMillis(now), id); err != nil {
		return err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAccountDeleted, ActorID: by.ID, TargetID: id, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"username": target.Username}}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidateCache()
	s.invalidateBasicCache(id)
	s.notifyRevoked(RevokeEvent{AccountID: id, Reason: RevokeDisabled})
	return nil
}

// AuditQuery filters the audit log
type AuditQuery struct {
	Limit     int
	Before    int64
	Type      string
	AccountID string
}

// AuditEntry is an audit event with the names of the accounts
type AuditEntry struct {
	AuditEvent
	Actor  *LinkUserRef `json:"actor"`
	Target *LinkUserRef `json:"target"`
}

// AuditLog returns audit events, newest first, and the id to continue with (0 at the end)
func (s *Service) AuditLog(q AuditQuery) ([]*AuditEntry, int64, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	rows, err := s.db.Query(`SELECT id, time, type, actor_id, target_id, ip, user_agent, details FROM audit_events
		WHERE (? = 0 OR id < ?) AND (? = '' OR type = ?) AND (? = '' OR actor_id = ? OR target_id = ?)
		ORDER BY id DESC LIMIT ?`, q.Before, q.Before, q.Type, q.Type, q.AccountID, q.AccountID, q.AccountID, q.Limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []*AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var t int64
		var details string
		if err = rows.Scan(&e.ID, &t, &e.Type, &e.ActorID, &e.TargetID, &e.IP, &e.UserAgent, &details); err != nil {
			return nil, 0, err
		}
		e.Time = fromMillis(t)
		_ = json.Unmarshal([]byte(details), &e.Details)
		if e.ActorID != "" {
			e.Actor = s.userRef(e.ActorID)
		}
		if e.TargetID != "" {
			e.Target = s.userRef(e.TargetID)
		}
		out = append(out, &e)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > q.Limit {
		out = out[:q.Limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}
