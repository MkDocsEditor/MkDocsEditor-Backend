package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// OAuth 2.1 authorization server for MCP clients, see docs/oauth.md. Clients register
// themselves (RFC 7591), the user approves a request in the web client, and the client trades
// the code (with PKCE) for an access and a refresh token. A grant is an app token with the
// name of the client: it is listed and revoked like any other app token. Pending requests and
// codes live in memory only; a restart makes the client start over.

// Token prefixes of OAuth
//
//nolint:gosec // prefixes, not credentials
const (
	PrefixOAuthAccess  = "mdo_"
	PrefixOAuthRefresh = "mdr_"
	PrefixOAuthCode    = "mdk_"
)

// Lifetimes of OAuth
const (
	OAuthAccessTokenLifetime  = time.Hour
	OAuthRefreshTokenLifetime = 30 * 24 * time.Hour
	OAuthCodeLifetime         = time.Minute
	OAuthRequestLifetime      = 10 * time.Minute
	// unusedOAuthClientLifetime is how long a client that never got a token is kept
	unusedOAuthClientLifetime = 24 * time.Hour
	maxOAuthRedirectURIs      = 10
	maxOAuthRedirectURILength = 2000
	maxOAuthClientName        = 100
	// maxPendingOAuth caps the requests and codes in memory
	maxPendingOAuth = 1000
	// defaultOAuthClientName is the token name of a client without client_name
	defaultOAuthClientName = "MCP client"
)

// OAuth error codes (RFC 6749, RFC 7591, RFC 8707)
const (
	OAuthInvalidRequest          = "invalid_request"
	OAuthInvalidClient           = "invalid_client"
	OAuthInvalidGrant            = "invalid_grant"
	OAuthUnsupportedGrantType    = "unsupported_grant_type"
	OAuthUnsupportedResponseType = "unsupported_response_type"
	OAuthInvalidScope            = "invalid_scope"
	OAuthInvalidTarget           = "invalid_target"
	OAuthAccessDenied            = "access_denied"
	OAuthInvalidRedirectURI      = "invalid_redirect_uri"
	OAuthInvalidClientMetadata   = "invalid_client_metadata"
)

// OAuthError is an error in the format of an OAuth error response
type OAuthError struct {
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	return e.Code + ": " + e.Description
}

func oauthError(code, description string) *OAuthError {
	return &OAuthError{Code: code, Description: description}
}

// ValidOAuthAccessToken reports whether a string has the format of an OAuth access token
func ValidOAuthAccessToken(token string) bool {
	return validTokenFormat(token, PrefixOAuthAccess)
}

// OAuthClient is a registered client (RFC 7591). All clients are public: they have no secret
// and prove themselves with PKCE.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// OAuthClientMetadata is the registration request of a client (RFC 7591 section 2). Fields
// this server does not use are ignored.
type OAuthClientMetadata struct {
	RedirectURIs  []string `json:"redirect_uris"`
	ClientName    string   `json:"client_name"`
	GrantTypes    []string `json:"grant_types"`
	ResponseTypes []string `json:"response_types"`
}

// OAuthRequest is an authorization request that waits for the decision of the user
type OAuthRequest struct {
	ID            string
	Client        *OAuthClient
	RedirectURI   string
	Scope         string
	State         string
	CodeChallenge string
	Resource      string
	ExpiresAt     time.Time
}

// OAuthTokens is the answer of the token endpoint
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	// Scope is the granted scope: ScopeRead or ScopeWrite
	Scope string
}

// oauthCode is an issued authorization code
type oauthCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	scope         string
	resource      string
	accountID     string
	expiresAt     time.Time
	// grantID is the app token made with the code; a second use of the code revokes it
	grantID string
	used    bool
}

// oauthState holds the pending requests and the codes
type oauthState struct {
	mu       sync.Mutex
	requests map[string]*OAuthRequest
	codes    map[string]*oauthCode // by hash of the code
}

// pruneLocked forgets expired requests and codes
func (o *oauthState) pruneLocked(now time.Time) {
	for id, r := range o.requests {
		if !now.Before(r.ExpiresAt) {
			delete(o.requests, id)
		}
	}
	for h, c := range o.codes {
		if !now.Before(c.expiresAt) {
			delete(o.codes, h)
		}
	}
}

// ParseOAuthScope reads a requested scope: ScopeWrite if it has "write", ScopeRead if it has
// "read", else "". Unknown scopes are ignored.
func ParseOAuthScope(scope string) string {
	read := false
	for _, s := range strings.Fields(scope) {
		switch s {
		case ScopeWrite:
			return ScopeWrite
		case ScopeRead:
			read = true
		}
	}
	if read {
		return ScopeRead
	}
	return ""
}

// OAuthScopeString is the scope of a token response: write includes read
func OAuthScopeString(scope string) string {
	if scope == ScopeWrite {
		return "read write"
	}
	return ScopeRead
}

// capScope limits a scope to what the account may do
func capScope(scope string, u *User) string {
	if scope == ScopeWrite && u.CanWrite() {
		return ScopeWrite
	}
	return ScopeRead
}

// schemePattern is a URI scheme
var schemePattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// forbiddenRedirectSchemes may never be a redirect URI: the web client navigates to it
var forbiddenRedirectSchemes = map[string]bool{
	"javascript": true, "data": true, "vbscript": true, "file": true, "about": true, "blob": true,
	"filesystem": true, "ws": true, "wss": true, "ftp": true, "mailto": true, "tel": true, "sms": true, "intent": true,
}

// isLoopbackHost reports whether a host name is the own machine (RFC 8252 section 7.3)
func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// ValidRedirectURI accepts https URLs, http URLs of the loopback interface and private-use
// schemes of native apps (RFC 8252), without fragment and credentials
func ValidRedirectURI(s string) bool {
	if s == "" || len(s) > maxOAuthRedirectURILength || strings.ContainsAny(s, "# \t\r\n\\") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	}
	return schemePattern.MatchString(u.Scheme) && !forbiddenRedirectSchemes[u.Scheme]
}

// AllowsRedirect reports whether a redirect URI is registered for the client: the same string,
// or for loopback http URIs the same URI with any port (RFC 8252 section 7.3)
func (c *OAuthClient) AllowsRedirect(uri string) bool {
	if uri == "" {
		return false
	}
	for _, r := range c.RedirectURIs {
		if r == uri || loopbackMatch(r, uri) {
			return true
		}
	}
	return false
}

func loopbackMatch(registered, uri string) bool {
	a, err := url.Parse(registered)
	if err != nil || a.Scheme != "http" || !isLoopbackHost(a.Hostname()) {
		return false
	}
	b, err := url.Parse(uri)
	if err != nil || b.Scheme != "http" || b.User != nil || b.Fragment != "" || strings.Contains(uri, "#") {
		return false
	}
	return a.Hostname() == b.Hostname() && a.EscapedPath() == b.EscapedPath() && a.RawQuery == b.RawQuery
}

// RegisterOAuthClient registers a client (RFC 7591)
func (s *Service) RegisterOAuthClient(m OAuthClientMetadata, ri RequestInfo) (*OAuthClient, error) {
	if len(m.RedirectURIs) == 0 || len(m.RedirectURIs) > maxOAuthRedirectURIs {
		return nil, oauthError(OAuthInvalidRedirectURI, "redirect_uris needs 1 to 10 URIs")
	}
	for _, u := range m.RedirectURIs {
		if !ValidRedirectURI(u) {
			return nil, oauthError(OAuthInvalidRedirectURI, "redirect URIs are https URLs, http URLs of 127.0.0.1, [::1] or localhost, or private-use schemes of native apps, without fragment: "+u)
		}
	}
	for _, g := range m.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return nil, oauthError(OAuthInvalidClientMetadata, "the grant types are authorization_code and refresh_token")
		}
	}
	for _, r := range m.ResponseTypes {
		if r != "code" {
			return nil, oauthError(OAuthInvalidClientMetadata, "the only response type is code")
		}
	}
	name := strings.TrimSpace(m.ClientName)
	if strings.IndexFunc(name, unicode.IsControl) >= 0 || len([]rune(name)) > maxOAuthClientName {
		return nil, oauthError(OAuthInvalidClientMetadata, "client_name has at most 100 characters and no control characters")
	}
	if name == "" {
		name = defaultOAuthClientName
	}
	uris, err := json.Marshal(m.RedirectURIs)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	c := &OAuthClient{ID: newID(), Name: name, RedirectURIs: m.RedirectURIs, CreatedAt: now}
	// registration needs no account: forget clients that never got a token
	if _, err = s.db.Exec("DELETE FROM oauth_clients WHERE last_used_at = 0 AND created_at < ?", toMillis(now.Add(-unusedOAuthClientLifetime))); err != nil {
		return nil, err
	}
	if _, err = s.db.Exec("INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)",
		c.ID, c.Name, string(uris), toMillis(now)); err != nil {
		return nil, err
	}
	s.Audit(AuditEvent{Type: AuditOAuthClientRegistered, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"clientId": c.ID, "clientName": c.Name, "redirectUris": c.RedirectURIs}})
	return c, nil
}

// OAuthClientByID returns a registered client, ErrNotFound for unknown ids
func (s *Service) OAuthClientByID(id string) (*OAuthClient, error) {
	if id == "" || len(id) > 64 {
		return nil, ErrNotFound
	}
	var c OAuthClient
	var uris string
	var created int64
	err := s.db.QueryRow("SELECT id, name, redirect_uris, created_at FROM oauth_clients WHERE id = ?", id).Scan(&c.ID, &c.Name, &uris, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	c.CreatedAt = fromMillis(created)
	return &c, nil
}

// codeChallengePattern is a S256 code challenge: base64url of 32 bytes
var codeChallengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// codeVerifierPattern is a PKCE code verifier (RFC 7636 section 4.1)
var codeVerifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// StartOAuthRequest stores an authorization request of a client until the user decides. The
// caller checked the client, the redirect URI and the resource. An empty scope asks for
// write.
func (s *Service) StartOAuthRequest(c *OAuthClient, redirectURI, scope, state, codeChallenge, resource string) (*OAuthRequest, error) {
	if !codeChallengePattern.MatchString(codeChallenge) {
		return nil, oauthError(OAuthInvalidRequest, "code_challenge must be a S256 PKCE challenge")
	}
	granted := ParseOAuthScope(scope)
	if granted == "" {
		if strings.TrimSpace(scope) != "" {
			return nil, oauthError(OAuthInvalidScope, "the scopes are read and write")
		}
		granted = ScopeWrite
	}
	if len(state) > 1024 {
		return nil, oauthError(OAuthInvalidRequest, "state is too long")
	}
	now := s.now()
	r := &OAuthRequest{ID: newID() + newID(), Client: c, RedirectURI: redirectURI, Scope: granted, State: state,
		CodeChallenge: codeChallenge, Resource: resource, ExpiresAt: now.Add(OAuthRequestLifetime)}
	o := &s.oauth
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(now)
	if len(o.requests) >= maxPendingOAuth {
		return nil, &RateLimitError{RetryAfter: OAuthRequestLifetime}
	}
	if o.requests == nil {
		o.requests = map[string]*OAuthRequest{}
	}
	o.requests[r.ID] = r
	return r, nil
}

// PendingOAuthRequest returns a request that waits for a decision, ErrNotFound for unknown,
// decided and expired ones
func (s *Service) PendingOAuthRequest(id string) (*OAuthRequest, error) {
	o := &s.oauth
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.requests[id]
	if r == nil || !s.now().Before(r.ExpiresAt) {
		return nil, ErrNotFound
	}
	return r, nil
}

// takeOAuthRequest removes a pending request for its decision
func (s *Service) takeOAuthRequest(id string) (*OAuthRequest, error) {
	o := &s.oauth
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.requests[id]
	if r == nil || !s.now().Before(r.ExpiresAt) {
		return nil, ErrNotFound
	}
	delete(o.requests, id)
	return r, nil
}

// redirectWith adds query parameters to a redirect URI
func redirectWith(redirectURI string, params url.Values) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// OAuthErrorRedirect is the redirect URI with an error response (RFC 6749 section 4.1.2.1)
func OAuthErrorRedirect(redirectURI, state string, e *OAuthError) string {
	params := url.Values{"error": {e.Code}, "error_description": {e.Description}}
	if state != "" {
		params.Set("state", state)
	}
	return redirectWith(redirectURI, params)
}

// ApproveOAuthRequest approves a request for an account and returns the redirect with the
// code. scope "" keeps the requested scope; it can only be lowered, and readers get read.
func (s *Service) ApproveOAuthRequest(u *User, id, scope string, ri RequestInfo) (string, error) {
	if scope != "" && scope != ScopeRead && scope != ScopeWrite {
		return "", ErrInvalidTokenScope
	}
	r, err := s.takeOAuthRequest(id)
	if err != nil {
		return "", err
	}
	granted := r.Scope
	if scope == ScopeRead {
		granted = ScopeRead
	}
	granted = capScope(granted, u)
	code := newToken(PrefixOAuthCode)
	now := s.now()
	o := &s.oauth
	o.mu.Lock()
	o.pruneLocked(now)
	if len(o.codes) >= maxPendingOAuth {
		o.mu.Unlock()
		return "", &RateLimitError{RetryAfter: OAuthCodeLifetime}
	}
	if o.codes == nil {
		o.codes = map[string]*oauthCode{}
	}
	o.codes[string(tokenHash(code))] = &oauthCode{clientID: r.Client.ID, redirectURI: r.RedirectURI, codeChallenge: r.CodeChallenge,
		scope: granted, resource: r.Resource, accountID: u.ID, expiresAt: now.Add(OAuthCodeLifetime)}
	o.mu.Unlock()
	s.Audit(AuditEvent{Type: AuditOAuthApproved, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"clientId": r.Client.ID, "clientName": r.Client.Name, "scope": granted, "redirectUri": r.RedirectURI}})
	params := url.Values{"code": {code}}
	if r.State != "" {
		params.Set("state", r.State)
	}
	return redirectWith(r.RedirectURI, params), nil
}

// DenyOAuthRequest denies a request and returns the redirect with the error access_denied
func (s *Service) DenyOAuthRequest(u *User, id string, ri RequestInfo) (string, error) {
	r, err := s.takeOAuthRequest(id)
	if err != nil {
		return "", err
	}
	s.Audit(AuditEvent{Type: AuditOAuthDenied, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"clientId": r.Client.ID, "clientName": r.Client.Name}})
	return OAuthErrorRedirect(r.RedirectURI, r.State, oauthError(OAuthAccessDenied, "the user denied the request")), nil
}

// verifyPKCE checks a S256 code verifier against its challenge
func verifyPKCE(verifier, challenge string) bool {
	if !codeVerifierPattern.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

var errInvalidCode = oauthError(OAuthInvalidGrant, "the code is invalid, expired or used")

// ExchangeOAuthCode trades an authorization code for tokens (RFC 6749 section 4.1.3 with
// PKCE). A code works once; using it again revokes the grant made with it. resource "" is
// the resource of the request.
func (s *Service) ExchangeOAuthCode(clientID, code, redirectURI, verifier, resource string, ri RequestInfo) (*OAuthTokens, error) {
	if !validTokenFormat(code, PrefixOAuthCode) {
		return nil, errInvalidCode
	}
	now := s.now()
	o := &s.oauth
	o.mu.Lock()
	c := o.codes[string(tokenHash(code))]
	if c == nil || !now.Before(c.expiresAt) {
		o.mu.Unlock()
		return nil, errInvalidCode
	}
	if c.used {
		grantID := c.grantID
		o.mu.Unlock()
		if grantID != "" {
			s.revokeGrant(grantID, c.accountID, "code-reused", ri)
		}
		return nil, errInvalidCode
	}
	// every attempt uses the code up
	c.used = true
	o.mu.Unlock()
	switch {
	case c.clientID != clientID:
		return nil, errInvalidCode
	case c.redirectURI != redirectURI:
		return nil, oauthError(OAuthInvalidGrant, "redirect_uri differs from the authorization request")
	case !verifyPKCE(verifier, c.codeChallenge):
		return nil, oauthError(OAuthInvalidGrant, "the code_verifier does not match the code_challenge")
	case resource != "" && resource != c.resource:
		return nil, oauthError(OAuthInvalidTarget, "resource differs from the authorization request")
	}
	client, err := s.OAuthClientByID(clientID)
	if err != nil {
		return nil, oauthError(OAuthInvalidClient, "unknown client")
	}
	u := s.UserByID(c.accountID)
	if u == nil || u.Status != StatusActive {
		return nil, errInvalidCode
	}
	tokens, grantID, err := s.createGrant(u, client, capScope(c.scope, u), c.resource, ri)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	c.grantID = grantID
	o.mu.Unlock()
	return tokens, nil
}

// tokenName is the app token name of a client: at most 64 characters
func tokenName(clientName string) string {
	r := []rune(clientName)
	if len(r) > 64 {
		r = r[:64]
	}
	return strings.TrimSpace(string(r))
}

// createGrant makes the app token of a grant and its first tokens
func (s *Service) createGrant(u *User, client *OAuthClient, scope, resource string, ri RequestInfo) (*OAuthTokens, string, error) {
	now := s.now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec("DELETE FROM oauth_tokens WHERE expires_at <= ?", toMillis(now)); err != nil {
		return nil, "", err
	}
	id, name := newID(), tokenName(client.Name)
	// the app token itself is never handed out: the grant is used with its access tokens
	if _, err = tx.Exec("INSERT INTO app_tokens (id, account_id, name, scope, token_hash, created_at, expires_at, oauth_client_id, resource) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, u.ID, name, scope, tokenHash(newToken(PrefixAppToken)), toMillis(now), toMillis(now.Add(OAuthRefreshTokenLifetime)), client.ID, resource); err != nil {
		return nil, "", err
	}
	if _, err = tx.Exec("UPDATE oauth_clients SET last_used_at = ? WHERE id = ?", toMillis(now), client.ID); err != nil {
		return nil, "", err
	}
	tokens, err := insertTokenPair(tx, id, scope, now)
	if err != nil {
		return nil, "", err
	}
	if err = insertAudit(tx, now, AuditEvent{Type: AuditAppTokenCreated, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"tokenId": id, "name": name, "scope": scope, "oauthClientId": client.ID}}); err != nil {
		return nil, "", err
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	return tokens, id, nil
}

// insertTokenPair stores a new access and refresh token of a grant
func insertTokenPair(tx *sql.Tx, grantID, scope string, now time.Time) (*OAuthTokens, error) {
	t := &OAuthTokens{AccessToken: newToken(PrefixOAuthAccess), RefreshToken: newToken(PrefixOAuthRefresh),
		ExpiresIn: int(OAuthAccessTokenLifetime / time.Second), Scope: scope}
	for _, row := range []struct {
		token, kind string
		lifetime    time.Duration
	}{{t.AccessToken, "access", OAuthAccessTokenLifetime}, {t.RefreshToken, "refresh", OAuthRefreshTokenLifetime}} {
		if _, err := tx.Exec("INSERT INTO oauth_tokens (token_hash, app_token_id, kind, created_at, expires_at) VALUES (?, ?, ?, ?, ?)",
			tokenHash(row.token), grantID, row.kind, toMillis(now), toMillis(now.Add(row.lifetime))); err != nil {
			return nil, err
		}
	}
	return t, nil
}

var errInvalidRefreshToken = oauthError(OAuthInvalidGrant, "the refresh token is invalid, expired or revoked")

// RefreshOAuthToken trades a refresh token for new tokens (RFC 6749 section 6). Refresh tokens
// rotate: using one twice revokes the grant. scope "" keeps the scope, read lowers it.
func (s *Service) RefreshOAuthToken(clientID, refreshToken, scope string, ri RequestInfo) (*OAuthTokens, error) {
	if !validTokenFormat(refreshToken, PrefixOAuthRefresh) {
		return nil, errInvalidRefreshToken
	}
	requested := ParseOAuthScope(scope)
	if requested == "" && strings.TrimSpace(scope) != "" {
		return nil, oauthError(OAuthInvalidScope, "the scopes are read and write")
	}
	now := s.now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var grantID string
	var expires, used int64
	err = tx.QueryRow("SELECT app_token_id, expires_at, used_at FROM oauth_tokens WHERE token_hash = ? AND kind = 'refresh'", tokenHash(refreshToken)).
		Scan(&grantID, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errInvalidRefreshToken
	}
	if err != nil {
		return nil, err
	}
	g, err := scanAppToken(tx.QueryRow("SELECT "+appTokenColumns+" FROM app_tokens WHERE id = ?", grantID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errInvalidRefreshToken
	}
	if err != nil {
		return nil, err
	}
	if g.OAuthClientID != clientID {
		return nil, errInvalidRefreshToken
	}
	if used != 0 {
		// a stolen refresh token, or a client that lost the answer: end the grant
		_ = tx.Rollback()
		s.revokeGrant(grantID, g.AccountID, "refresh-token-reused", ri)
		return nil, errInvalidRefreshToken
	}
	if expires <= toMillis(now) {
		return nil, errInvalidRefreshToken
	}
	u := s.UserByID(g.AccountID)
	if u == nil || u.Status != StatusActive {
		return nil, errInvalidRefreshToken
	}
	granted := capScope(g.Scope, u)
	if requested == ScopeRead {
		granted = ScopeRead
	} else if requested == ScopeWrite && g.Scope != ScopeWrite {
		return nil, oauthError(OAuthInvalidScope, "the grant has the scope read")
	}
	if _, err = tx.Exec("UPDATE oauth_tokens SET used_at = ? WHERE token_hash = ?", toMillis(now), tokenHash(refreshToken)); err != nil {
		return nil, err
	}
	// old access tokens of the grant end with the rotation
	if _, err = tx.Exec("DELETE FROM oauth_tokens WHERE app_token_id = ? AND (kind = 'access' OR expires_at <= ?)", grantID, toMillis(now)); err != nil {
		return nil, err
	}
	tokens, err := insertTokenPair(tx, grantID, granted, now)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE app_tokens SET scope = ?, expires_at = ?, last_used_at = ? WHERE id = ?",
		granted, toMillis(now.Add(OAuthRefreshTokenLifetime)), toMillis(now), grantID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return tokens, nil
}

// RevokeOAuthToken ends the grant of an access or refresh token (RFC 7009). Unknown tokens are
// no error.
func (s *Service) RevokeOAuthToken(token string, ri RequestInfo) error {
	if !validTokenFormat(token, PrefixOAuthAccess) && !validTokenFormat(token, PrefixOAuthRefresh) {
		return nil
	}
	var grantID, accountID string
	err := s.db.QueryRow("SELECT t.id, t.account_id FROM oauth_tokens o JOIN app_tokens t ON t.id = o.app_token_id WHERE o.token_hash = ?", tokenHash(token)).
		Scan(&grantID, &accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.revokeGrant(grantID, accountID, "client", ri)
	return nil
}

// revokeGrant deletes the app token of a grant with all its tokens
func (s *Service) revokeGrant(grantID, accountID, reason string, ri RequestInfo) {
	res, err := s.db.Exec("DELETE FROM app_tokens WHERE id = ? AND oauth_client_id != ''", grantID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	s.Audit(AuditEvent{Type: AuditAppTokenRevoked, ActorID: accountID, TargetID: accountID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"tokenId": grantID, "reason": reason}})
}
