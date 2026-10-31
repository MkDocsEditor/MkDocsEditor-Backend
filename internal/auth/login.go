package auth

import (
	"errors"
	"time"
)

// Login errors
var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrAccountDisabled    = errors.New("the account is disabled")
	ErrChallengeExpired   = errors.New("the login has expired, start again")
)

const (
	challengeLifetime    = 5 * time.Minute
	challengeMaxAttempts = 5
)

// Second factor methods of a login challenge
const (
	MethodTOTP         = "totp"
	MethodRecoveryCode = "recovery-code"
	MethodPasskey      = "passkey"
)

// LoginResult is the outcome of a successful first login step: either a session, or a
// challenge for the second factor
type LoginResult struct {
	User    *User
	Session *Session
	Token   string

	Challenge string
	Methods   []string
	ExpiresAt time.Time
}

type challenge struct {
	accountID string
	usernameK string
	expires   time.Time
}

// LoginPassword checks a username and password. It returns a session, or a challenge when
// the account has a second factor.
func (s *Service) LoginPassword(username, password string, p SessionParams) (*LoginResult, error) {
	key := usernameKey(username)
	if err := s.checkAccount(key, p.RequestInfo); err != nil {
		return nil, err
	}
	u := s.loginUser(username)
	hash := ""
	if u != nil {
		var err error
		if hash, err = s.passwordHash(s.db, u.ID); err != nil {
			return nil, err
		}
	}
	if hash == "" {
		verifyDummy(password)
		s.fail(key)
		s.Audit(AuditEvent{Type: AuditLoginFailed, IP: p.IP, UserAgent: p.UserAgent, TargetID: idOf(u),
			Details: map[string]any{"username": truncate(username, 64), "method": "password"}})
		return nil, ErrInvalidCredentials
	}
	ok, rehash := VerifyPassword(hash, password)
	if !ok {
		s.fail(key)
		s.Audit(AuditEvent{Type: AuditLoginFailed, IP: p.IP, UserAgent: p.UserAgent, TargetID: u.ID,
			Details: map[string]any{"username": truncate(username, 64), "method": "password"}})
		return nil, ErrInvalidCredentials
	}
	s.succeed(key)
	if u.Status != StatusActive {
		s.Audit(AuditEvent{Type: AuditLoginFailed, IP: p.IP, UserAgent: p.UserAgent, TargetID: u.ID,
			Details: map[string]any{"username": u.Username, "reason": "disabled"}})
		return nil, ErrAccountDisabled
	}
	if rehash {
		_, _ = s.rehashPassword(u.ID, hash, password)
	}
	if methods := s.secondFactorMethods(u.ID); len(methods) > 0 {
		c, expires := s.newChallenge(u.ID, key)
		return &LoginResult{User: u, Challenge: c, Methods: methods, ExpiresAt: expires}, nil
	}
	return s.completeLogin(u, p, "password")
}

// loginUser returns the account with the current username (not a former one), not deleted
func (s *Service) loginUser(username string) *User {
	u := s.UserByUsername(username)
	if u == nil || u.Status == StatusDeleted || usernameKey(u.Username) != usernameKey(username) {
		return nil
	}
	return u
}

func idOf(u *User) string {
	if u == nil {
		return ""
	}
	return u.ID
}

// completeLogin creates the session of a fully authenticated login
func (s *Service) completeLogin(u *User, p SessionParams, method string) (*LoginResult, error) {
	now := s.now().UTC()
	sess, token, err := s.createSession(s.db, u.ID, p, now)
	if err != nil {
		return nil, err
	}
	s.invalidateCache()
	s.Audit(AuditEvent{Type: AuditLoginSucceeded, ActorID: u.ID, TargetID: u.ID, IP: p.IP, UserAgent: p.UserAgent,
		Details: map[string]any{"method": method, "sessionId": sess.ID, "sessionType": sess.Type}})
	return &LoginResult{User: s.UserByID(u.ID), Session: sess, Token: token}, nil
}

func (s *Service) newChallenge(accountID, key string) (string, time.Time) {
	token := newToken(PrefixChallenge)
	expires := s.now().Add(challengeLifetime)
	s.challengesMu.Lock()
	defer s.challengesMu.Unlock()
	now := s.now()
	for k, c := range s.challenges {
		if !now.Before(c.expires) {
			delete(s.challenges, k)
		}
	}
	s.challenges[string(tokenHash(token))] = &challenge{accountID: accountID, usernameK: key, expires: expires}
	return token, expires
}

// secondFactorMethods lists the second factors of an account for a login challenge
func (s *Service) secondFactorMethods(accountID string) []string {
	totp, passkeys := s.secondFactors(accountID)
	var methods []string
	if totp {
		methods = append(methods, MethodTOTP, MethodRecoveryCode)
	}
	if passkeys > 0 && s.PasskeysEnabled() {
		methods = append(methods, MethodPasskey)
	}
	return methods
}

// SecondFactorSetupRequired reports whether a user must still set up a second factor
// (instance setting require2FA)
func (s *Service) SecondFactorSetupRequired(u *User) bool {
	if u == nil || u.ID == AnonymousID || !s.Require2FA() {
		return false
	}
	totp, passkeys := s.secondFactors(u.ID)
	return !totp && passkeys == 0
}

const settingRequire2FA = "require_2fa"

// Require2FA returns the instance setting require2FA
func (s *Service) Require2FA() bool {
	v, err := s.setting(settingRequire2FA)
	return err == nil && v == "true"
}
