package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/gorilla/websocket"
)

const testWebOrigin = "http://localhost:4174"

// accountEnv is a backend in setup mode behind an HTTP server
type accountEnv struct {
	*restTestEnv
	t   *testing.T
	srv *httptest.Server
}

func newAccountEnv(t *testing.T, configure ...func(c *configuration.Configuration)) *accountEnv {
	t.Helper()
	base := func(c *configuration.Configuration) {
		c.Server.CORS.AllowedOrigins = []string{testWebOrigin}
		c.Server.Auth.ClientURL = testWebOrigin
		c.Server.Auth.RateLimits.Disabled = true
	}
	e := newRestTestEnvWithAuth(t, true, append([]func(c *configuration.Configuration){base}, configure...)...)
	srv := httptest.NewServer(e.rs.echoRest)
	t.Cleanup(srv.Close)
	return &accountEnv{restTestEnv: e, t: t, srv: srv}
}

// client is a user agent: a browser (cookie jar, Origin, X-Requested-With) or a native app (bearer)
type client struct {
	env     *accountEnv
	cookie  *http.Cookie
	bearer  string
	browser bool
	origin  string
}

func (e *accountEnv) browser() *client { return &client{env: e, browser: true, origin: testWebOrigin} }
func (e *accountEnv) app() *client     { return &client{env: e} }

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r *response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (r *response) code(t *testing.T) string {
	t.Helper()
	var e ErrorResult
	r.json(t, &e)
	return e.Code
}

func (c *client) do(method, path string, body any) *response {
	c.env.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			c.env.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.env.srv.URL+path, reader)
	if err != nil {
		c.env.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "test-agent")
	if c.browser {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.cookie != nil {
			req.AddCookie(c.cookie)
		}
	} else if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	} else if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "MkDocsEditor-Android")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.env.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	for _, ck := range resp.Cookies() {
		if strings.HasSuffix(ck.Name, "mkdocseditor_session") {
			if ck.MaxAge < 0 || ck.Value == "" {
				c.cookie = nil
			} else {
				c.cookie = ck
			}
		}
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}
}

func expect(t *testing.T, r *response, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	if code != "" {
		if got := r.code(t); got != code {
			t.Fatalf("code = %q, want %q: %s", got, code, r.body)
		}
	}
}

// login logs a client in with a password
func (c *client) login(username, password string) *SessionResponse {
	c.env.t.Helper()
	sessionType := auth.SessionCookie
	if !c.browser {
		sessionType = auth.SessionBearer
	}
	c.bearer = ""
	r := c.do(http.MethodPost, "/auth/login/", map[string]any{"username": username, "password": password, "sessionType": sessionType})
	expect(c.env.t, r, http.StatusOK, "")
	var s SessionResponse
	r.json(c.env.t, &s)
	if s.Status != "ok" {
		c.env.t.Fatalf("login: %s", r.body)
	}
	if !c.browser {
		c.bearer = s.Token
	}
	return &s
}

// setupAdmin runs the setup flow and returns the admin's browser
func (e *accountEnv) setupAdmin() *client {
	e.t.Helper()
	_, token, err := e.rs.authenticator.CreateSetupLink()
	if err != nil {
		e.t.Fatal(err)
	}
	b := e.browser()
	r := b.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": token, "username": "markus", "displayName": "Markus", "password": "a long admin secret"})
	expect(e.t, r, http.StatusOK, "")
	return b
}

// invite creates an invite with the admin and redeems it with a new client
func (e *accountEnv) invite(admin *client, role, username string, browser bool) *client {
	e.t.Helper()
	r := admin.do(http.MethodPost, "/admin/invites/", map[string]any{"role": role})
	expect(e.t, r, http.StatusOK, "")
	var created CreatedLinkResponse
	r.json(e.t, &created)
	c := e.app()
	sessionType := auth.SessionBearer
	if browser {
		c = e.browser()
		sessionType = auth.SessionCookie
	}
	r = c.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": created.Token, "username": username,
		"password": "plum tree 42 blossoms", "sessionType": sessionType})
	expect(e.t, r, http.StatusOK, "")
	var s SessionResponse
	r.json(e.t, &s)
	c.bearer = s.Token
	return c
}

func TestAccountSetupFlow(t *testing.T) {
	e := newAccountEnv(t)
	b := e.browser()

	var status AuthStatusResponse
	r := b.do(http.MethodGet, "/auth/status/", nil)
	expect(t, r, http.StatusOK, "")
	r.json(t, &status)
	if !status.SetupRequired || status.AuthDisabled || status.PasswordPolicy.MinLength != 10 {
		t.Fatalf("status = %+v", status)
	}
	expect(t, b.do(http.MethodGet, "/section/", nil), http.StatusUnauthorized, CodeUnauthenticated)

	_, token, err := e.rs.authenticator.CreateSetupLink()
	if err != nil {
		t.Fatal(err)
	}
	r = b.do(http.MethodPost, "/auth/link/inspect/", map[string]any{"token": token})
	expect(t, r, http.StatusOK, "")
	var insp LinkInspectResponse
	r.json(t, &insp)
	if insp.Kind != auth.LinkSetup || insp.Role != auth.RoleAdmin {
		t.Errorf("inspect = %+v", insp)
	}
	expect(t, b.do(http.MethodPost, "/auth/link/inspect/", map[string]any{"token": "mdl_nope"}), http.StatusBadRequest, CodeInvalidLink)
	r = b.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": token, "username": "markus", "password": "short"})
	expect(t, r, http.StatusBadRequest, CodeWeakPassword)
	var weak ErrorResult
	r.json(t, &weak)
	if weak.Details["reason"] != auth.WeakTooShort {
		t.Errorf("details = %v", weak.Details)
	}
	expect(t, b.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": token, "username": "mar kus", "password": "a long admin secret"}),
		http.StatusBadRequest, CodeInvalidUsername)

	r = b.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": token, "username": "markus", "displayName": "Markus", "password": "a long admin secret"})
	expect(t, r, http.StatusOK, "")
	var sess SessionResponse
	r.json(t, &sess)
	if sess.Token != "" || sess.User.Role != auth.RoleAdmin || sess.Session.Type != auth.SessionCookie || b.cookie == nil {
		t.Fatalf("session response = %s", r.body)
	}
	if b.cookie.Name != cookieNameInsecure || !b.cookie.HttpOnly || b.cookie.Secure || b.cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v", b.cookie)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Error("session response is cacheable")
	}

	// the cookie authenticates
	r = b.do(http.MethodGet, "/auth/whoami/", nil)
	expect(t, r, http.StatusOK, "")
	var who WhoAmIResponse
	r.json(t, &who)
	if who.Username != "markus" || who.AuthMethod != AuthMethodSession || who.SessionID != sess.Session.ID || who.ID == "" {
		t.Errorf("whoami = %s", r.body)
	}
	expect(t, b.do(http.MethodGet, "/section/", nil), http.StatusOK, "")
	expect(t, b.do(http.MethodPost, "/document/", NewDocumentRequest{Parent: RootSectionID, Name: "by-markus"}), http.StatusOK, "")
	r = b.do(http.MethodGet, "/auth/status/", nil)
	r.json(t, &status)
	if status.SetupRequired {
		t.Error("setup still required")
	}
	expect(t, b.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": token, "username": "other", "password": "a long admin secret"}),
		http.StatusBadRequest, CodeInvalidLink)

	// logout ends the session and clears the cookie
	old := b.cookie
	expect(t, b.do(http.MethodPost, "/auth/logout/", nil), http.StatusNoContent, "")
	if b.cookie != nil {
		t.Error("cookie not cleared")
	}
	b.cookie = old
	expect(t, b.do(http.MethodGet, "/section/", nil), http.StatusUnauthorized, CodeUnauthenticated)
}

func TestAccountCSRF(t *testing.T) {
	e := newAccountEnv(t)
	b := e.setupAdmin()
	create := func(c *client) *response {
		return c.do(http.MethodPost, "/document/", NewDocumentRequest{Parent: RootSectionID, Name: "doc-" + time.Now().Format("150405.000000000")})
	}
	expect(t, create(b), http.StatusOK, "")

	// no X-Requested-With
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/document/", strings.NewReader(`{"parent":"root","name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testWebOrigin)
	req.AddCookie(b.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("without X-Requested-With: %d", resp.StatusCode)
	}

	evil := *b
	evil.origin = "https://evil.example.com"
	expect(t, create(&evil), http.StatusForbidden, CodeOriginNotAllowed)
	noOrigin := *b
	noOrigin.origin = ""
	expect(t, create(&noOrigin), http.StatusForbidden, CodeOriginNotAllowed)
	// the backend's own origin is allowed
	own := *b
	own.origin = e.srv.URL
	expect(t, create(&own), http.StatusOK, "")
	// GET needs no CSRF header
	expect(t, evil.do(http.MethodGet, "/section/", nil), http.StatusOK, "")
	// login CSRF: a foreign page cannot log a browser in
	anon := e.browser()
	anon.origin = "https://evil.example.com"
	expect(t, anon.do(http.MethodPost, "/auth/login/", map[string]any{"username": "markus", "password": "a long admin secret"}), http.StatusForbidden, CodeOriginNotAllowed)

	// the preflight allows PATCH and the custom header for the web origin only
	pre, _ := http.NewRequest(http.MethodOptions, e.srv.URL+"/account/", nil)
	pre.Header.Set("Origin", testWebOrigin)
	pre.Header.Set("Access-Control-Request-Method", "PATCH")
	pre.Header.Set("Access-Control-Request-Headers", "x-requested-with,content-type")
	resp, err = http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "PATCH") || resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("preflight = %v", resp.Header)
	}
}

func TestAccountBearerAndInvites(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()

	// invites are admin-only and need a session
	r := admin.do(http.MethodPost, "/admin/invites/", map[string]any{"role": "editor", "note": "for Anna", "expiresInHours": 48})
	expect(t, r, http.StatusOK, "")
	var created CreatedLinkResponse
	r.json(t, &created)
	if created.URL == nil || *created.URL != testWebOrigin+"/invite#"+created.Token || created.Invite.Note != "for Anna" {
		t.Fatalf("invite = %s", r.body)
	}
	expect(t, admin.do(http.MethodPost, "/admin/invites/", map[string]any{"role": "root"}), http.StatusBadRequest, CodeBadRequest)

	app := e.app()
	r = app.do(http.MethodPost, "/auth/link/inspect/", map[string]any{"token": created.Token})
	expect(t, r, http.StatusOK, "")
	r = app.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": created.Token, "username": "anna",
		"password": "violets are blue 7", "sessionType": "bearer", "deviceName": "Pixel 8"})
	expect(t, r, http.StatusOK, "")
	var sess SessionResponse
	r.json(t, &sess)
	if !strings.HasPrefix(sess.Token, auth.PrefixSession) || sess.Session.DeviceName != "Pixel 8" || sess.User.Role != auth.RoleEditor {
		t.Fatalf("session = %s", r.body)
	}
	app.bearer = sess.Token
	expect(t, app.do(http.MethodPost, "/document/", NewDocumentRequest{Parent: RootSectionID, Name: "by-anna"}), http.StatusOK, "")
	expect(t, app.do(http.MethodGet, "/admin/invites/", nil), http.StatusForbidden, CodeForbidden)

	// invalid bearer tokens are 401, even with a valid cookie
	bad := e.app()
	bad.bearer = "mds_" + strings.Repeat("x", 43)
	expect(t, bad.do(http.MethodGet, "/section/", nil), http.StatusUnauthorized, CodeUnauthenticated)

	r = admin.do(http.MethodGet, "/admin/invites/?status=used", nil)
	var links []*auth.Link
	r.json(t, &links)
	if len(links) != 1 || links[0].UsedBy.Username != "anna" {
		t.Errorf("used invites = %s", r.body)
	}
	r = admin.do(http.MethodPost, "/admin/invites/", map[string]any{"role": "reader"})
	r.json(t, &created)
	expect(t, admin.do(http.MethodDelete, "/admin/invites/"+created.Invite.ID+"/", nil), http.StatusNoContent, "")
	expect(t, app.do(http.MethodPost, "/auth/link/inspect/", map[string]any{"token": created.Token}), http.StatusBadRequest, CodeInvalidLink)

	// readers may read, not write
	kid := e.invite(admin, "reader", "kid", false)
	expect(t, kid.do(http.MethodGet, "/section/", nil), http.StatusOK, "")
	expect(t, kid.do(http.MethodPost, "/document/", NewDocumentRequest{Parent: RootSectionID, Name: "by-kid"}), http.StatusForbidden, CodeForbidden)
	expect(t, kid.do(http.MethodPost, "/auth/ws-ticket/", nil), http.StatusOK, "")
	expect(t, kid.do(http.MethodGet, "/account/sessions/", nil), http.StatusOK, "")

	// sessions: list, revoke one, revoke the others
	second := e.app()
	second.login("anna", "violets are blue 7")
	r = app.do(http.MethodGet, "/account/sessions/", nil)
	var sessions []*auth.Session
	r.json(t, &sessions)
	if len(sessions) != 2 || !sessions[0].Current || sessions[0].ID != sess.Session.ID || sessions[1].Current {
		t.Fatalf("sessions = %s", r.body)
	}
	expect(t, kid.do(http.MethodDelete, "/account/sessions/"+sessions[1].ID+"/", nil), http.StatusNotFound, CodeNotFound)
	expect(t, app.do(http.MethodDelete, "/account/sessions/"+sessions[1].ID+"/", nil), http.StatusNoContent, "")
	expect(t, second.do(http.MethodGet, "/section/", nil), http.StatusUnauthorized, CodeUnauthenticated)
	second.login("anna", "violets are blue 7")
	r = app.do(http.MethodPost, "/account/sessions/revoke-others/", nil)
	var revoked RevokedResponse
	r.json(t, &revoked)
	if revoked.Revoked != 1 {
		t.Errorf("revoked = %d", revoked.Revoked)
	}
	expect(t, second.do(http.MethodGet, "/section/", nil), http.StatusUnauthorized, CodeUnauthenticated)
	expect(t, app.do(http.MethodGet, "/section/", nil), http.StatusOK, "")

	// wrong passwords give a generic error
	expect(t, e.app().do(http.MethodPost, "/auth/login/", map[string]any{"username": "anna", "password": "wrong password"}), http.StatusUnauthorized, CodeInvalidCredentials)
	expect(t, e.app().do(http.MethodPost, "/auth/login/", map[string]any{"username": "nobody", "password": "wrong password"}), http.StatusUnauthorized, CodeInvalidCredentials)
	// account and admin endpoints need a session, not basic auth
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/account/sessions/", nil)
	req.SetBasicAuth("anna", "violets are blue 7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("basic auth on /account/: %d", resp.StatusCode)
	}
}

func TestAccountRateLimitResponse(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.Auth.RateLimits = configuration.RateLimitConfiguration{PerIPBurst: 3, PerIPRefill: time.Minute}
	})
	e.setupAdmin()
	app := e.app()
	for i := 0; i < 2; i++ {
		expect(t, app.do(http.MethodPost, "/auth/login/", map[string]any{"username": "markus", "password": "wrong password"}), http.StatusUnauthorized, CodeInvalidCredentials)
	}
	r := app.do(http.MethodPost, "/auth/login/", map[string]any{"username": "markus", "password": "a long admin secret"})
	expect(t, r, http.StatusTooManyRequests, CodeRateLimited)
	if r.header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q", r.header.Get("Retry-After"))
	}
}

func TestAccountLegacyBasicAuthOff(t *testing.T) {
	off := false
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.Auth.LegacyBasicAuth = &off
	})
	e.setupAdmin()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/section/", nil)
	req.SetBasicAuth("markus", "a long admin secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("basic auth: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestAccountTrustedProxy(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.TrustedProxies = []string{"127.0.0.1"}
		c.Server.CORS.AllowedOrigins = []string{"https://edit.example.com"}
	})
	_, token, _ := e.rs.authenticator.CreateSetupLink()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/auth/link/redeem/",
		strings.NewReader(`{"token":"`+token+`","username":"markus","password":"a long admin secret"}`))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", "https://edit.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieNameSecure || !cookies[0].Secure {
		t.Errorf("cookies = %+v", cookies)
	}
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS over HTTPS")
	}
	sessions, _ := e.rs.authenticator.Sessions(e.rs.authenticator.UserByUsername("markus").ID)
	if len(sessions) != 1 || sessions[0].IP != "10.0.0.1" {
		// 10.0.0.1 is not a trusted proxy, so it is the client
		t.Errorf("client ip = %+v", sessions[0])
	}
}

func TestAccountSecurityHeaders(t *testing.T) {
	e := newAccountEnv(t)
	r := e.browser().do(http.MethodGet, "/auth/status/", nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
		"Cross-Origin-Resource-Policy": "same-site",
		"Cache-Control":                "no-store",
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if r.header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS over plain HTTP")
	}
}

// websocket helpers

func (e *accountEnv) dialEvents(header http.Header, hello map[string]any) (*websocket.Conn, error) {
	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/events/ws/", header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if hello == nil {
		hello = map[string]any{}
	}
	hello["type"] = "hello"
	if err = ws.WriteJSON(hello); err != nil {
		return nil, err
	}
	return ws, nil
}

// readUntilClose returns the first message type and the close code
func readUntilClose(t *testing.T, ws *websocket.Conn, timeout time.Duration) (string, int) {
	t.Helper()
	first := ""
	_ = ws.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return first, ce.Code
			}
			return first, 0
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		if first == "" {
			first, _ = m["type"].(string)
		}
	}
}

func expectOnline(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m map[string]any
	if err := ws.ReadJSON(&m); err != nil || m["type"] != MsgOnline {
		t.Fatalf("first message %v, %v", m, err)
	}
}

func TestAccountWebsocketAuth(t *testing.T) {
	e := newAccountEnv(t)
	e.rs.sync.HeartbeatTimeout = 30 * time.Second
	admin := e.setupAdmin()
	app := e.invite(admin, "editor", "anna", false)

	// the cookie authenticates the upgrade from the web origin
	h := http.Header{"Origin": {testWebOrigin}, "Cookie": {admin.cookie.String()}}
	ws, err := e.dialEvents(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectOnline(t, ws)
	_ = ws.Close()

	// a foreign page cannot use the cookie
	h = http.Header{"Origin": {"https://evil.example.com"}, "Cookie": {admin.cookie.String()}}
	if ws, err = e.dialEvents(h, nil); err == nil {
		if _, code := readUntilClose(t, ws, 5*time.Second); code != CloseUnauthorized {
			t.Errorf("foreign origin: close code %d", code)
		}
	}

	// bearer token on the upgrade
	ws, err = e.dialEvents(http.Header{"Authorization": {"Bearer " + app.bearer}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectOnline(t, ws)

	// revoking the session closes the websocket with 4001
	r := app.do(http.MethodGet, "/account/sessions/", nil)
	var sessions []*auth.Session
	r.json(t, &sessions)
	expect(t, app.do(http.MethodDelete, "/account/sessions/"+sessions[0].ID+"/", nil), http.StatusNoContent, "")
	if _, code := readUntilClose(t, ws, 5*time.Second); code != CloseUnauthorized {
		t.Errorf("after revoke: close code %d", code)
	}

	// tickets keep working and are bound to the session
	r = admin.do(http.MethodPost, "/auth/ws-ticket/", nil)
	var ticket TicketResponse
	r.json(t, &ticket)
	ws, err = e.dialEvents(nil, map[string]any{"ticket": ticket.Ticket})
	if err != nil {
		t.Fatal(err)
	}
	expectOnline(t, ws)
	expect(t, admin.do(http.MethodPost, "/auth/logout/", nil), http.StatusNoContent, "")
	if _, code := readUntilClose(t, ws, 5*time.Second); code != CloseUnauthorized {
		t.Errorf("after logout: close code %d", code)
	}
}
