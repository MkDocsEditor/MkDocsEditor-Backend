package auth

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

// avatarHashLength is the number of hex characters of the sha256 that name an avatar
const avatarHashLength = 16

func avatarOrNil(hash string) *string {
	if hash == "" {
		return nil
	}
	return &hash
}

// AvatarHash returns the version of an avatar image
func AvatarHash(image []byte) string {
	sum := sha256.Sum256(image)
	return hex.EncodeToString(sum[:])[:avatarHashLength]
}

// SetAvatar stores the profile picture of an account. image must already be normalized; it is
// stored as it is.
func (s *Service) SetAvatar(accountID string, image []byte, by *User, ri RequestInfo) (*User, error) {
	return s.changeAvatar(accountID, image, by, ri)
}

// RemoveAvatar removes the profile picture of an account. An account without one is no error.
func (s *Service) RemoveAvatar(accountID string, by *User, ri RequestInfo) (*User, error) {
	return s.changeAvatar(accountID, nil, by, ri)
}

func (s *Service) changeAvatar(accountID string, image []byte, by *User, ri RequestInfo) (*User, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var status, current string
	err = tx.QueryRow("SELECT status, avatar FROM accounts WHERE id = ?", accountID).Scan(&status, &current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status == StatusDeleted) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	hash := ""
	if image != nil {
		hash = AvatarHash(image)
	}
	if hash == current {
		return s.UserByID(accountID), nil
	}
	now := s.now().UTC()
	if image == nil {
		_, err = tx.Exec("DELETE FROM avatars WHERE account_id = ?", accountID)
	} else {
		_, err = tx.Exec(`INSERT INTO avatars (account_id, hash, image, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (account_id) DO UPDATE SET hash = excluded.hash, image = excluded.image, updated_at = excluded.updated_at`,
			accountID, hash, image, toMillis(now))
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE accounts SET avatar = ?, updated_at = ? WHERE id = ?", hash, toMillis(now), accountID); err != nil {
		return nil, err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAccountUpdated, ActorID: idOf(by), TargetID: accountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"avatar": image != nil}}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateCache()
	return s.UserByID(accountID), nil
}

// Avatar returns the image of the current avatar of an account. ErrNotFound if hash is not the
// current one.
func (s *Service) Avatar(accountID string, hash string) ([]byte, error) {
	var image []byte
	err := s.db.QueryRow("SELECT a.image FROM avatars a JOIN accounts acc ON acc.id = a.account_id WHERE a.account_id = ? AND a.hash = ? AND acc.avatar = ? AND acc.status != ?",
		accountID, hash, hash, StatusDeleted).Scan(&image)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return image, err
}
