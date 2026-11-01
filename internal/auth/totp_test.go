package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

func TestTOTPCodeRFC6238(t *testing.T) {
	// RFC 6238 appendix B, SHA1, last 6 of the 8 digits
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(secret, totpStep(time.Unix(unix, 0))); got != want {
			t.Errorf("t=%d: %s, want %s", unix, got, want)
		}
	}
}

func FuzzMatchTOTP(f *testing.F) {
	f.Add("123456")
	f.Add(" 12 34 56 ")
	f.Add("")
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111109, 0)
	f.Fuzz(func(t *testing.T, code string) {
		if step := matchTOTP(secret, code, now, 0); step != 0 && totpCode(secret, step) != strings.ReplaceAll(strings.TrimSpace(code), " ", "") {
			t.Fatalf("%q matched step %d", code, step)
		}
	})
}

// enrollTOTP enables TOTP for a user and returns the raw secret and the recovery codes
func enrollTOTP(t *testing.T, s *Service, c *clock, u *User) ([]byte, []string) {
	t.Helper()
	e, err := s.BeginTOTP(u)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := totpEncoding.DecodeString(e.Secret)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := s.ConfirmTOTP(u, totpCode(secret, totpStep(c.now())), ri())
	if err != nil {
		t.Fatal(err)
	}
	return secret, codes
}

func TestTOTPEnrollment(t *testing.T) {
	s, c, admin := setupService(t, configuration.AuthConfiguration{})
	if _, err := s.ConfirmTOTP(admin, "123456", ri()); !errors.Is(err, ErrNoTOTPEnrollment) {
		t.Errorf("confirm without enrollment: %v", err)
	}
	e, err := s.BeginTOTP(admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Secret) != 32 || !e.ExpiresAt.Equal(c.now().Add(10*time.Minute)) {
		t.Errorf("enrollment = %+v", e)
	}
	wantURI := "otpauth://totp/MkDocsEditor:admin?secret=" + e.Secret + "&issuer=MkDocsEditor&algorithm=SHA1&digits=6&period=30"
	if e.OTPAuthURI != wantURI {
		t.Errorf("uri = %s", e.OTPAuthURI)
	}
	secret, _ := totpEncoding.DecodeString(e.Secret)
	// the secret is stored encrypted
	var stored []byte
	_ = s.db.QueryRow("SELECT secret FROM totp WHERE account_id = ?", admin.ID).Scan(&stored)
	if bytes.Contains(stored, secret) {
		t.Error("TOTP secret stored in plain text")
	}
	if totp, _ := s.secondFactors(admin.ID); totp {
		t.Error("pending TOTP counts as enabled")
	}
	if _, err = s.ConfirmTOTP(admin, "000000", ri()); !errors.Is(err, ErrInvalidTOTPCode) {
		t.Errorf("wrong code: %v", err)
	}
	c.advance(11 * time.Minute)
	if _, err = s.ConfirmTOTP(admin, totpCode(secret, totpStep(c.now())), ri()); !errors.Is(err, ErrNoTOTPEnrollment) {
		t.Errorf("expired enrollment: %v", err)
	}

	secret, codes := enrollTOTP(t, s, c, admin)
	if len(codes) != 10 {
		t.Fatalf("codes = %v", codes)
	}
	pattern := regexp.MustCompile(`^[2-9a-km-np-z]{4}-[2-9a-km-np-z]{4}-[2-9a-km-np-z]{4}$`)
	seen := map[string]bool{}
	for _, code := range codes {
		if !pattern.MatchString(code) || seen[code] {
			t.Errorf("bad or duplicate code %q", code)
		}
		seen[code] = true
	}
	if _, err = s.BeginTOTP(admin); !errors.Is(err, ErrTOTPAlreadyEnabled) {
		t.Errorf("second enrollment: %v", err)
	}
	d, _ := s.AccountDetails(admin.ID)
	if !d.TOTPEnabled || d.RecoveryCodesLeft != 10 || !d.HasPassword {
		t.Errorf("details = %+v", d)
	}
	if countAudit(t, s, AuditTOTPEnabled) != 1 || countAudit(t, s, AuditRecoveryCodesGenerated) != 1 {
		t.Error("audit events missing")
	}
	// the code used for the confirmation cannot be used again (replay)
	if ok, _ := s.verifyTOTP(admin.ID, totpCode(secret, totpStep(c.now()))); ok {
		t.Error("confirmation code accepted again")
	}

	newCodes, err := s.RegenerateRecoveryCodes(admin, ri())
	if err != nil || len(newCodes) != 10 || newCodes[0] == codes[0] {
		t.Fatal(newCodes, err)
	}
	if ok, _ := s.useRecoveryCode(admin.ID, codes[0]); ok {
		t.Error("old recovery code still works")
	}

	if err = setSetting(s.db, settingRequire2FA, "true"); err != nil {
		t.Fatal(err)
	}
	if err = s.DisableTOTP(admin, ri()); !errors.Is(err, ErrSecondFactorRequired) {
		t.Errorf("disable with require2FA: %v", err)
	}
	_ = setSetting(s.db, settingRequire2FA, "false")
	if err = s.DisableTOTP(admin, ri()); err != nil {
		t.Fatal(err)
	}
	if err = s.DisableTOTP(admin, ri()); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Errorf("disable twice: %v", err)
	}
	if _, err = s.RegenerateRecoveryCodes(admin, ri()); !errors.Is(err, ErrTOTPNotEnabled) {
		t.Errorf("codes without TOTP: %v", err)
	}
	if n, _ := recoveryCodesLeft(s.db, admin.ID); n != 0 {
		t.Errorf("%d recovery codes left after disabling", n)
	}
}

func TestSecondFactorLogin(t *testing.T) {
	s, c, admin := setupService(t, configuration.AuthConfiguration{RateLimits: configuration.RateLimitConfiguration{Disabled: true}})
	secret, codes := enrollTOTP(t, s, c, admin)
	if s.AuthenticateBasic("admin", "boss secret 2026!") != nil {
		t.Error("basic auth works for an account with TOTP")
	}

	r, err := s.LoginPassword("admin", "boss secret 2026!", sp(SessionCookie))
	if err != nil || r.Session != nil || r.Challenge == "" || strings.Join(r.Methods, ",") != "totp,recovery-code" {
		t.Fatal(r, err)
	}
	// the code of the enrollment step was used already
	_, err = s.LoginSecondFactor(r.Challenge, totpCode(secret, totpStep(c.now())), "", sp(SessionCookie))
	var sfe *SecondFactorError
	if !errors.As(err, &sfe) || sfe.AttemptsLeft != 4 {
		t.Fatalf("replayed code: %v", err)
	}
	c.advance(30 * time.Second)
	code := totpCode(secret, totpStep(c.now()))
	done, err := s.LoginSecondFactor(r.Challenge, code, "", sp(SessionBearer))
	if err != nil || done.Session == nil || done.Token == "" {
		t.Fatal(done, err)
	}
	if _, err = s.LoginSecondFactor(r.Challenge, code, "", sp(SessionBearer)); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("challenge used twice: %v", err)
	}

	// a code of the previous step is accepted once (clock skew), an older one never
	r, _ = s.LoginPassword("admin", "boss secret 2026!", sp(SessionCookie))
	if _, err = s.LoginSecondFactor(r.Challenge, totpCode(secret, totpStep(c.now())-2), "", sp("")); err == nil {
		t.Error("code from two steps ago accepted")
	}
	c.advance(30 * time.Second)
	if _, err = s.LoginSecondFactor(r.Challenge, code, "", sp("")); err == nil {
		t.Error("used code accepted in the next step")
	}

	// recovery codes ignore case, spaces and dashes and work once
	r, _ = s.LoginPassword("admin", "boss secret 2026!", sp(SessionCookie))
	messy := " " + strings.ToUpper(strings.ReplaceAll(codes[3], "-", " ")) + " "
	if _, err = s.LoginSecondFactor(r.Challenge, "", messy, sp("")); err != nil {
		t.Fatal(err)
	}
	r, _ = s.LoginPassword("admin", "boss secret 2026!", sp(SessionCookie))
	if _, err = s.LoginSecondFactor(r.Challenge, "", codes[3], sp("")); !errors.As(err, &sfe) {
		t.Errorf("recovery code used twice: %v", err)
	}
	if n, _ := recoveryCodesLeft(s.db, admin.ID); n != 9 || countAudit(t, s, AuditRecoveryCodeUsed) != 1 {
		t.Errorf("%d recovery codes left", n)
	}

	// five wrong attempts end the challenge
	r, _ = s.LoginPassword("admin", "boss secret 2026!", sp(SessionCookie))
	for range 4 {
		_, _ = s.LoginSecondFactor(r.Challenge, "000000", "", sp(""))
	}
	if _, err = s.LoginSecondFactor(r.Challenge, "000000", "", sp("")); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("fifth wrong attempt: %v", err)
	}
	c.advance(30 * time.Second)
	if _, err = s.LoginSecondFactor(r.Challenge, totpCode(secret, totpStep(c.now())), "", sp("")); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("challenge out of attempts still works: %v", err)
	}
	// challenges expire after 5 minutes
	r, _ = s.LoginPassword("admin", "boss secret 2026!", sp(SessionCookie))
	c.advance(5 * time.Minute)
	if _, err = s.LoginSecondFactor(r.Challenge, totpCode(secret, totpStep(c.now())), "", sp("")); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("expired challenge: %v", err)
	}
	if countAudit(t, s, AuditLoginSecondFactorFailed) < 6 {
		t.Error("failed second factors not audited")
	}

	// a reset link with resetSecondFactors removes TOTP; without, the reset sign-in skips it
	_, token, _ := s.CreateResetLink(admin, admin.ID, 0, false, ri())
	if _, err = s.RedeemLink(token, NewAccount{Password: "violets are blue 7"}, sp("")); err != nil {
		t.Fatal(err)
	}
	if totp, _ := s.secondFactors(admin.ID); !totp {
		t.Error("reset without resetSecondFactors removed TOTP")
	}
	_, token, _ = s.CreateResetLink(admin, admin.ID, 0, true, ri())
	if _, err = s.RedeemLink(token, NewAccount{Password: "plum tree 42 blossoms"}, sp("")); err != nil {
		t.Fatal(err)
	}
	if totp, _ := s.secondFactors(admin.ID); totp {
		t.Error("reset with resetSecondFactors kept TOTP")
	}
	if r, _ = s.LoginPassword("admin", "plum tree 42 blossoms", sp("")); r.Session == nil {
		t.Error("login after the reset still asks for a second factor")
	}
}

func TestReauthenticate(t *testing.T) {
	s, c, admin := setupService(t, configuration.AuthConfiguration{RateLimits: configuration.RateLimitConfiguration{Disabled: true}})
	r, _ := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	sess := r.Session
	if !s.RecentlyAuthenticated(sess) {
		t.Error("a fresh login is not recent")
	}
	c.advance(11 * time.Minute)
	if s.RecentlyAuthenticated(sess) {
		t.Error("login 11 minutes ago is recent")
	}
	if _, err := s.Reauthenticate(admin, sess, "wrong password!", "", ri()); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password: %v", err)
	}
	until, err := s.Reauthenticate(admin, sess, "boss secret 2026!", "", ri())
	if err != nil || !until.Equal(c.now().Add(ReauthWindow)) || !s.RecentlyAuthenticated(sess) {
		t.Fatal(until, err)
	}
	// stored: a fresh read of the session is recent too
	fresh, _ := s.SessionByID(sess.ID)
	if !s.RecentlyAuthenticated(fresh) {
		t.Error("step-up not stored")
	}

	secret, _ := enrollTOTP(t, s, c, admin)
	c.advance(30 * time.Second)
	if _, err = s.Reauthenticate(admin, sess, "boss secret 2026!", "", ri()); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("password without TOTP code: %v", err)
	}
	code := totpCode(secret, totpStep(c.now()))
	if _, err = s.Reauthenticate(admin, sess, "wrong password!", code, ri()); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password with code: %v", err)
	}
	// the code was not spent by the failed attempt
	if _, err = s.Reauthenticate(admin, sess, "boss secret 2026!", code, ri()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reauthenticate(admin, sess, "boss secret 2026!", code, ri()); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("code replayed: %v", err)
	}
	if countAudit(t, s, AuditReauthSucceeded) != 2 || countAudit(t, s, AuditReauthFailed) != 4 {
		t.Error("reauth audit events")
	}
}

func TestProfileAndPassword(t *testing.T) {
	s, _, admin := setupService(t, configuration.AuthConfiguration{RateLimits: configuration.RateLimitConfiguration{Disabled: true}})
	str := func(v string) *string { return &v }

	u, err := s.UpdateProfile(admin.ID, ProfileUpdate{DisplayName: str(" Boss "), Color: str("#ABCDEF"), Email: str("boss@example.com")}, admin, ri())
	if err != nil || u.DisplayName != "Boss" || u.Color != "#abcdef" || u.Email != "boss@example.com" {
		t.Fatal(u, err)
	}
	for _, upd := range []ProfileUpdate{{DisplayName: str("  ")}, {Color: str("red")}, {Email: str("no-at-sign")}, {Username: str("bad name")}} {
		if _, err = s.UpdateProfile(admin.ID, upd, admin, ri()); err == nil {
			t.Errorf("accepted %+v", upd)
		}
	}
	if u, _ = s.UpdateProfile(admin.ID, ProfileUpdate{Email: str("")}, admin, ri()); u.Email != "" {
		t.Error("email not removed")
	}

	// renaming keeps the old name reserved and resolvable
	if u, err = s.UpdateProfile(admin.ID, ProfileUpdate{Username: str("chief")}, admin, ri()); err != nil || u.Username != "chief" {
		t.Fatal(u, err)
	}
	if old := s.UserByUsername("admin"); old == nil || old.ID != admin.ID {
		t.Error("old username does not resolve")
	}
	if err = s.UsernameAvailable("Admin"); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("old username free for others: %v", err)
	}
	if r, _ := s.LoginPassword("admin", "boss secret 2026!", sp("")); r != nil {
		t.Error("login with the former username")
	}
	if u, err = s.UpdateProfile(admin.ID, ProfileUpdate{Username: str("Admin")}, admin, ri()); err != nil || u.Username != "Admin" {
		t.Fatal("taking back the old name:", u, err)
	}
	var aliases int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM username_aliases WHERE account_id = ?", admin.ID).Scan(&aliases)
	if aliases != 1 {
		t.Errorf("%d aliases, want 1 (chief)", aliases)
	}

	// password change: the current password must match, other sessions end
	a, _ := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	b, _ := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	if err = s.ChangePassword(u, a.Session, str("wrong password!"), "a brand new phrase", ri()); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong current password: %v", err)
	}
	if err = s.ChangePassword(u, a.Session, str("boss secret 2026!"), "password123", ri()); err == nil {
		t.Error("weak new password accepted")
	}
	if err = s.ChangePassword(u, a.Session, str("boss secret 2026!"), "a brand new phrase", ri()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AuthenticateSession(a.Token, ""); err != nil {
		t.Error("current session ended")
	}
	if _, _, err = s.AuthenticateSession(b.Token, ""); err == nil {
		t.Error("other session survived the password change")
	}
	if r, _ := s.LoginPassword("admin", "a brand new phrase", sp("")); r == nil || r.Session == nil {
		t.Error("new password does not work")
	}
	if err = s.RemovePassword(u, a.Session, ri()); !errors.Is(err, ErrLastLoginMethod) {
		t.Errorf("removed the only login method: %v", err)
	}
}

func TestServerKey(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, configuration.ServerConfiguration{})
	info, err := os.Stat(filepath.Join(dir, SecretKeyFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	c := newClock()
	s.now = c.now
	_, token, _ := s.CreateSetupLink()
	r, _ := s.RedeemLink(token, NewAccount{Username: "admin", Password: "boss secret 2026!"}, sp(""))
	secret, _ := enrollTOTP(t, s, c, r.User)
	_ = s.Close()

	// same key after a restart
	s = openTest(t, dir, configuration.ServerConfiguration{})
	s.now = c.now
	c.advance(30 * time.Second)
	if ok, err := s.verifyTOTP(r.User.ID, totpCode(secret, totpStep(c.now()))); !ok || err != nil {
		t.Fatal("TOTP after restart:", ok, err)
	}
	_ = s.Close()

	// another key (environment) cannot read the secret
	t.Setenv(SecretKeyEnv, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	s = openTest(t, dir, configuration.ServerConfiguration{})
	s.now = c.now
	c.advance(30 * time.Second)
	if _, err := s.verifyTOTP(r.User.ID, totpCode(secret, totpStep(c.now()))); !errors.Is(err, errSecretUnreadable) {
		t.Errorf("foreign key: %v", err)
	}
	_ = s.Close()

	t.Setenv(SecretKeyEnv, "too short")
	if _, err := Open(Options{StateDir: dir}); err == nil {
		t.Error("invalid key accepted")
	}
	t.Setenv(SecretKeyEnv, "")
	if err := os.WriteFile(filepath.Join(dir, SecretKeyFileName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{StateDir: dir}); err == nil {
		t.Error("invalid key file accepted")
	}
	// a configured file is used and created there
	other := filepath.Join(t.TempDir(), "secret.key")
	openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: configuration.AuthConfiguration{SecretKeyFile: other}})
	if _, err := os.Stat(other); err != nil {
		t.Error("configured key file not created:", err)
	}
}
