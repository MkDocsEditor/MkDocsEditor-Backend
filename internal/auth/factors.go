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

// passkeyCount returns the number of passkeys of an account
func (s *Service) passkeyCount(db querier, accountID string) int {
	return 0
}

// PasskeysEnabled reports whether passkeys are configured
func (s *Service) PasskeysEnabled() bool {
	return false
}

// RPID returns the WebAuthn relying party id, "" when passkeys are off
func (s *Service) RPID() string {
	return ""
}

// revokeCredentialsOnReset removes what a reset link revokes besides the sessions: with
// resetSecondFactors, TOTP and the recovery codes
func (s *Service) revokeCredentialsOnReset(tx *sql.Tx, accountID string, resetSecondFactors bool) error {
	if !resetSecondFactors {
		return nil
	}
	return deleteTOTP(tx, accountID)
}
