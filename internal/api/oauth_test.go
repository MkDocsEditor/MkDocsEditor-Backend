package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// noRedirect is a client that returns redirects instead of following them
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// oauthDo sends a request without credentials. A url.Values body is a form, anything else
// JSON.
func (e *syncEnv) oauthDo(method, path string, body any) *tokenResponse {
	e.t.Helper()
	var reader *bytes.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case url.Values:
		reader = bytes.NewReader([]byte(b.Encode()))
		contentType = "application/x-www-form-urlencoded"
	default:
		data, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, e.http.URL+path, reader)
	req.Header.Set("Content-Type", contentType)
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := &tokenResponse{httpRecorder: httpRecorder{Code: resp.StatusCode}, header: resp.Header}
	_, _ = r.Body.ReadFrom(resp.Body)
	return r
}

// session sends a request with the session of a user
func (e *syncEnv) session(method, path, user string, body any) *tokenResponse {
	e.t.Helper()
	header := authHeader(e.t, e.authn, user, testPasswords[user])
	return e.doToken(method, path, strings.TrimPrefix(header, "Bearer "), body)
}

func pkce(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// oauthFlow runs the authorization of a client up to the code
type oauthFlow struct {
	e           *syncEnv
	clientID    string
	redirectURI string
	verifier    string
}

func (e *syncEnv) registerClient(redirectURIs ...string) string {
	e.t.Helper()
	r := e.oauthDo(http.MethodPost, APIPrefix+"/oauth/register", map[string]any{"client_name": "Claude Code", "redirect_uris": redirectURIs})
	if r.Code != http.StatusCreated {
		e.t.Fatalf("register = %d %s", r.Code, r.Body.String())
	}
	var c OAuthClientResponse
	r.json(e.t, &c)
	if c.ClientID == "" || c.TokenEndpointAuthMethod != "none" {
		e.t.Fatalf("register: %s", r.Body.String())
	}
	return c.ClientID
}

// authorize starts a request and returns its id (from the redirect to the consent page)
func (f *oauthFlow) authorize(extra url.Values) string {
	f.e.t.Helper()
	q := url.Values{"response_type": {"code"}, "client_id": {f.clientID}, "redirect_uri": {f.redirectURI},
		"code_challenge": {pkce(f.verifier)}, "code_challenge_method": {"S256"}, "state": {"xyz"}}
	for k, v := range extra {
		q[k] = v
	}
	r := f.e.oauthDo(http.MethodGet, APIPrefix+"/oauth/authorize?"+q.Encode(), nil)
	if r.Code != http.StatusFound {
		f.e.t.Fatalf("authorize = %d %s", r.Code, r.Body.String())
	}
	loc, err := url.Parse(r.header.Get("Location"))
	if err != nil || loc.Path != oauthConsentPath || loc.Query().Get("request") == "" {
		f.e.t.Fatalf("authorize redirects to %q", r.header.Get("Location"))
	}
	return loc.Query().Get("request")
}

// approve approves a request as a user and returns the code
func (f *oauthFlow) approve(user, id string, body any) string {
	f.e.t.Helper()
	r := f.e.session(http.MethodPost, "/oauth/requests/"+id+"/approve/", user, body)
	if r.Code != http.StatusOK {
		f.e.t.Fatalf("approve = %d %s", r.Code, r.Body.String())
	}
	var d OAuthDecisionResponse
	r.json(f.e.t, &d)
	loc, err := url.Parse(d.RedirectURL)
	if err != nil || !strings.HasPrefix(d.RedirectURL, f.redirectURI) || loc.Query().Get("state") != "xyz" || loc.Query().Get("code") == "" {
		f.e.t.Fatalf("approve redirects to %q", d.RedirectURL)
	}
	return loc.Query().Get("code")
}

func (f *oauthFlow) exchange(code, verifier string) *tokenResponse {
	f.e.t.Helper()
	return f.e.oauthDo(http.MethodPost, APIPrefix+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {f.redirectURI}, "client_id": {f.clientID}, "code_verifier": {verifier}})
}

func (f *oauthFlow) refresh(refreshToken string) *tokenResponse {
	f.e.t.Helper()
	return f.e.oauthDo(http.MethodPost, APIPrefix+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {f.clientID}})
}

func tokensOf(t *testing.T, r *tokenResponse) OAuthTokenResponse {
	t.Helper()
	if r.Code != http.StatusOK {
		t.Fatalf("token = %d %s", r.Code, r.Body.String())
	}
	var tr OAuthTokenResponse
	r.json(t, &tr)
	if tr.TokenType != "Bearer" || tr.AccessToken == "" || tr.RefreshToken == "" || tr.ExpiresIn != 3600 {
		t.Fatalf("token: %s", r.Body.String())
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("token response may be cached")
	}
	return tr
}

func oauthErrorOf(t *testing.T, r *tokenResponse) string {
	t.Helper()
	var e oauthErrorResponse
	r.json(t, &e)
	return e.Error
}

// What an MCP client finds without credentials: a 401 that points to the metadata, and the
// metadata at the root and below /api
func TestOAuthDiscovery(t *testing.T) {
	e := newSyncEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := e.oauthDo(method, APIPrefix+"/mcp", map[string]any{})
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("%s /mcp = %d", method, r.Code)
		}
		want := `Bearer resource_metadata="` + e.http.URL + `/api/.well-known/oauth-protected-resource/mcp"`
		if h := r.header.Get("WWW-Authenticate"); !strings.HasPrefix(h, want) {
			t.Errorf("%s /mcp WWW-Authenticate = %q", method, h)
		}
	}
	// a wrong token is invalid_token, so the client refreshes or starts over
	r := e.doToken(http.MethodPost, "/mcp", auth.PrefixOAuthAccess+strings.Repeat("A", 43), map[string]any{})
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("bad token: %d %q", r.Code, r.header.Get("WWW-Authenticate"))
	}

	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/api/mcp", "/api/.well-known/oauth-protected-resource/mcp"} {
		r := e.oauthDo(http.MethodGet, p, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p, r.Code, r.Body.String())
		}
		var m ProtectedResourceMetadata
		r.json(t, &m)
		if m.Resource != e.http.URL+"/api/mcp" || len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != e.http.URL+"/api" {
			t.Errorf("GET %s: %s", p, r.Body.String())
		}
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/api",
		"/.well-known/openid-configuration/api", "/api/.well-known/openid-configuration", "/api/.well-known/oauth-authorization-server"} {
		r := e.oauthDo(http.MethodGet, p, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p, r.Code, r.Body.String())
		}
		var m AuthServerMetadata
		r.json(t, &m)
		if m.Issuer != e.http.URL+"/api" || m.TokenEndpoint != e.http.URL+"/api/oauth/token" || m.RegistrationEndpoint != e.http.URL+"/api/oauth/register" ||
			len(m.CodeChallengeMethodsSupported) != 1 || m.CodeChallengeMethodsSupported[0] != "S256" {
			t.Errorf("GET %s: %s", p, r.Body.String())
		}
	}
	// other root paths stay outside the API
	if r := e.oauthDo(http.MethodGet, "/.well-known/assetlinks.json", nil); r.Code != http.StatusNotFound {
		t.Errorf("root assetlinks = %d", r.Code)
	}
}

// The whole flow of an MCP client: register, authorize, approve, token, MCP, refresh, revoke
func TestOAuthFlow(t *testing.T) {
	e := newSyncEnv(t)
	f := &oauthFlow{e: e, clientID: e.registerClient("http://127.0.0.1/callback"), redirectURI: "http://127.0.0.1:43123/callback",
		verifier: strings.Repeat("v", 50)}
	id := f.authorize(url.Values{"resource": {e.http.URL + "/api/mcp"}})

	// the consent page reads the request with a session; app tokens may not decide
	r := e.session(http.MethodGet, "/oauth/requests/"+id+"/", "bob", nil)
	if r.Code != http.StatusOK {
		t.Fatalf("GET request = %d %s", r.Code, r.Body.String())
	}
	var view OAuthRequestView
	r.json(t, &view)
	if view.ClientName != "Claude Code" || view.Scope != auth.ScopeWrite || view.RedirectURI != f.redirectURI || len(view.AllowedScopes) != 2 {
		t.Errorf("request: %s", r.Body.String())
	}
	expectCode(t, e.doToken(http.MethodPost, "/oauth/requests/"+id+"/approve/", e.appToken("bob", "script", auth.ScopeWrite), nil), http.StatusForbidden, CodeForbidden)

	code := f.approve("bob", id, nil)
	// the request is decided
	expectCode(t, e.session(http.MethodGet, "/oauth/requests/"+id+"/", "bob", nil), http.StatusNotFound, CodeNotFound)

	tr := tokensOf(t, f.exchange(code, f.verifier))
	if tr.Scope != "read write" {
		t.Errorf("scope = %q", tr.Scope)
	}

	// the access token works for MCP, as an agent of bob with the client's name
	s := e.mcpSession(tr.AccessToken)
	if names := toolNames(t, s); len(names) != 9 {
		t.Errorf("tools = %v", names)
	}
	docID := e.docID("guide/setup.md")
	if text, failed := callTool(t, s, "edit_document", map[string]any{"document": "guide/setup.md", "edits": []map[string]string{{"old": "Some text.", "new": "OAuth text."}}}); failed {
		t.Fatalf("edit: %s", text)
	}
	// and only for MCP
	expectCode(t, e.doToken(http.MethodGet, "/document/"+docID+"/content/", tr.AccessToken, nil), http.StatusForbidden, CodeForbidden)

	// the grant is an app token of bob, named after the client
	r = e.session(http.MethodGet, "/account/tokens/", "bob", nil)
	var tokens []auth.AppToken
	r.json(t, &tokens)
	var grant *auth.AppToken
	for i := range tokens {
		if tokens[i].OAuthClientID == f.clientID {
			grant = &tokens[i]
		}
	}
	if grant == nil || grant.Name != "Claude Code" || grant.Scope != auth.ScopeWrite || grant.Resource != e.http.URL+"/api/mcp" || grant.ExpiresAt == nil {
		t.Fatalf("grant not listed: %s", r.Body.String())
	}

	// a code works once; the second use revokes the grant
	if r := f.exchange(code, f.verifier); r.Code != http.StatusBadRequest || oauthErrorOf(t, r) != auth.OAuthInvalidGrant {
		t.Fatalf("second exchange = %d %s", r.Code, r.Body.String())
	}
	if r := e.doToken(http.MethodPost, "/mcp", tr.AccessToken, map[string]any{}); r.Code != http.StatusUnauthorized {
		t.Fatalf("token after code reuse = %d", r.Code)
	}

	// a new grant: refresh rotates, the old access token ends
	id = f.authorize(nil)
	tr = tokensOf(t, f.exchange(f.approve("bob", id, map[string]string{"scope": auth.ScopeRead}), f.verifier))
	if tr.Scope != auth.ScopeRead {
		t.Errorf("lowered scope = %q", tr.Scope)
	}
	if names := toolNames(t, e.mcpSession(tr.AccessToken)); len(names) != 5 {
		t.Errorf("read tools = %v", names)
	}
	tr2 := tokensOf(t, f.refresh(tr.RefreshToken))
	if r := e.doToken(http.MethodPost, "/mcp", tr.AccessToken, map[string]any{}); r.Code != http.StatusUnauthorized {
		t.Errorf("old access token after refresh = %d", r.Code)
	}
	e.mcpSession(tr2.AccessToken)
	// another client may not use the refresh token
	other := &oauthFlow{e: e, clientID: e.registerClient("https://example.com/cb"), redirectURI: "https://example.com/cb"}
	if r := other.refresh(tr2.RefreshToken); oauthErrorOf(t, r) != auth.OAuthInvalidGrant {
		t.Errorf("refresh of another client: %s", r.Body.String())
	}
	// using a refresh token twice ends the grant
	if r := f.refresh(tr.RefreshToken); oauthErrorOf(t, r) != auth.OAuthInvalidGrant {
		t.Fatalf("refresh reuse: %s", r.Body.String())
	}
	if r := e.doToken(http.MethodPost, "/mcp", tr2.AccessToken, map[string]any{}); r.Code != http.StatusUnauthorized {
		t.Errorf("token after refresh reuse = %d", r.Code)
	}

	// revoking the app token ends the grant
	tr = tokensOf(t, f.exchange(f.approve("bob", f.authorize(nil), nil), f.verifier))
	r = e.session(http.MethodGet, "/account/tokens/", "bob", nil)
	r.json(t, &tokens)
	for _, tok := range tokens {
		if tok.OAuthClientID != "" {
			if r := e.session(http.MethodDelete, "/account/tokens/"+tok.ID+"/", "bob", nil); r.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", r.Code, r.Body.String())
			}
		}
	}
	if r := e.doToken(http.MethodPost, "/mcp", tr.AccessToken, map[string]any{}); r.Code != http.StatusUnauthorized {
		t.Errorf("token after revoke = %d", r.Code)
	}
	if r := f.refresh(tr.RefreshToken); oauthErrorOf(t, r) != auth.OAuthInvalidGrant {
		t.Errorf("refresh after revoke: %s", r.Body.String())
	}

	// RFC 7009 revocation by the client
	tr = tokensOf(t, f.exchange(f.approve("bob", f.authorize(nil), nil), f.verifier))
	if r := e.oauthDo(http.MethodPost, APIPrefix+"/oauth/revoke", url.Values{"token": {tr.RefreshToken}}); r.Code != http.StatusOK {
		t.Fatalf("revoke = %d", r.Code)
	}
	if r := e.doToken(http.MethodPost, "/mcp", tr.AccessToken, map[string]any{}); r.Code != http.StatusUnauthorized {
		t.Errorf("token after client revoke = %d", r.Code)
	}

	// readers get read, whatever the client asks for
	tr = tokensOf(t, f.exchange(f.approve("carol", f.authorize(url.Values{"scope": {"read write"}}), nil), f.verifier))
	if tr.Scope != auth.ScopeRead {
		t.Errorf("reader scope = %q", tr.Scope)
	}
}

func TestOAuthErrors(t *testing.T) {
	e := newSyncEnv(t)
	clientID := e.registerClient("https://client.example/cb", "http://localhost/cb")
	f := &oauthFlow{e: e, clientID: clientID, redirectURI: "https://client.example/cb", verifier: strings.Repeat("w", 43)}

	// registration rejects redirect URIs a browser could be abused with
	for _, uri := range []string{"javascript:alert(1)", "http://evil.example/cb", "https://client.example/cb#x", "data:text/html,x", "", "https://user:pw@client.example/cb"} {
		r := e.oauthDo(http.MethodPost, APIPrefix+"/oauth/register", map[string]any{"redirect_uris": []string{uri}})
		if r.Code != http.StatusBadRequest || oauthErrorOf(t, r) != auth.OAuthInvalidRedirectURI {
			t.Errorf("register %q = %d %s", uri, r.Code, r.Body.String())
		}
	}
	if r := e.oauthDo(http.MethodPost, APIPrefix+"/oauth/register", map[string]any{"redirect_uris": []string{"cursor://anysphere.cursor-mcp/oauth/callback"}}); r.Code != http.StatusCreated {
		t.Errorf("private-use scheme = %d %s", r.Code, r.Body.String())
	}

	authorize := func(q url.Values) *tokenResponse {
		base := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {f.redirectURI},
			"code_challenge": {pkce(f.verifier)}, "code_challenge_method": {"S256"}, "state": {"s"}}
		for k, v := range q {
			base[k] = v
		}
		return e.oauthDo(http.MethodGet, APIPrefix+"/oauth/authorize?"+base.Encode(), nil)
	}
	// no redirect to an unknown client or an unregistered redirect URI
	for _, q := range []url.Values{{"client_id": {"nope"}}, {"redirect_uri": {"https://client.example/other"}}, {"redirect_uri": {"https://client.example:8443/cb"}}} {
		if r := authorize(q); r.Code != http.StatusBadRequest || r.header.Get("Location") != "" {
			t.Errorf("authorize %v = %d %q", q, r.Code, r.header.Get("Location"))
		}
	}
	// the port of loopback redirect URIs is free, the path is not
	if r := authorize(url.Values{"redirect_uri": {"http://localhost:9999/cb"}}); r.Code != http.StatusFound || !strings.Contains(r.header.Get("Location"), oauthConsentPath) {
		t.Errorf("loopback port = %d %q", r.Code, r.header.Get("Location"))
	}
	if r := authorize(url.Values{"redirect_uri": {"http://localhost:9999/other"}}); r.Code != http.StatusBadRequest {
		t.Errorf("loopback path = %d", r.Code)
	}
	// errors the client can be told about go to its redirect URI
	for q, want := range map[string]url.Values{
		auth.OAuthUnsupportedResponseType: {"response_type": {"token"}},
		auth.OAuthInvalidRequest:          {"code_challenge_method": {"plain"}},
		auth.OAuthInvalidTarget:           {"resource": {"https://other.example/mcp"}},
		auth.OAuthInvalidScope:            {"scope": {"admin"}},
	} {
		r := authorize(want)
		loc, _ := url.Parse(r.header.Get("Location"))
		if r.Code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), f.redirectURI) || loc.Query().Get("error") != q || loc.Query().Get("state") != "s" {
			t.Errorf("authorize %v = %d %q, want error %s", want, r.Code, r.header.Get("Location"), q)
		}
	}

	// deny
	id := f.authorize(nil)
	r := e.session(http.MethodPost, "/oauth/requests/"+id+"/deny/", "bob", nil)
	var d OAuthDecisionResponse
	r.json(t, &d)
	if loc, _ := url.Parse(d.RedirectURL); r.Code != http.StatusOK || loc.Query().Get("error") != auth.OAuthAccessDenied {
		t.Errorf("deny = %d %s", r.Code, r.Body.String())
	}
	expectCode(t, e.session(http.MethodPost, "/oauth/requests/"+id+"/approve/", "bob", nil), http.StatusNotFound, CodeNotFound)

	// a wrong verifier uses the code up
	code := f.approve("bob", f.authorize(nil), nil)
	if r := f.exchange(code, strings.Repeat("x", 43)); oauthErrorOf(t, r) != auth.OAuthInvalidGrant {
		t.Errorf("wrong verifier: %s", r.Body.String())
	}
	if r := f.exchange(code, f.verifier); oauthErrorOf(t, r) != auth.OAuthInvalidGrant {
		t.Errorf("code after a wrong verifier: %s", r.Body.String())
	}
	// unknown grant types and missing clients
	if r := e.oauthDo(http.MethodPost, APIPrefix+"/oauth/token", url.Values{"grant_type": {"password"}, "client_id": {clientID}}); oauthErrorOf(t, r) != auth.OAuthUnsupportedGrantType {
		t.Errorf("password grant: %s", r.Body.String())
	}
	if r := e.oauthDo(http.MethodPost, APIPrefix+"/oauth/token", url.Values{"grant_type": {"authorization_code"}}); r.Code != http.StatusUnauthorized || oauthErrorOf(t, r) != auth.OAuthInvalidClient {
		t.Errorf("no client: %d %s", r.Code, r.Body.String())
	}
	// the token endpoint ignores a session cookie of a reader and needs no CSRF header
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+APIPrefix+"/oauth/token", strings.NewReader("grant_type=refresh_token&client_id="+clientID+"&refresh_token=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", authHeader(t, e.authn, "carol", testPasswords["carol"]))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("token with a reader's session = %d", resp.StatusCode)
	}
}

// The OAuth client of the MCP Go SDK connects on its own: discovery from the 401, metadata,
// registration, PKCE, token exchange. The test plays the browser and the web client.
func TestOAuthSDKClient(t *testing.T) {
	e := newSyncEnv(t)
	redirect := "http://127.0.0.1:38111/callback"
	handler, err := mcpauth.NewAuthorizationCodeHandler(&mcpauth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &mcpauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "SDK client", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethod: "none",
		}},
		RedirectURL: redirect,
		AuthorizationCodeFetcher: func(_ context.Context, args *mcpauth.AuthorizationArgs) (*mcpauth.AuthorizationResult, error) {
			resp, err := noRedirect.Get(args.URL)
			if err != nil {
				return nil, err
			}
			_ = resp.Body.Close()
			consent, err := url.Parse(resp.Header.Get("Location"))
			if err != nil || consent.Path != oauthConsentPath {
				return nil, fmt.Errorf("authorize answered %d %q", resp.StatusCode, resp.Header.Get("Location"))
			}
			r := e.session(http.MethodPost, "/oauth/requests/"+consent.Query().Get("request")+"/approve/", "bob", nil)
			var d OAuthDecisionResponse
			if err := json.Unmarshal(r.Body.Bytes(), &d); err != nil {
				return nil, err
			}
			back, err := url.Parse(d.RedirectURL)
			if err != nil {
				return nil, err
			}
			return &mcpauth.AuthorizationResult{Code: back.Query().Get("code"), State: back.Query().Get("state")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "sdk", Version: "1"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.http.URL + APIPrefix + "/mcp", OAuthHandler: handler, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if names := toolNames(t, s); len(names) != 9 {
		t.Errorf("tools = %v", names)
	}
	tokens, err := e.authn.AppTokens(e.authn.UserByUsername("bob").ID)
	if err != nil || len(tokens) != 1 || tokens[0].Name != "SDK client" || tokens[0].OAuthClientID == "" {
		t.Errorf("grants = %+v %v", tokens, err)
	}
}

// Name and end chosen on the consent page; renaming afterwards; the end never changes
func TestOAuthGrantNameAndEnd(t *testing.T) {
	e := newSyncEnv(t)
	f := &oauthFlow{e: e, clientID: e.registerClient("http://127.0.0.1/callback"), redirectURI: "http://127.0.0.1/callback", verifier: strings.Repeat("v", 50)}

	id := f.authorize(nil)
	r := e.session(http.MethodGet, "/oauth/requests/"+id+"/", "bob", nil)
	var view OAuthRequestView
	r.json(t, &view)
	if view.Defaults.Name != "Claude Code" || view.Defaults.ExpiresAt != nil || view.Defaults.MaxExpiresAt.Before(time.Now().Add(3649*24*time.Hour)) {
		t.Errorf("defaults: %s", r.Body.String())
	}
	// bad ends and names are refused and leave the request pending
	for _, body := range []map[string]any{
		{"expiresAt": time.Now().Add(-time.Hour).Format(time.RFC3339)},
		{"expiresAt": time.Now().Add(3651 * 24 * time.Hour).Format(time.RFC3339)},
		{"expiresAt": "2030-01-01"},
		{"name": strings.Repeat("n", 65)},
	} {
		expectCode(t, e.session(http.MethodPost, "/oauth/requests/"+id+"/approve/", "bob", body), http.StatusBadRequest, CodeBadRequest)
	}
	end := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	tokensOf(t, f.exchange(f.approve("bob", id, map[string]any{"name": "Laptop agent", "expiresAt": end.Format(time.RFC3339)}), f.verifier))

	var tokens []auth.AppToken
	e.session(http.MethodGet, "/account/tokens/", "bob", nil).json(t, &tokens)
	if len(tokens) != 1 || tokens[0].Name != "Laptop agent" || tokens[0].SlidingExpiry || tokens[0].ExpiresAt == nil || !tokens[0].ExpiresAt.Equal(end) {
		t.Fatalf("grant: %+v", tokens)
	}
	grantID := tokens[0].ID

	// rename: sessions of the owner only; the end cannot change
	r = e.session(http.MethodPatch, "/account/tokens/"+grantID+"/", "bob", map[string]any{"name": "Desk agent"})
	var renamed auth.AppToken
	r.json(t, &renamed)
	if r.Code != http.StatusOK || renamed.Name != "Desk agent" || !renamed.ExpiresAt.Equal(end) {
		t.Errorf("rename = %d %s", r.Code, r.Body.String())
	}
	for _, body := range []map[string]any{{"expiresAt": nil}, {"name": "x", "expiresAt": end.Add(time.Hour).Format(time.RFC3339)}, {}, {"name": ""}} {
		expectCode(t, e.session(http.MethodPatch, "/account/tokens/"+grantID+"/", "bob", body), http.StatusBadRequest, CodeBadRequest)
	}
	expectCode(t, e.session(http.MethodPatch, "/account/tokens/"+grantID+"/", "alice", map[string]any{"name": "mine"}), http.StatusNotFound, CodeNotFound)
	expectCode(t, e.doToken(http.MethodPatch, "/account/tokens/"+grantID+"/", e.appToken("bob", "script", auth.ScopeWrite), map[string]any{"name": "x"}), http.StatusForbidden, CodeForbidden)

	// app tokens can be created with an absolute end
	r = e.session(http.MethodPost, "/account/tokens/", "bob", map[string]any{"name": "ci", "scope": "read", "expiresAt": end.Format(time.RFC3339)})
	var created CreatedAppTokenResponse
	r.json(t, &created)
	if r.Code != http.StatusOK || created.AppToken.ExpiresAt == nil || !created.AppToken.ExpiresAt.Equal(end) || created.AppToken.SlidingExpiry {
		t.Errorf("create with expiresAt = %d %s", r.Code, r.Body.String())
	}
	expectCode(t, e.session(http.MethodPost, "/account/tokens/", "bob", map[string]any{"name": "ci", "scope": "read", "expiresAt": end.Format(time.RFC3339), "expiresInDays": 3}),
		http.StatusBadRequest, CodeBadRequest)
}
