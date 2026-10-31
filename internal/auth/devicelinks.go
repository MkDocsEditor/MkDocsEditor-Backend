package auth

import (
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"
)

// Device link states
const (
	DeviceLinkPending  = "pending"
	DeviceLinkRedeemed = "redeemed"
	DeviceLinkExpired  = "expired"
	DeviceLinkRevoked  = "revoked"
)

const (
	// DeviceLinkLifetime is how long a device link token can be redeemed
	DeviceLinkLifetime = 5 * time.Minute
	// maxPendingDeviceLinks per account; creating another revokes the oldest
	maxPendingDeviceLinks = 5
	// deviceLinkKeep is how long device links are kept after they expired
	deviceLinkKeep = 24 * time.Hour
)

// ErrDeviceLinkRedeemed is returned when a redeemed device link is revoked
var ErrDeviceLinkRedeemed = errors.New("the device link was already redeemed")

// DeviceLink pairs a new device with an account that is signed in elsewhere
type DeviceLink struct {
	ID                 string     `json:"id"`
	DeviceName         *string    `json:"deviceName"`
	CreatedAt          time.Time  `json:"createdAt"`
	ExpiresAt          time.Time  `json:"expiresAt"`
	Status             string     `json:"status"`
	RedeemedAt         *time.Time `json:"redeemedAt"`
	RedeemedDeviceName *string    `json:"redeemedDeviceName"`
	SessionID          *string    `json:"sessionId"`
	AccountID          string     `json:"-"`
}

// ValidDeviceLinkToken reports whether a string has the format of a device link token
func ValidDeviceLinkToken(token string) bool {
	return validTokenFormat(token, PrefixDevice)
}

const deviceLinkColumns = "id, account_id, device_name, created_at, expires_at, redeemed_at, redeemed_session, redeemed_device_name, revoked_at"

func (s *Service) scanDeviceLink(row interface{ Scan(...any) error }) (*DeviceLink, error) {
	var l DeviceLink
	var deviceName, session, redeemedName string
	var created, expires, redeemed, revoked int64
	if err := row.Scan(&l.ID, &l.AccountID, &deviceName, &created, &expires, &redeemed, &session, &redeemedName, &revoked); err != nil {
		return nil, err
	}
	l.CreatedAt, l.ExpiresAt = fromMillis(created), fromMillis(expires)
	if deviceName != "" {
		l.DeviceName = &deviceName
	}
	switch {
	case redeemed != 0:
		l.Status = DeviceLinkRedeemed
		r := fromMillis(redeemed)
		l.RedeemedAt, l.RedeemedDeviceName, l.SessionID = &r, &redeemedName, &session
	case revoked != 0:
		l.Status = DeviceLinkRevoked
	case !s.now().Before(l.ExpiresAt):
		l.Status = DeviceLinkExpired
	default:
		l.Status = DeviceLinkPending
	}
	return &l, nil
}

// CreateDeviceLink creates a device link for the account of a session. The caller checks
// the step-up.
func (s *Service) CreateDeviceLink(u *User, sess *Session, deviceName string, ri RequestInfo) (*DeviceLink, string, error) {
	deviceName = strings.TrimSpace(deviceName)
	if deviceName != "" {
		deviceName = cleanDeviceName(deviceName, "")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	// keep at most maxPendingDeviceLinks pending: revoke the oldest ones
	if _, err = tx.Exec(`UPDATE device_links SET revoked_at = ? WHERE id IN (
		SELECT id FROM device_links WHERE account_id = ? AND redeemed_at = 0 AND revoked_at = 0 AND expires_at > ?
		ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?)`, toMillis(now), u.ID, toMillis(now), maxPendingDeviceLinks-1); err != nil {
		return nil, "", err
	}
	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	token := newToken(PrefixDevice)
	l := &DeviceLink{ID: newID(), CreatedAt: now, ExpiresAt: now.Add(DeviceLinkLifetime), Status: DeviceLinkPending, AccountID: u.ID}
	if deviceName != "" {
		l.DeviceName = &deviceName
	}
	if _, err = tx.Exec("INSERT INTO device_links (id, account_id, token_hash, device_name, created_by_session, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		l.ID, u.ID, tokenHash(token), deviceName, sessionID, toMillis(now), toMillis(l.ExpiresAt)); err != nil {
		return nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditDeviceLinkCreated, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"deviceLinkId": l.ID, "deviceName": deviceName, "sessionId": sessionID}}); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	return l, token, nil
}

// DeviceLinks lists the device links of an account that expired less than 24 hours ago or
// are still pending, newest first
func (s *Service) DeviceLinks(accountID string) ([]*DeviceLink, error) {
	rows, err := s.db.Query("SELECT "+deviceLinkColumns+" FROM device_links WHERE account_id = ? AND expires_at > ? ORDER BY created_at DESC, rowid DESC",
		accountID, toMillis(s.now().Add(-deviceLinkKeep)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*DeviceLink{}
	for rows.Next() {
		l, err := s.scanDeviceLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeviceLink returns a device link of an account, ErrNotFound for unknown ids and links of
// other accounts
func (s *Service) DeviceLink(accountID, id string) (*DeviceLink, error) {
	l, err := s.scanDeviceLink(s.db.QueryRow("SELECT "+deviceLinkColumns+" FROM device_links WHERE id = ? AND account_id = ?", id, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

// RevokeDeviceLink revokes a pending device link. Expired and revoked links are left as
// they are; redeemed ones give ErrDeviceLinkRedeemed.
func (s *Service) RevokeDeviceLink(u *User, id string, ri RequestInfo) error {
	l, err := s.DeviceLink(u.ID, id)
	if err != nil {
		return err
	}
	switch l.Status {
	case DeviceLinkRedeemed:
		return ErrDeviceLinkRedeemed
	case DeviceLinkPending:
		res, err := s.db.Exec("UPDATE device_links SET revoked_at = ? WHERE id = ? AND redeemed_at = 0 AND revoked_at = 0", toMillis(s.now()), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// redeemed in the meantime
			return ErrDeviceLinkRedeemed
		}
		s.Audit(AuditEvent{Type: AuditDeviceLinkRevoked, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
			Details: map[string]any{"deviceLinkId": id}})
	}
	return nil
}

// RedeemDeviceLink exchanges a device link token for a session of its account. username is
// optional; when set, it must match the account. Unknown, used, expired and revoked tokens
// and a wrong username give ErrInvalidLink. The session is not recently authenticated.
func (s *Service) RedeemDeviceLink(token, username string, p SessionParams) (*LoginResult, error) {
	if !ValidDeviceLinkToken(token) {
		return nil, ErrInvalidLink
	}
	if p.Type == "" {
		p.Type = SessionBearer
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	l, err := s.scanDeviceLink(tx.QueryRow("SELECT "+deviceLinkColumns+" FROM device_links WHERE token_hash = ?", tokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidLink
	}
	if err != nil {
		return nil, err
	}
	if l.Status != DeviceLinkPending {
		return nil, ErrInvalidLink
	}
	u := s.UserByID(l.AccountID)
	if u == nil || u.Status == StatusDeleted {
		return nil, ErrInvalidLink
	}
	if username != "" && usernameKey(username) != usernameKey(u.Username) {
		return nil, ErrInvalidLink
	}
	if u.Status != StatusActive {
		return nil, ErrAccountDisabled
	}
	now := s.now().UTC()
	sess, sessionToken, err := s.createSession(tx, u.ID, p, now)
	if err != nil {
		return nil, err
	}
	// the new device has not authenticated itself: no step-up window
	if _, err = tx.Exec("UPDATE sessions SET reauth_at = 0 WHERE id = ?", sess.ID); err != nil {
		return nil, err
	}
	sess.ReauthAt = time.Time{}
	res, err := tx.Exec("UPDATE device_links SET redeemed_at = ?, redeemed_session = ?, redeemed_device_name = ? WHERE id = ? AND redeemed_at = 0 AND revoked_at = 0",
		toMillis(now), sess.ID, sess.DeviceName, l.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvalidLink
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditDeviceLinkRedeemed, ActorID: u.ID, TargetID: u.ID, IP: p.IP, UserAgent: p.UserAgent,
		Details: map[string]any{"deviceLinkId": l.ID, "deviceName": sess.DeviceName, "sessionId": sess.ID}}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	log.Printf("Device link redeemed by %s (%s)", u.Username, sess.DeviceName)
	return &LoginResult{User: u, Session: sess, Token: sessionToken}, nil
}

// revokeDeviceLinks revokes the pending device links of an account
func revokeDeviceLinks(db execer, accountID string, now time.Time) error {
	_, err := db.Exec("UPDATE device_links SET revoked_at = ? WHERE account_id = ? AND redeemed_at = 0 AND revoked_at = 0", toMillis(now), accountID)
	return err
}

// PasskeyCount returns the number of passkeys of an account
func (s *Service) PasskeyCount(accountID string) int {
	return s.passkeyCount(s.db, accountID)
}
