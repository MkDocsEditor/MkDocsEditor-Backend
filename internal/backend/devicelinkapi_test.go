package backend

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth/passkeytest"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

func TestServerInfo(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.WikiURL = "https://wiki.example.com/"
		c.Server.BackendURL = "https://api.example.com"
	})
	r := e.app().do(http.MethodGet, "/server-info/", nil)
	expect(t, r, http.StatusOK, "")
	var info ServerInfoResponse
	r.json(t, &info)
	if info.Name != "wiki.example.com" || info.ProtocolVersion != ProtocolVersion || info.Version == "" ||
		info.BackendURL == nil || *info.BackendURL != "https://api.example.com" ||
		info.WikiURL == nil || *info.WikiURL != "https://wiki.example.com" ||
		info.WebClientURL == nil || *info.WebClientURL != testWebOrigin ||
		!info.SetupRequired || info.AuthDisabled || !info.Passkeys.Enabled || info.Passkeys.RPID != "localhost" ||
		strings.Join(info.LoginMethods, ",") != "password,passkey,device-link" || info.Require2FA {
		t.Errorf("server info = %s", r.body)
	}
	// nothing about accounts
	if strings.Contains(string(r.body), "markus") {
		t.Error("server info mentions an account")
	}

	e.setupAdmin()
	r = e.app().do(http.MethodGet, "/server-info/", nil)
	r.json(t, &info)
	if info.SetupRequired {
		t.Errorf("setupRequired after setup: %s", r.body)
	}

	// defaults: name from the client URL, backend URL from the request
	e = newAccountEnv(t, func(c *configuration.Configuration) { c.Server.InstanceName = "  " })
	r = e.app().do(http.MethodGet, "/server-info/", nil)
	r.json(t, &info)
	if info.Name != "localhost" || info.WikiURL != nil || info.BackendURL == nil || *info.BackendURL != e.srv.URL {
		t.Errorf("server info defaults = %s", r.body)
	}

	// without authentication
	e = newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.Auth.Disabled = true
		c.Server.InstanceName = "Dev Wiki"
	})
	r = e.app().do(http.MethodGet, "/server-info/", nil)
	r.json(t, &info)
	if info.Name != "Dev Wiki" || !info.AuthDisabled || len(info.LoginMethods) != 0 {
		t.Errorf("server info without auth = %s", r.body)
	}
}

// createDeviceLink creates a device link with a signed-in client
func createDeviceLink(t *testing.T, c *client, deviceName string) CreatedDeviceLinkResponse {
	t.Helper()
	r := c.do(http.MethodPost, "/account/device-links/", map[string]any{"deviceName": deviceName})
	expect(t, r, http.StatusOK, "")
	var created CreatedDeviceLinkResponse
	r.json(t, &created)
	return created
}

func redeemDeviceLink(c *client, token, username, deviceName string) *response {
	return c.do(http.MethodPost, "/auth/device-link/redeem/", map[string]any{"token": token, "username": username, "deviceName": deviceName})
}

func TestDeviceLinkFlow(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) { c.Server.BackendURL = "https://api.example.com/" })
	clk := e.clock()
	b := e.setupAdmin()

	created := createDeviceLink(t, b, "Pixel 8")
	l := created.DeviceLink
	if !strings.HasPrefix(created.Token, "mdd_") || len(created.Token) != 47 || l.Status != auth.DeviceLinkPending ||
		l.DeviceName == nil || *l.DeviceName != "Pixel 8" || !created.ExpiresAt.Equal(l.ExpiresAt) ||
		l.ExpiresAt.Sub(l.CreatedAt) != auth.DeviceLinkLifetime || l.SessionID != nil {
		t.Fatalf("created = %+v", created)
	}

	// the connect link carries the backend URL, the username and the token
	if created.ConnectURL == nil || !strings.HasPrefix(*created.ConnectURL, testWebOrigin+"/connect#") {
		t.Fatalf("connectUrl = %v", created.ConnectURL)
	}
	fragment := strings.TrimPrefix(*created.ConnectURL, testWebOrigin+"/connect#")
	data, err := base64.RawURLEncoding.DecodeString(fragment)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["v"] != float64(1) || payload["backendUrl"] != "https://api.example.com" || payload["username"] != "markus" ||
		payload["token"] != created.Token || payload["expiresAt"] == nil {
		t.Errorf("payload = %s", data)
	}

	// redeem on the phone: a bearer session by default
	phone := e.app()
	expect(t, redeemDeviceLink(phone, created.Token, "someone-else", "Pixel 8"), http.StatusBadRequest, CodeInvalidLink)
	r := redeemDeviceLink(phone, created.Token, "MARKUS", "Pixel 8")
	expect(t, r, http.StatusOK, "")
	var s SessionResponse
	r.json(t, &s)
	if s.Status != "ok" || s.Session.Type != auth.SessionBearer || s.Token == "" || s.User.Username != "markus" || s.Session.DeviceName != "Pixel 8" ||
		s.User.PasskeyCount != 0 {
		t.Fatalf("session = %s", r.body)
	}
	phone.bearer = s.Token
	expect(t, phone.do(http.MethodGet, "/auth/whoami/", nil), http.StatusOK, "")

	// used up
	expect(t, redeemDeviceLink(e.app(), created.Token, "", ""), http.StatusBadRequest, CodeInvalidLink)

	// the web sees who redeemed it
	r = b.do(http.MethodGet, "/account/device-links/"+l.ID+"/", nil)
	expect(t, r, http.StatusOK, "")
	var got auth.DeviceLink
	r.json(t, &got)
	if got.Status != auth.DeviceLinkRedeemed || got.RedeemedDeviceName == nil || *got.RedeemedDeviceName != "Pixel 8" ||
		got.SessionID == nil || *got.SessionID != s.Session.ID || got.RedeemedAt == nil {
		t.Errorf("device link = %s", r.body)
	}
	expect(t, b.do(http.MethodDelete, "/account/device-links/"+l.ID+"/", nil), http.StatusConflict, CodeConflict)

	// the new session is not step-up fresh, but may add a passkey for 10 minutes
	expect(t, phone.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "x", "scope": "read"}), http.StatusForbidden, CodeReauthRequired)
	expect(t, phone.do(http.MethodPost, "/account/device-links/", nil), http.StatusForbidden, CodeReauthRequired)
	ceremony, options := ceremonyOf(t, phone.do(http.MethodPost, "/account/passkeys/begin/", nil))
	key := &passkeytest.Authenticator{RPID: "localhost", Origin: testWebOrigin}
	cred, err := key.Create(options)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, phone.do(http.MethodPost, "/account/passkeys/finish/", map[string]any{"ceremonyId": ceremony, "credential": cred}), http.StatusOK, "")
	clk.advance(11 * time.Minute)
	expect(t, phone.do(http.MethodPost, "/account/passkeys/begin/", nil), http.StatusForbidden, CodeReauthRequired)

	// the login response reports the passkeys
	r = e.app().do(http.MethodPost, "/auth/login/", map[string]any{"username": "markus", "password": "a long admin secret", "sessionType": "bearer"})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(string(r.body), `"second-factor-required"`) {
		t.Errorf("login with a passkey = %s", r.body)
	}

	// creating needs a recent authentication
	expect(t, b.do(http.MethodPost, "/account/device-links/", nil), http.StatusForbidden, CodeReauthRequired)
	expect(t, b.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"}), http.StatusOK, "")
	created = createDeviceLink(t, b, "")
	if created.DeviceLink.DeviceName != nil {
		t.Errorf("deviceName = %v", *created.DeviceLink.DeviceName)
	}
	// the session response reports the passkey added above
	created2 := createDeviceLink(t, b, "")
	r = redeemDeviceLink(e.app(), created2.Token, "", "")
	expect(t, r, http.StatusOK, "")
	r.json(t, &s)
	if s.User.PasskeyCount != 1 {
		t.Errorf("passkeyCount = %d", s.User.PasskeyCount)
	}

	// app tokens cannot create device links
	r = b.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "script", "scope": "write"})
	expect(t, r, http.StatusOK, "")
	var tok struct {
		Token string `json:"token"`
	}
	r.json(t, &tok)
	script := &client{env: e, bearer: tok.Token}
	expect(t, script.do(http.MethodPost, "/account/device-links/", nil), http.StatusForbidden, CodeForbidden)

	// the list shows all three, newest first
	r = b.do(http.MethodGet, "/account/device-links/", nil)
	var list []auth.DeviceLink
	r.json(t, &list)
	if len(list) != 3 || list[0].ID != created2.DeviceLink.ID || list[1].ID != created.DeviceLink.ID || list[2].ID != l.ID {
		t.Errorf("list = %s", r.body)
	}
}

func TestDeviceLinkExpiryRevokeAndAccounts(t *testing.T) {
	e := newAccountEnv(t)
	clk := e.clock()
	admin := e.setupAdmin()
	anna := e.invite(admin, auth.RoleEditor, "anna", false)

	// expiry
	created := createDeviceLink(t, admin, "")
	clk.advance(auth.DeviceLinkLifetime)
	expect(t, redeemDeviceLink(e.app(), created.Token, "", ""), http.StatusBadRequest, CodeInvalidLink)
	r := admin.do(http.MethodGet, "/account/device-links/"+created.DeviceLink.ID+"/", nil)
	var got auth.DeviceLink
	r.json(t, &got)
	if got.Status != auth.DeviceLinkExpired {
		t.Errorf("status = %s", got.Status)
	}
	// revoking an expired link changes nothing
	expect(t, admin.do(http.MethodDelete, "/account/device-links/"+created.DeviceLink.ID+"/", nil), http.StatusNoContent, "")

	// revoke
	expect(t, admin.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"}), http.StatusOK, "")
	created = createDeviceLink(t, admin, "")
	expect(t, admin.do(http.MethodDelete, "/account/device-links/"+created.DeviceLink.ID+"/", nil), http.StatusNoContent, "")
	expect(t, redeemDeviceLink(e.app(), created.Token, "", ""), http.StatusBadRequest, CodeInvalidLink)

	// other accounts cannot see or revoke the link, and the token is bound to its account
	created = createDeviceLink(t, admin, "")
	expect(t, anna.do(http.MethodGet, "/account/device-links/"+created.DeviceLink.ID+"/", nil), http.StatusNotFound, CodeNotFound)
	expect(t, anna.do(http.MethodDelete, "/account/device-links/"+created.DeviceLink.ID+"/", nil), http.StatusNotFound, CodeNotFound)
	expect(t, redeemDeviceLink(e.app(), created.Token, "anna", ""), http.StatusBadRequest, CodeInvalidLink)
	r = redeemDeviceLink(e.app(), created.Token, "", "")
	expect(t, r, http.StatusOK, "")
	var s SessionResponse
	r.json(t, &s)
	if s.User.Username != "markus" {
		t.Errorf("redeemed as %s", s.User.Username)
	}

	// malformed and unknown tokens
	expect(t, redeemDeviceLink(e.app(), "", "", ""), http.StatusBadRequest, CodeBadRequest)
	expect(t, redeemDeviceLink(e.app(), "mdl_"+strings.Repeat("A", 43), "", ""), http.StatusBadRequest, CodeInvalidLink)
	expect(t, redeemDeviceLink(e.app(), "mdd_"+strings.Repeat("A", 43), "", ""), http.StatusBadRequest, CodeInvalidLink)
	// the redeem endpoint needs X-Requested-With like the other public POSTs
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/auth/device-link/redeem/", strings.NewReader(`{"token":"x"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("redeem without X-Requested-With: %d", resp.StatusCode)
	}

	// at most 5 pending links: the oldest is revoked
	var tokens []string
	for range 6 {
		tokens = append(tokens, createDeviceLink(t, admin, "").Token)
	}
	expect(t, redeemDeviceLink(e.app(), tokens[0], "", ""), http.StatusBadRequest, CodeInvalidLink)
	expect(t, redeemDeviceLink(e.app(), tokens[1], "", ""), http.StatusOK, "")

	// a password change revokes pending links
	pending := createDeviceLink(t, admin, "")
	expect(t, admin.do(http.MethodPut, "/account/password/", map[string]any{"currentPassword": "a long admin secret", "newPassword": "an even longer admin secret"}), http.StatusNoContent, "")
	expect(t, redeemDeviceLink(e.app(), pending.Token, "", ""), http.StatusBadRequest, CodeInvalidLink)

	// disabling an account revokes its pending links
	expect(t, anna.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "plum tree 42 blossoms"}), http.StatusOK, "")
	annaLink := createDeviceLink(t, anna, "")
	var users []map[string]any
	admin.do(http.MethodGet, "/admin/users/", nil).json(t, &users)
	annaID := ""
	for _, u := range users {
		if u["username"] == "anna" {
			annaID, _ = u["id"].(string)
		}
	}
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+annaID+"/", map[string]any{"status": "disabled"}), http.StatusOK, "")
	expect(t, redeemDeviceLink(e.app(), annaLink.Token, "", ""), http.StatusBadRequest, CodeInvalidLink)

	// audit events
	r = admin.do(http.MethodGet, "/admin/audit/?type=device-link.redeemed", nil)
	expect(t, r, http.StatusOK, "")
	if n := strings.Count(string(r.body), `"device-link.redeemed"`); n != 2 {
		t.Errorf("%d redeem events: %s", n, r.body)
	}
	for _, typ := range []string{"device-link.created", "device-link.revoked"} {
		if r = admin.do(http.MethodGet, "/admin/audit/?type="+typ, nil); !strings.Contains(string(r.body), `"`+typ+`"`) {
			t.Errorf("no %s event", typ)
		}
	}
}

func TestDeviceLinkRateLimit(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.Auth.RateLimits = configuration.RateLimitConfiguration{PerIPBurst: 3, PerIPRefill: time.Hour}
	})
	for range 3 {
		expect(t, redeemDeviceLink(e.app(), "mdd_"+strings.Repeat("B", 43), "", ""), http.StatusBadRequest, CodeInvalidLink)
	}
	r := redeemDeviceLink(e.app(), "mdd_"+strings.Repeat("B", 43), "", "")
	expect(t, r, http.StatusTooManyRequests, CodeRateLimited)
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
}
