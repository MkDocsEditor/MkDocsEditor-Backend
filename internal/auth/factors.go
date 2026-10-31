package auth

import (
	"database/sql"
	"log"
)

// secondFactors returns whether an account has TOTP and how many passkeys it has
func (s *Service) secondFactors(accountID string) (totp bool, passkeys int) {
	totp, err := totpEnabled(s.db, accountID)
	if err != nil {
		log.Printf("Unable to read the second factors of %s: %v", accountID, err)
	}
	return totp, s.passkeyCount(s.db, accountID)
}

// revokeCredentialsOnReset removes what a reset link revokes besides the sessions: the app
// tokens and, with resetSecondFactors, TOTP, the recovery codes and the passkeys
func (s *Service) revokeCredentialsOnReset(tx *sql.Tx, accountID string, resetSecondFactors bool) error {
	if _, err := tx.Exec("DELETE FROM app_tokens WHERE account_id = ?", accountID); err != nil {
		return err
	}
	if err := revokeDeviceLinks(tx, accountID, s.now()); err != nil {
		return err
	}
	if !resetSecondFactors {
		return nil
	}
	if err := deleteTOTP(tx, accountID); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM passkeys WHERE account_id = ?", accountID)
	return err
}
