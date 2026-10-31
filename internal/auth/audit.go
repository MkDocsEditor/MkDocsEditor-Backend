package auth

import (
	"encoding/json"
	"log"
	"time"
)

// Audit event types, see docs/auth.md section 2
//
//nolint:gosec // event names, not credentials
const (
	AuditLoginSucceeded          = "login.succeeded"
	AuditLoginFailed             = "login.failed"
	AuditLoginSecondFactorFailed = "login.second-factor-failed"
	AuditLogout                  = "logout"
	AuditSessionRevoked          = "session.revoked"
	AuditReauthSucceeded         = "reauth.succeeded"
	AuditReauthFailed            = "reauth.failed"
	AuditPasswordChanged         = "password.changed"
	AuditPasswordRemoved         = "password.removed"
	AuditTOTPEnabled             = "totp.enabled"
	AuditTOTPDisabled            = "totp.disabled"
	AuditRecoveryCodesGenerated  = "recovery-codes.generated"
	AuditRecoveryCodeUsed        = "recovery-code.used"
	AuditPasskeyAdded            = "passkey.added"
	AuditPasskeyRenamed          = "passkey.renamed"
	AuditPasskeyRemoved          = "passkey.removed"
	AuditAppTokenCreated         = "app-token.created"
	AuditAppTokenRevoked         = "app-token.revoked"
	AuditDeviceLinkCreated       = "device-link.created"
	AuditDeviceLinkRevoked       = "device-link.revoked"
	AuditDeviceLinkRedeemed      = "device-link.redeemed"
	AuditSetupLinkCreated        = "setup.link-created"
	AuditSetupCompleted          = "setup.completed"
	AuditInviteCreated           = "invite.created"
	AuditInviteRevoked           = "invite.revoked"
	AuditInviteRedeemed          = "invite.redeemed"
	AuditResetCreated            = "reset.created"
	AuditResetRevoked            = "reset.revoked"
	AuditResetRedeemed           = "reset.redeemed"
	AuditAccountCreated          = "account.created"
	AuditAccountImported         = "account.imported"
	AuditAccountUpdated          = "account.updated"
	AuditAccountRoleChanged      = "account.role-changed"
	AuditAccountDisabled         = "account.disabled"
	AuditAccountEnabled          = "account.enabled"
	AuditAccountDeleted          = "account.deleted"
	AuditSettingsChanged         = "settings.changed"
	AuditRateLimitTriggered      = "rate-limit.triggered"
)

// AuditEvent is one entry of the audit log
type AuditEvent struct {
	ID        int64          `json:"id"`
	Time      time.Time      `json:"time"`
	Type      string         `json:"type"`
	ActorID   string         `json:"-"`
	TargetID  string         `json:"-"`
	IP        string         `json:"ip"`
	UserAgent string         `json:"userAgent"`
	Details   map[string]any `json:"details"`
}

func insertAudit(db execer, now time.Time, e AuditEvent) error {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	data, err := json.Marshal(details)
	if err != nil {
		return err
	}
	ua := e.UserAgent
	if len(ua) > 256 {
		ua = ua[:256]
	}
	_, err = db.Exec("INSERT INTO audit_events (time, type, actor_id, target_id, ip, user_agent, details) VALUES (?, ?, ?, ?, ?, ?, ?)",
		toMillis(now), e.Type, e.ActorID, e.TargetID, e.IP, ua, string(data))
	return err
}

// Audit records a security event. Failures are logged, never returned.
func (s *Service) Audit(e AuditEvent) {
	if err := insertAudit(s.db, s.now().UTC(), e); err != nil {
		log.Printf("Unable to write the audit log: %v", err)
	}
}
