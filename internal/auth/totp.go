package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1, which authenticator apps expect
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters of docs/auth.md section 3 (RFC 6238 defaults, understood by every app)
const (
	totpDigits         = 6
	totpPeriod         = 30 * time.Second
	totpSecretBytes    = 20
	totpEnrollLifetime = 10 * time.Minute

	recoveryCodeCount = 10
	// recovery code alphabet: 0-9a-z without 0, o, 1 and l
	recoveryAlphabet = "23456789abcdefghijkmnpqrstuvwxyz"
)

// TOTP and second factor errors
var (
	ErrTOTPAlreadyEnabled   = errors.New("TOTP is already enabled")
	ErrTOTPNotEnabled       = errors.New("TOTP is not enabled")
	ErrNoTOTPEnrollment     = errors.New("no TOTP enrollment is pending, start again")
	ErrInvalidTOTPCode      = errors.New("the code is wrong")
	ErrSecondFactorRequired = errors.New("the instance requires a second factor; add a passkey first")
	ErrLastLoginMethod      = errors.New("the account would have neither a password nor a passkey")
)

// SecondFactorError is a wrong TOTP or recovery code at login
type SecondFactorError struct {
	AttemptsLeft int
}

func (e *SecondFactorError) Error() string {
	return "the code is wrong"
}

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpCode computes the code of a time step (RFC 4226 dynamic truncation)
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // steps are positive
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1000000)
}

func totpStep(t time.Time) int64 {
	return t.Unix() / int64(totpPeriod/time.Second)
}

// matchTOTP returns the step of the window (now ±1) that the code matches and that is
// newer than lastStep, or 0
func matchTOTP(secret []byte, code string, now time.Time, lastStep int64) int64 {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0
	}
	cur := totpStep(now)
	var found int64
	for step := cur - 1; step <= cur+1; step++ {
		// compare every step, so the time does not depend on which one matched
		if hmac.Equal([]byte(totpCode(secret, step)), []byte(code)) && step > lastStep && found == 0 {
			found = step
		}
	}
	return found
}

// TOTPEnrollment is a pending TOTP secret, shown to the user once
type TOTPEnrollment struct {
	Secret     string    `json:"secret"`
	OTPAuthURI string    `json:"otpauthUri"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

func (s *Service) otpauthURI(username, secret string) string {
	issuer := url.PathEscape(s.cfg.issuer)
	q := "secret=" + secret + "&issuer=" + issuer + "&algorithm=SHA1&digits=6&period=30"
	return "otpauth://totp/" + issuer + ":" + url.PathEscape(username) + "?" + q
}

// totpState returns whether TOTP is enabled for an account
func totpEnabled(db querier, accountID string) (bool, error) {
	var enabled int
	err := db.QueryRow("SELECT enabled FROM totp WHERE account_id = ?", accountID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled == 1, err
}

// BeginTOTP creates a pending TOTP secret for an account (replacing an older pending one)
func (s *Service) BeginTOTP(u *User) (*TOTPEnrollment, error) {
	enabled, err := totpEnabled(s.db, u.ID)
	if err != nil {
		return nil, err
	}
	if enabled {
		return nil, ErrTOTPAlreadyEnabled
	}
	secret := make([]byte, totpSecretBytes)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	sealed, err := seal(s.keys.totp, secret, u.ID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	expires := now.Add(totpEnrollLifetime)
	_, err = s.db.Exec(`INSERT INTO totp (account_id, secret, enabled, pending_expires_at, last_step, created_at) VALUES (?, ?, 0, ?, 0, ?)
		ON CONFLICT (account_id) DO UPDATE SET secret = excluded.secret, pending_expires_at = excluded.pending_expires_at,
		last_step = 0, created_at = excluded.created_at WHERE enabled = 0`, u.ID, sealed, toMillis(expires), toMillis(now))
	if err != nil {
		return nil, err
	}
	encoded := totpEncoding.EncodeToString(secret)
	return &TOTPEnrollment{Secret: encoded, OTPAuthURI: s.otpauthURI(u.Username, encoded), ExpiresAt: expires}, nil
}

// ConfirmTOTP activates a pending TOTP secret with a current code and returns new recovery codes
func (s *Service) ConfirmTOTP(u *User, code string, ri RequestInfo) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var sealed []byte
	var enabled int
	var expires int64
	err = tx.QueryRow("SELECT secret, enabled, pending_expires_at FROM totp WHERE account_id = ?", u.ID).Scan(&sealed, &enabled, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoTOTPEnrollment
	}
	if err != nil {
		return nil, err
	}
	if enabled == 1 {
		return nil, ErrTOTPAlreadyEnabled
	}
	now := s.now().UTC()
	if !now.Before(fromMillis(expires)) {
		return nil, ErrNoTOTPEnrollment
	}
	secret, err := unseal(s.keys.totp, sealed, u.ID)
	if err != nil {
		return nil, err
	}
	step := matchTOTP(secret, code, now, 0)
	if step == 0 {
		return nil, ErrInvalidTOTPCode
	}
	if _, err = tx.Exec("UPDATE totp SET enabled = 1, pending_expires_at = 0, last_step = ? WHERE account_id = ?", step, u.ID); err != nil {
		return nil, err
	}
	codes, err := s.replaceRecoveryCodes(tx, u.ID, now)
	if err != nil {
		return nil, err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditTOTPEnabled, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent}); err != nil {
		return nil, err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditRecoveryCodesGenerated, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateBasicCache(u.ID)
	return codes, nil
}

// DisableTOTP removes TOTP and the recovery codes of an account
func (s *Service) DisableTOTP(u *User, ri RequestInfo) error {
	enabled, err := totpEnabled(s.db, u.ID)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrTOTPNotEnabled
	}
	if s.Require2FA() && s.passkeyCount(s.db, u.ID) == 0 {
		return ErrSecondFactorRequired
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = deleteTOTP(tx, u.ID); err != nil {
		return err
	}
	if err = insertAudit(tx, s.now(), AuditEvent{Type: AuditTOTPDisabled, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent}); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteTOTP(db execer, accountID string) error {
	if _, err := db.Exec("DELETE FROM totp WHERE account_id = ?", accountID); err != nil {
		return err
	}
	_, err := db.Exec("DELETE FROM recovery_codes WHERE account_id = ?", accountID)
	return err
}

// verifyTOTP checks a code of an enabled TOTP secret and marks its step used (replay
// protection). It returns false for wrong or already used codes.
func (s *Service) verifyTOTP(accountID, code string) (bool, error) {
	var sealed []byte
	var lastStep int64
	err := s.db.QueryRow("SELECT secret, last_step FROM totp WHERE account_id = ? AND enabled = 1", accountID).Scan(&sealed, &lastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	secret, err := unseal(s.keys.totp, sealed, accountID)
	if err != nil {
		return false, err
	}
	step := matchTOTP(secret, code, s.now(), lastStep)
	if step == 0 {
		return false, nil
	}
	// only one request can move last_step past this step
	res, err := s.db.Exec("UPDATE totp SET last_step = ? WHERE account_id = ? AND last_step < ?", step, accountID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// normalizeRecoveryCode removes case, spaces and dashes
func normalizeRecoveryCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '\t':
			return -1
		}
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, code)
}

func (s *Service) recoveryCodeHash(accountID, code string) []byte {
	mac := hmac.New(sha256.New, s.keys.recoveryCodes)
	mac.Write([]byte(accountID + "\x00" + normalizeRecoveryCode(code)))
	return mac.Sum(nil)
}

func newRecoveryCode() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		// 256 is a multiple of 32, so this has no bias
		sb.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
	}
	return sb.String()
}

func (s *Service) replaceRecoveryCodes(tx *sql.Tx, accountID string, now time.Time) ([]string, error) {
	if _, err := tx.Exec("DELETE FROM recovery_codes WHERE account_id = ?", accountID); err != nil {
		return nil, err
	}
	codes := make([]string, 0, recoveryCodeCount)
	for len(codes) < recoveryCodeCount {
		code := newRecoveryCode()
		res, err := tx.Exec("INSERT OR IGNORE INTO recovery_codes (account_id, code_hash, created_at) VALUES (?, ?, ?)",
			accountID, s.recoveryCodeHash(accountID, code), toMillis(now))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			codes = append(codes, code)
		}
	}
	return codes, nil
}

// RegenerateRecoveryCodes replaces the recovery codes of an account with TOTP
func (s *Service) RegenerateRecoveryCodes(u *User, ri RequestInfo) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	enabled, err := totpEnabled(tx, u.ID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrTOTPNotEnabled
	}
	now := s.now().UTC()
	codes, err := s.replaceRecoveryCodes(tx, u.ID, now)
	if err != nil {
		return nil, err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditRecoveryCodesGenerated, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent}); err != nil {
		return nil, err
	}
	return codes, tx.Commit()
}

// useRecoveryCode spends a recovery code; it returns false for unknown or used codes
func (s *Service) useRecoveryCode(accountID, code string) (bool, error) {
	norm := normalizeRecoveryCode(code)
	if len(norm) != 12 {
		return false, nil
	}
	res, err := s.db.Exec("DELETE FROM recovery_codes WHERE account_id = ? AND code_hash = ?", accountID, s.recoveryCodeHash(accountID, norm))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func recoveryCodesLeft(db querier, accountID string) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM recovery_codes WHERE account_id = ?", accountID).Scan(&n)
	return n, err
}
