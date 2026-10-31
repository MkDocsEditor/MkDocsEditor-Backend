package backend

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth/passkeytest"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

func ceremonyOf(t *testing.T, r *response) (string, any) {
	t.Helper()
	expect(t, r, http.StatusOK, "")
	var c struct {
		CeremonyID string          `json:"ceremonyId"`
		PublicKey  json.RawMessage `json:"publicKey"`
	}
	r.json(t, &c)
	var options any
	_ = json.Unmarshal(c.PublicKey, &options)
	return c.CeremonyID, options
}

func TestAccountPasskeyFlow(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.Auth.WebAuthn.Android = []configuration.AndroidAppConfiguration{{PackageName: "de.markusressel.mkdocseditor.debug",
			SHA256CertFingerprints: []string{"74:A7:45:FA:C2:4A:BF:BD:F0:A7:30:83:18:71:FE:A6:DE:24:6C:6B:D2:E3:F8:62:49:24:27:1F:C4:D3:70:48"}}}
		c.Server.Auth.WebAuthn.ServeAssetLinks = true
	})
	clk := e.clock()
	key := &passkeytest.Authenticator{RPID: "localhost", Origin: testWebOrigin, AAGUID: passkeytest.Bitwarden, BackedUp: true}

	r := e.app().do(http.MethodGet, "/auth/status/", nil)
	var status AuthStatusResponse
	r.json(t, &status)
	if !status.Passkeys.Enabled || status.Passkeys.RPID != "localhost" || !status.SetupRequired {
		t.Errorf("status = %s", r.body)
	}

	// setup with a passkey in the browser
	_, token, err := e.rs.authenticator.CreateSetupLink()
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	expect(t, b.do(http.MethodPost, "/auth/link/passkey/begin/", map[string]any{"token": "mdl_nope", "username": "markus"}), http.StatusBadRequest, CodeInvalidLink)
	id, options := ceremonyOf(t, b.do(http.MethodPost, "/auth/link/passkey/begin/", map[string]any{"token": token, "username": "markus", "displayName": "Markus"}))
	cred, err := key.Create(options)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, b.do(http.MethodPost, "/auth/link/passkey/finish/", map[string]any{"ceremonyId": id}), http.StatusBadRequest, CodeBadRequest)
	r = b.do(http.MethodPost, "/auth/link/passkey/finish/", map[string]any{"ceremonyId": id, "credential": cred, "passkeyName": "Phone"})
	expect(t, r, http.StatusOK, "")
	if b.cookie == nil {
		t.Fatal("no session cookie")
	}
	r = b.do(http.MethodGet, "/account/", nil)
	var acc AccountResponse
	r.json(t, &acc)
	if acc.HasPassword || acc.PasskeyCount != 1 || acc.Role != auth.RoleAdmin {
		t.Errorf("account = %s", r.body)
	}

	// discoverable login with the app
	app := e.app()
	id, options = ceremonyOf(t, app.do(http.MethodPost, "/auth/passkey/login/begin/", map[string]any{}))
	cred, _ = key.Get(options)
	r = app.do(http.MethodPost, "/auth/passkey/login/finish/", map[string]any{"ceremonyId": id, "credential": cred, "sessionType": "bearer", "deviceName": "Pixel 8"})
	expect(t, r, http.StatusOK, "")
	var sess SessionResponse
	r.json(t, &sess)
	if sess.Token == "" || sess.User.Username != "markus" {
		t.Fatalf("session = %s", r.body)
	}
	app.bearer = sess.Token
	r = app.do(http.MethodPost, "/auth/passkey/login/finish/", map[string]any{"ceremonyId": id, "credential": cred})
	expect(t, r, http.StatusUnauthorized, CodeChallengeExpired)
	id, options = ceremonyOf(t, e.app().do(http.MethodPost, "/auth/passkey/login/begin/", nil))
	evil := *key
	evil.Origin = "http://localhost:9999"
	cred, _ = evil.Get(options)
	expect(t, e.app().do(http.MethodPost, "/auth/passkey/login/finish/", map[string]any{"ceremonyId": id, "credential": cred}), http.StatusUnauthorized, CodePasskeyVerificationFailed)

	// account passkeys: list, add (step-up), rename, delete
	r = app.do(http.MethodGet, "/account/passkeys/", nil)
	var keys []auth.Passkey
	r.json(t, &keys)
	if len(keys) != 1 || keys[0].Name != "Phone" || keys[0].Provider != "Bitwarden" || !keys[0].BackedUp || keys[0].LastUsedAt == nil {
		t.Fatalf("passkeys = %s", r.body)
	}
	clk.advance(11 * time.Minute)
	expect(t, app.do(http.MethodPost, "/account/passkeys/begin/", map[string]any{}), http.StatusForbidden, CodeReauthRequired)
	id, options = ceremonyOf(t, app.do(http.MethodPost, "/auth/reauth/passkey/begin/", map[string]any{}))
	cred, _ = key.Get(options)
	expect(t, app.do(http.MethodPost, "/auth/reauth/passkey/finish/", map[string]any{"ceremonyId": id, "credential": cred}), http.StatusOK, "")
	laptop := &passkeytest.Authenticator{RPID: "localhost", Origin: testWebOrigin}
	id, options = ceremonyOf(t, app.do(http.MethodPost, "/account/passkeys/begin/", map[string]any{}))
	cred, _ = laptop.Create(options)
	r = app.do(http.MethodPost, "/account/passkeys/finish/", map[string]any{"ceremonyId": id, "credential": cred, "name": "Laptop"})
	expect(t, r, http.StatusOK, "")
	var added auth.Passkey
	r.json(t, &added)
	if added.Name != "Laptop" || added.ID == "" {
		t.Errorf("added = %s", r.body)
	}
	r = app.do(http.MethodPatch, "/account/passkeys/"+added.ID+"/", map[string]any{"name": "Work laptop"})
	expect(t, r, http.StatusOK, "")
	expect(t, app.do(http.MethodPatch, "/account/passkeys/nope/", map[string]any{"name": "x"}), http.StatusNotFound, CodeNotFound)
	expect(t, app.do(http.MethodDelete, "/account/passkeys/"+added.ID+"/", nil), http.StatusNoContent, "")
	expect(t, app.do(http.MethodDelete, "/account/passkeys/"+keys[0].ID+"/", nil), http.StatusConflict, CodeLastLoginMethod)

	// the backend serves assetlinks.json
	resp, err := http.Get(e.srv.URL + "/.well-known/assetlinks.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(string(body), "de.markusressel.mkdocseditor.debug") || !strings.Contains(string(body), "get_login_creds") {
		t.Errorf("assetlinks: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func TestAccountPasskeysDisabled(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) { c.Server.Auth.ClientURL = "" })
	b := e.setupAdmin()
	expect(t, e.app().do(http.MethodPost, "/auth/passkey/login/begin/", map[string]any{}), http.StatusNotFound, CodePasskeysDisabled)
	expect(t, b.do(http.MethodPost, "/account/passkeys/begin/", map[string]any{}), http.StatusNotFound, CodePasskeysDisabled)
	r := b.do(http.MethodGet, "/account/passkeys/", nil)
	expect(t, r, http.StatusOK, "")
	if strings.TrimSpace(string(r.body)) != "[]" {
		t.Errorf("passkeys = %s", r.body)
	}
	resp, err := http.Get(e.srv.URL + "/.well-known/assetlinks.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("assetlinks without configuration: %d", resp.StatusCode)
	}
}
