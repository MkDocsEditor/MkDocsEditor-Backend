package backend

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

// testTOTP computes a TOTP code like an authenticator app
func testTOTP(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30)) //nolint:gosec // positive
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1000000)
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (e *accountEnv) clock() *testClock {
	c := &testClock{t: time.Now().UTC().Truncate(30 * time.Second)}
	e.rs.authenticator.SetClock(c.now)
	return c
}

func TestAccountTOTPFlow(t *testing.T) {
	e := newAccountEnv(t, legacyBasicAuth)
	clk := e.clock()
	b := e.setupAdmin()

	r := b.do(http.MethodGet, "/account/", nil)
	expect(t, r, http.StatusOK, "")
	var acc AccountResponse
	r.json(t, &acc)
	if !acc.HasPassword || acc.TOTPEnabled || acc.Username != "markus" || acc.ReauthenticatedUntil == nil || acc.Require2FA {
		t.Errorf("account = %s", r.body)
	}

	r = b.do(http.MethodPatch, "/account/", map[string]any{"displayName": "Markus R.", "email": "m@example.com"})
	expect(t, r, http.StatusOK, "")
	r.json(t, &acc)
	if acc.DisplayName != "Markus R." || acc.Email != "m@example.com" {
		t.Errorf("patched = %s", r.body)
	}

	// step-up: sensitive actions need an authentication within 10 minutes
	clk.advance(11 * time.Minute)
	expect(t, b.do(http.MethodPatch, "/account/", map[string]any{"username": "mr"}), http.StatusForbidden, CodeReauthRequired)
	expect(t, b.do(http.MethodPost, "/account/totp/", map[string]any{}), http.StatusForbidden, CodeReauthRequired)
	expect(t, b.do(http.MethodPatch, "/account/", map[string]any{"username": "markus"}), http.StatusOK, "")
	expect(t, b.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "wrong password!"}), http.StatusUnauthorized, CodeInvalidCredentials)
	r = b.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"})
	expect(t, r, http.StatusOK, "")
	var re ReauthResponse
	r.json(t, &re)
	if !re.ReauthenticatedUntil.Equal(clk.now().Add(auth.ReauthWindow)) {
		t.Errorf("reauth = %s", r.body)
	}
	expect(t, b.do(http.MethodPatch, "/account/", map[string]any{"username": "mr"}), http.StatusOK, "")

	// TOTP enrollment
	r = b.do(http.MethodPost, "/account/totp/", map[string]any{})
	expect(t, r, http.StatusOK, "")
	var enroll auth.TOTPEnrollment
	r.json(t, &enroll)
	if enroll.OTPAuthURI != "otpauth://totp/MkDocsEditor:mr?secret="+enroll.Secret+"&issuer=MkDocsEditor&algorithm=SHA1&digits=6&period=30" {
		t.Errorf("uri = %s", enroll.OTPAuthURI)
	}
	expect(t, b.do(http.MethodPost, "/account/totp/confirm/", map[string]any{"code": "000000"}), http.StatusBadRequest, CodeInvalidSecondFactor)
	r = b.do(http.MethodPost, "/account/totp/confirm/", map[string]any{"code": testTOTP(t, enroll.Secret, clk.now())})
	expect(t, r, http.StatusOK, "")
	var codes RecoveryCodesResponse
	r.json(t, &codes)
	if len(codes.RecoveryCodes) != 10 {
		t.Fatalf("codes = %s", r.body)
	}
	expect(t, b.do(http.MethodPost, "/account/totp/", map[string]any{}), http.StatusConflict, CodeTOTPAlreadyEnabled)

	// login with the second factor
	expect(t, b.do(http.MethodPost, "/auth/logout/", nil), http.StatusNoContent, "")
	app := e.app()
	r = app.do(http.MethodPost, "/auth/login/", map[string]any{"username": "mr", "password": "a long admin secret", "sessionType": "bearer"})
	expect(t, r, http.StatusOK, "")
	var challenge SecondFactorRequiredResponse
	r.json(t, &challenge)
	if challenge.Status != "second-factor-required" || len(challenge.Methods) != 2 {
		t.Fatalf("login = %s", r.body)
	}
	expect(t, app.do(http.MethodPost, "/auth/login/second-factor/", map[string]any{"challenge": challenge.Challenge}), http.StatusBadRequest, CodeBadRequest)
	r = app.do(http.MethodPost, "/auth/login/second-factor/", map[string]any{"challenge": challenge.Challenge, "code": "000000"})
	expect(t, r, http.StatusUnauthorized, CodeInvalidSecondFactor)
	var errResult ErrorResult
	r.json(t, &errResult)
	if errResult.Details["attemptsLeft"] != float64(4) {
		t.Errorf("details = %v", errResult.Details)
	}
	clk.advance(30 * time.Second)
	r = app.do(http.MethodPost, "/auth/login/second-factor/", map[string]any{"challenge": challenge.Challenge,
		"code": testTOTP(t, enroll.Secret, clk.now()), "sessionType": "bearer", "deviceName": "Pixel 8"})
	expect(t, r, http.StatusOK, "")
	var sess SessionResponse
	r.json(t, &sess)
	if sess.Token == "" || sess.Session.DeviceName != "Pixel 8" {
		t.Fatalf("session = %s", r.body)
	}
	app.bearer = sess.Token
	expect(t, app.do(http.MethodPost, "/auth/login/second-factor/", map[string]any{"challenge": challenge.Challenge, "code": "123456"}),
		http.StatusUnauthorized, CodeChallengeExpired)

	// recovery code login in the browser
	r = b.do(http.MethodPost, "/auth/login/", map[string]any{"username": "mr", "password": "a long admin secret"})
	r.json(t, &challenge)
	expect(t, b.do(http.MethodPost, "/auth/login/second-factor/", map[string]any{"challenge": challenge.Challenge, "recoveryCode": codes.RecoveryCodes[0]}),
		http.StatusOK, "")
	if b.cookie == nil {
		t.Fatal("no session cookie after the second factor")
	}

	// basic auth is refused for accounts with a second factor
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/auth/whoami/", nil)
	req.SetBasicAuth("mr", "a long admin secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("basic auth with TOTP: %d", resp.StatusCode)
	}

	// step-up with password and code
	clk.advance(11 * time.Minute)
	expect(t, app.do(http.MethodPost, "/account/recovery-codes/", map[string]any{}), http.StatusForbidden, CodeReauthRequired)
	expect(t, app.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"}), http.StatusUnauthorized, CodeInvalidCredentials)
	expect(t, app.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret", "code": testTOTP(t, enroll.Secret, clk.now())}),
		http.StatusOK, "")
	r = app.do(http.MethodPost, "/account/recovery-codes/", map[string]any{})
	expect(t, r, http.StatusOK, "")
	r.json(t, &codes)
	r = app.do(http.MethodGet, "/account/", nil)
	r.json(t, &acc)
	if !acc.TOTPEnabled || acc.RecoveryCodesLeft != 10 || acc.PasskeyCount != 0 {
		t.Errorf("account = %s", r.body)
	}

	// password change and removal
	expect(t, app.do(http.MethodPut, "/account/password/", map[string]any{"currentPassword": "wrong password!", "newPassword": "a fresh admin phrase"}),
		http.StatusUnauthorized, CodeInvalidCredentials)
	expect(t, app.do(http.MethodPut, "/account/password/", map[string]any{"currentPassword": "a long admin secret", "newPassword": "short"}),
		http.StatusBadRequest, CodeWeakPassword)
	expect(t, app.do(http.MethodPut, "/account/password/", map[string]any{"currentPassword": "a long admin secret", "newPassword": "a fresh admin phrase"}),
		http.StatusNoContent, "")
	// the browser session ended, the app session stays
	expect(t, b.do(http.MethodGet, "/account/", nil), http.StatusUnauthorized, CodeUnauthenticated)
	expect(t, app.do(http.MethodDelete, "/account/password/", nil), http.StatusConflict, CodeLastLoginMethod)

	expect(t, app.do(http.MethodDelete, "/account/totp/", nil), http.StatusNoContent, "")
	expect(t, app.do(http.MethodPost, "/account/recovery-codes/", map[string]any{}), http.StatusConflict, CodeTOTPNotEnabled)
	expect(t, app.do(http.MethodDelete, "/account/totp/", nil), http.StatusConflict, CodeTOTPNotEnabled)
}

func TestAccountRequire2FA(t *testing.T) {
	e := newAccountEnv(t)
	e.clock()
	admin := e.setupAdmin()
	reader := e.invite(admin, auth.RoleReader, "kid", false)

	// account endpoints work for readers
	expect(t, reader.do(http.MethodPatch, "/account/", map[string]any{"displayName": "Kid"}), http.StatusOK, "")

	if err := e.rs.authenticator.SetRequire2FA(true, nil, auth.RequestInfo{}); err != nil {
		t.Fatal(err)
	}
	r := reader.do(http.MethodGet, "/auth/whoami/", nil)
	var who WhoAmIResponse
	r.json(t, &who)
	if !who.SecondFactorSetupRequired {
		t.Errorf("whoami = %s", r.body)
	}
	expect(t, reader.do(http.MethodGet, "/actors/", nil), http.StatusForbidden, CodeSecondFactorSetupRequired)
	expect(t, reader.do(http.MethodPatch, "/account/", map[string]any{"displayName": "K"}), http.StatusForbidden, CodeSecondFactorSetupRequired)
	r = reader.do(http.MethodGet, "/account/", nil)
	var acc AccountResponse
	r.json(t, &acc)
	if !acc.Require2FA || !acc.SecondFactorSetupRequired {
		t.Errorf("account = %s", r.body)
	}
	r = reader.do(http.MethodPost, "/account/totp/", map[string]any{})
	expect(t, r, http.StatusOK, "")
	var enroll auth.TOTPEnrollment
	r.json(t, &enroll)
	expect(t, reader.do(http.MethodPost, "/account/totp/confirm/", map[string]any{"code": testTOTP(t, enroll.Secret, time.Now())}), http.StatusOK, "")
	expect(t, reader.do(http.MethodGet, "/actors/", nil), http.StatusOK, "")
	expect(t, reader.do(http.MethodDelete, "/account/totp/", nil), http.StatusConflict, CodeSecondFactorRequired)
}

func TestAccountEndpointsWithoutAuth(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) { c.Server.Auth.Disabled = true })
	for _, path := range []string{"/account/", "/account/sessions/", "/admin/invites/"} {
		expect(t, e.app().do(http.MethodGet, path, nil), http.StatusForbidden, CodeForbidden)
	}
	expect(t, e.app().do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "x"}), http.StatusForbidden, CodeForbidden)
}
