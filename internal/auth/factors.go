package auth

import "database/sql"

// secondFactors returns whether an account has TOTP and how many passkeys it has
func (s *Service) secondFactors(accountID string) (totp bool, passkeys int) {
	return false, 0
}

// PasskeysEnabled reports whether passkeys are configured
func (s *Service) PasskeysEnabled() bool {
	return false
}

// RPID returns the WebAuthn relying party id, "" when passkeys are off
func (s *Service) RPID() string {
	return ""
}

// revokeCredentialsOnReset removes what a reset link revokes besides the sessions
func (s *Service) revokeCredentialsOnReset(tx *sql.Tx, accountID string, resetSecondFactors bool) error {
	return nil
}
