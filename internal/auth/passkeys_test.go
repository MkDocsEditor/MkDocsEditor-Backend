package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth/passkeytest"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

const testDebugFingerprint = "74:A7:45:FA:C2:4A:BF:BD:F0:A7:30:83:18:71:FE:A6:DE:24:6C:6B:D2:E3:F8:62:49:24:27:1F:C4:D3:70:48"

func passkeyConf() configuration.AuthConfiguration {
	return configuration.AuthConfiguration{
		ClientURL:  "https://edit.wiki.example.com",
		RateLimits: configuration.RateLimitConfiguration{Disabled: true},
		WebAuthn: configuration.WebAuthnConfiguration{
			RPID:    "wiki.example.com",
			Android: []configuration.AndroidAppConfiguration{{PackageName: "de.markusressel.mkdocseditor.debug", SHA256CertFingerprints: []string{testDebugFingerprint}}},
		},
	}
}

func webAuthenticator() *passkeytest.Authenticator {
	return &passkeytest.Authenticator{RPID: "wiki.example.com", Origin: "https://edit.wiki.example.com", AAGUID: passkeytest.Bitwarden, BackedUp: true}
}

func mustCreate(t *testing.T, a *passkeytest.Authenticator, options any) json.RawMessage {
	t.Helper()
	cred, err := a.Create(options)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func mustGet(t *testing.T, a *passkeytest.Authenticator, options any) json.RawMessage {
	t.Helper()
	cred, err := a.Get(options)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestPasskeyConfig(t *testing.T) {
	origin, err := AndroidOrigin(testDebugFingerprint)
	if err != nil || origin != "android:apk-key-hash:dKdF-sJKv73wpzCDGHH-pt4kbGvS4_hiSSQnH8TTcEg" {
		t.Errorf("origin = %s, %v", origin, err)
	}
	if _, err = AndroidOrigin("AB:CD"); err == nil {
		t.Error("short fingerprint accepted")
	}

	s := openTest(t, t.TempDir(), configuration.ServerConfiguration{})
	if s.PasskeysEnabled() || s.RPID() != "" || s.AssetLinks() != nil {
		t.Error("passkeys on without configuration")
	}
	if _, _, err = s.BeginPasskeyLogin(""); !errors.Is(err, ErrPasskeysDisabled) {
		t.Errorf("begin without passkeys: %v", err)
	}

	// the RP ID defaults to the host of clientUrl
	s = openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: configuration.AuthConfiguration{ClientURL: "http://localhost:4174"}})
	if !s.PasskeysEnabled() || s.RPID() != "localhost" {
		t.Errorf("rp id = %q", s.RPID())
	}

	conf := passkeyConf()
	conf.WebAuthn.ServeAssetLinks = true
	s = openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: conf})
	var links []map[string]any
	if err = json.Unmarshal(s.AssetLinks(), &links); err != nil || len(links) != 1 {
		t.Fatal(string(s.AssetLinks()), err)
	}
	if !strings.Contains(string(s.AssetLinks()), "common.get_login_creds") || !strings.Contains(string(s.AssetLinks()), testDebugFingerprint) {
		t.Errorf("assetlinks = %s", s.AssetLinks())
	}

	for name, c := range map[string]configuration.AuthConfiguration{
		"assetlinks without apps": {ClientURL: "https://x.example.com", WebAuthn: configuration.WebAuthnConfiguration{ServeAssetLinks: true}},
		"bad fingerprint": {ClientURL: "https://x.example.com", WebAuthn: configuration.WebAuthnConfiguration{
			Android: []configuration.AndroidAppConfiguration{{PackageName: "a.b", SHA256CertFingerprints: []string{"nope"}}}}},
		"rp id without origins": {WebAuthn: configuration.WebAuthnConfiguration{RPID: "example.com"}},
	} {
		if _, err := Open(Options{StateDir: t.TempDir(), Server: configuration.ServerConfiguration{Auth: c}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPasskeySetupAndLogin(t *testing.T) {
	s := openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: passkeyConf()})
	c := newClock()
	s.now = c.now
	phone := webAuthenticator()

	// the first admin registers with a passkey only
	_, token, _ := s.CreateSetupLink()
	if _, _, err := s.BeginLinkPasskey(token, NewAccount{Username: "bad name"}); !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("invalid username: %v", err)
	}
	id, options, err := s.BeginLinkPasskey(token, NewAccount{Username: "admin", DisplayName: "The Admin"})
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := json.Marshal(options)
	for _, want := range []string{`"residentKey":"required"`, `"userVerification":"required"`, `"attestation":"none"`, `"id":"wiki.example.com"`, `"alg":-7`} {
		if !strings.Contains(string(opts), want) {
			t.Errorf("creation options miss %s: %s", want, opts)
		}
	}
	cred := mustCreate(t, phone, options)
	r, err := s.FinishLinkPasskey(id, cred, "", sp(SessionBearer))
	if err != nil || r.Session == nil || r.User.Role != RoleAdmin {
		t.Fatal(r, err)
	}
	if _, err = s.FinishLinkPasskey(id, cred, "", sp(SessionBearer)); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("ceremony used twice: %v", err)
	}
	admin := r.User
	d, _ := s.AccountDetails(admin.ID)
	if d.HasPassword || d.PasskeyCount != 1 {
		t.Errorf("details = %+v", d)
	}
	keys, _ := s.Passkeys(admin.ID)
	if len(keys) != 1 || keys[0].Name != "Bitwarden" || keys[0].Provider != "Bitwarden" || !keys[0].BackedUp ||
		strings.Join(keys[0].Transports, ",") != "internal,hybrid" || keys[0].LastUsedAt != nil {
		t.Errorf("passkeys = %+v", keys[0])
	}

	// discoverable login, no username
	id, options, err = s.BeginPasskeyLogin("")
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.FinishPasskeyLogin(id, mustGet(t, phone, options), sp(SessionCookie))
	if err != nil || r.User.ID != admin.ID || r.Session == nil {
		t.Fatal(r, err)
	}
	if keys, _ = s.Passkeys(admin.ID); keys[0].LastUsedAt == nil {
		t.Error("lastUsedAt not set")
	}

	// rejected: no user verification, another origin, an expired ceremony
	noUV := *phone
	noUV.SkipUV = true
	id, options, _ = s.BeginPasskeyLogin("")
	if _, err = s.FinishPasskeyLogin(id, mustGet(t, &noUV, options), sp("")); !errors.Is(err, ErrPasskeyVerificationFailed) {
		t.Errorf("without user verification: %v", err)
	}
	evil := *phone
	evil.Origin = "https://evil.example.org"
	id, options, _ = s.BeginPasskeyLogin("")
	if _, err = s.FinishPasskeyLogin(id, mustGet(t, &evil, options), sp("")); !errors.Is(err, ErrPasskeyVerificationFailed) {
		t.Errorf("foreign origin: %v", err)
	}
	id, options, _ = s.BeginPasskeyLogin("")
	c.advance(6 * time.Minute)
	if _, err = s.FinishPasskeyLogin(id, mustGet(t, phone, options), sp("")); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("expired ceremony: %v", err)
	}

	// the Android app signs with its own origin
	android := *phone
	android.Origin = "android:apk-key-hash:dKdF-sJKv73wpzCDGHH-pt4kbGvS4_hiSSQnH8TTcEg"
	id, options, _ = s.BeginPasskeyLogin("")
	if _, err = s.FinishPasskeyLogin(id, mustGet(t, &android, options), sp(SessionBearer)); err != nil {
		t.Errorf("android origin: %v", err)
	}

	// another passkey; the existing one is excluded
	id, options, err = s.BeginAddPasskey(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = phone.Create(options); err == nil {
		t.Error("existing passkey not excluded")
	}
	laptop := &passkeytest.Authenticator{RPID: "wiki.example.com", Origin: "https://edit.wiki.example.com"}
	pk, err := s.FinishAddPasskey(admin, id, mustCreate(t, laptop, options), "Laptop", ri())
	if err != nil || pk.Name != "Laptop" || pk.BackedUp || pk.Provider != "" {
		t.Fatal(pk, err)
	}
	if pk, err = s.RenamePasskey(admin, pk.ID, " Work laptop ", ri()); err != nil || pk.Name != "Work laptop" {
		t.Fatal(pk, err)
	}
	if _, err = s.RenamePasskey(admin, pk.ID, "", ri()); !errors.Is(err, ErrInvalidPasskeyName) {
		t.Errorf("empty name: %v", err)
	}
	if _, err = s.RenamePasskey(admin, "nope", "x", ri()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown passkey: %v", err)
	}

	// step-up with a passkey
	sess := r.Session
	c.advance(11 * time.Minute)
	id, options, err = s.BeginReauthPasskey(admin, sess)
	if err != nil {
		t.Fatal(err)
	}
	until, err := s.FinishReauthPasskey(admin, sess, id, mustGet(t, laptop, options), ri())
	if err != nil || !until.Equal(c.now().Add(ReauthWindow)) {
		t.Fatal(until, err)
	}

	// deleting: fine while another passkey exists, never the last login method
	keys, _ = s.Passkeys(admin.ID)
	if err = s.DeletePasskey(admin, keys[0].ID, ri()); err != nil {
		t.Fatal(err)
	}
	if err = s.DeletePasskey(admin, keys[1].ID, ri()); !errors.Is(err, ErrLastLoginMethod) {
		t.Errorf("last login method deleted: %v", err)
	}
	if countAudit(t, s, AuditPasskeyAdded) != 2 || countAudit(t, s, AuditPasskeyRemoved) != 1 || countAudit(t, s, AuditPasskeyRenamed) != 1 {
		t.Error("passkey audit events")
	}

	// disabled accounts cannot log in with a passkey
	_, _ = s.db.Exec("UPDATE accounts SET status = 'disabled' WHERE id = ?", admin.ID)
	s.invalidateCache()
	id, options, _ = s.BeginPasskeyLogin("")
	if _, err = s.FinishPasskeyLogin(id, mustGet(t, laptop, options), sp("")); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("disabled account: %v", err)
	}
}

func TestPasskeySecondFactorAndReset(t *testing.T) {
	s, c, admin := setupService(t, passkeyConf())
	phone := webAuthenticator()
	if err := s.RemovePassword(admin, nil, ri()); !errors.Is(err, ErrLastLoginMethod) {
		t.Errorf("remove the only password: %v", err)
	}
	id, options, _ := s.BeginAddPasskey(admin)
	if _, err := s.FinishAddPasskey(admin, id, mustCreate(t, phone, options), "", ri()); err != nil {
		t.Fatal(err)
	}
	if s.AuthenticateBasic("admin", "boss secret 2026!") != nil {
		t.Error("basic auth works for an account with a passkey")
	}

	r, err := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	if err != nil || r.Session != nil || strings.Join(r.Methods, ",") != "passkey" {
		t.Fatal(r, err)
	}
	if _, _, err = s.BeginPasskeyLogin("mdc_" + strings.Repeat("a", 43)); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("unknown challenge: %v", err)
	}
	id, options, err = s.BeginPasskeyLogin(r.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	if opts, _ := json.Marshal(options); !strings.Contains(string(opts), "allowCredentials") {
		t.Errorf("second factor options without allowCredentials: %s", opts)
	}
	done, err := s.FinishPasskeyLogin(id, mustGet(t, phone, options), sp(SessionBearer))
	if err != nil || done.Session == nil {
		t.Fatal(done, err)
	}
	if _, _, err = s.BeginPasskeyLogin(r.Challenge); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("challenge reused after the passkey: %v", err)
	}

	// another account's passkey does not complete this account's challenge
	_, invite, _ := s.CreateInvite(admin, RoleEditor, "", 0, ri())
	other := &passkeytest.Authenticator{RPID: "wiki.example.com", Origin: "https://edit.wiki.example.com"}
	id, options, _ = s.BeginLinkPasskey(invite, NewAccount{Username: "anna"})
	anna, err := s.FinishLinkPasskey(id, mustCreate(t, other, options), "Anna's phone", sp(""))
	if err != nil || anna.User.Username != "anna" || anna.User.Role != RoleEditor {
		t.Fatal(anna, err)
	}
	r, _ = s.LoginPassword("admin", "boss secret 2026!", sp(""))
	id, options, _ = s.BeginPasskeyLogin(r.Challenge)
	foreign, _ := other.Sign(other.Keys[0], passkeytest.Challenge(options))
	if _, err = s.FinishPasskeyLogin(id, foreign, sp("")); !errors.Is(err, ErrPasskeyVerificationFailed) {
		t.Errorf("foreign passkey as second factor: %v", err)
	}

	// require2FA: the last passkey of an account without TOTP stays
	_ = setSetting(s.db, settingRequire2FA, "true")
	keys, _ := s.Passkeys(admin.ID)
	if err = s.DeletePasskey(admin, keys[0].ID, ri()); !errors.Is(err, ErrSecondFactorRequired) {
		t.Errorf("last second factor deleted: %v", err)
	}
	_ = setSetting(s.db, settingRequire2FA, "false")

	// reset with a new passkey keeps the old one; resetSecondFactors removes it first
	_, token, _ := s.CreateResetLink(admin, admin.ID, 0, false, ri())
	id, options, _ = s.BeginLinkPasskey(token, NewAccount{})
	if _, err = phone.Create(options); err == nil {
		t.Error("existing passkey not excluded on reset")
	}
	laptop := &passkeytest.Authenticator{RPID: "wiki.example.com", Origin: "https://edit.wiki.example.com"}
	if _, err = s.FinishLinkPasskey(id, mustCreate(t, laptop, options), "Laptop", sp("")); err != nil {
		t.Fatal(err)
	}
	if n := s.passkeyCount(s.db, admin.ID); n != 2 {
		t.Errorf("%d passkeys after the reset", n)
	}
	_, token, _ = s.CreateResetLink(admin, admin.ID, 0, true, ri())
	if _, err = s.RedeemLink(token, NewAccount{Password: "violets are blue 7"}, sp("")); err != nil {
		t.Fatal(err)
	}
	if n := s.passkeyCount(s.db, admin.ID); n != 0 {
		t.Errorf("%d passkeys after resetSecondFactors", n)
	}
	c.advance(time.Minute)
}

// A step-up ceremony counts only for the account and the session that began it, with a passkey
// of that account, with user verification, and only once.
func TestPasskeyReauthBinding(t *testing.T) {
	s, c, admin := setupService(t, passkeyConf())
	phone := webAuthenticator()
	id, options, err := s.BeginAddPasskey(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishAddPasskey(admin, id, mustCreate(t, phone, options), "", ri()); err != nil {
		t.Fatal(err)
	}
	// with a passkey, a password login needs a second factor
	login := func() *Session {
		t.Helper()
		r, err := s.LoginPassword("admin", "boss secret 2026!", sp(SessionBearer))
		if err != nil {
			t.Fatal(err)
		}
		id, options, err := s.BeginPasskeyLogin(r.Challenge)
		if err != nil {
			t.Fatal(err)
		}
		done, err := s.FinishPasskeyLogin(id, mustGet(t, phone, options), sp(SessionBearer))
		if err != nil {
			t.Fatal(err)
		}
		return done.Session
	}
	sess, otherSess := login(), login()

	_, invite, _ := s.CreateInvite(admin, RoleEditor, "", 0, ri())
	other := &passkeytest.Authenticator{RPID: "wiki.example.com", Origin: "https://edit.wiki.example.com"}
	id, options, _ = s.BeginLinkPasskey(invite, NewAccount{Username: "anna"})
	anna, err := s.FinishLinkPasskey(id, mustCreate(t, other, options), "", sp(SessionBearer))
	if err != nil {
		t.Fatal(err)
	}
	c.advance(11 * time.Minute)
	recent := func() bool { return s.RecentlyAuthenticated(sess) }

	begin := func() (string, any) {
		t.Helper()
		id, options, err := s.BeginReauthPasskey(admin, sess)
		if err != nil {
			t.Fatal(err)
		}
		return id, options
	}
	id, options = begin()
	opts, _ := json.Marshal(options)
	for _, want := range []string{`"allowCredentials"`, `"userVerification":"required"`} {
		if !strings.Contains(string(opts), want) {
			t.Errorf("step-up options miss %s: %s", want, opts)
		}
	}

	// a passkey of another account, signed by a working authenticator
	foreign, _ := other.Sign(other.Keys[0], passkeytest.Challenge(options))
	if _, err = s.FinishReauthPasskey(admin, sess, id, foreign, ri()); !errors.Is(err, ErrPasskeyVerificationFailed) {
		t.Errorf("foreign passkey: %v", err)
	}
	// without user verification
	id, options = begin()
	phone.SkipUV = true
	unverified := mustGet(t, phone, options)
	phone.SkipUV = false
	if _, err = s.FinishReauthPasskey(admin, sess, id, unverified, ri()); !errors.Is(err, ErrPasskeyVerificationFailed) {
		t.Errorf("no user verification: %v", err)
	}
	// a challenge that belongs to another ceremony
	id, options = begin()
	_, options2 := begin()
	if _, err = s.FinishReauthPasskey(admin, sess, id, mustGet(t, phone, options2), ri()); !errors.Is(err, ErrPasskeyVerificationFailed) {
		t.Errorf("challenge of another ceremony: %v", err)
	}
	// finished by another session of the account, or by another account
	id, options = begin()
	if _, err = s.FinishReauthPasskey(admin, otherSess, id, mustGet(t, phone, options), ri()); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("another session: %v", err)
	}
	id, options = begin()
	if _, err = s.FinishReauthPasskey(anna.User, anna.Session, id, mustGet(t, phone, options), ri()); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("another account: %v", err)
	}
	if recent() || s.RecentlyAuthenticated(otherSess) || s.RecentlyAuthenticated(anna.Session) {
		t.Error("a failed step-up counts as recent authentication")
	}

	// the real one works, once, and only for this session
	id, options = begin()
	cred := mustGet(t, phone, options)
	if _, err = s.FinishReauthPasskey(admin, sess, id, cred, ri()); err != nil {
		t.Fatal(err)
	}
	if !recent() {
		t.Error("the step-up did not count")
	}
	if s.RecentlyAuthenticated(otherSess) {
		t.Error("the step-up counted for another session")
	}
	if _, err = s.FinishReauthPasskey(admin, sess, id, cred, ri()); !errors.Is(err, ErrChallengeExpired) {
		t.Errorf("ceremony used twice: %v", err)
	}
}
