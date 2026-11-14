package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v5"
)

// OAuth 2.1 for MCP clients (the authorization part of the MCP specification), see
// docs/oauth.md. The backend is both the authorization server (issuer: the backend URL, such as
// https://wiki.example.com/api) and the protected resource (the MCP endpoint). The user
// approves requests in the web client at /connect/authorize.

const (
	// FeatureOAuth means the OAuth endpoints exist
	FeatureOAuth = "oauth"

	oauthPathAuthorize = "/oauth/authorize/"
	oauthPathToken     = "/oauth/token/" //nolint:gosec // a path
	oauthPathRegister  = "/oauth/register/"
	oauthPathRevoke    = "/oauth/revoke/"
	// oauthConsentPath is the page of the web client that shows a request
	oauthConsentPath = "/connect/authorize"

	wellKnownProtectedResource = "/.well-known/oauth-protected-resource"
	wellKnownAuthServer        = "/.well-known/oauth-authorization-server"
	wellKnownOpenIDConfig      = "/.well-known/openid-configuration"

	maxOAuthRequestBytes = 64 << 10
)

// wellKnownOAuthPaths are the metadata routes. They answer at the root (outside APIPrefix) and
// below APIPrefix; the suffixed forms are the RFC 8414 and RFC 9728 locations of the issuer
// <origin>/api and the resource <origin>/api/mcp.
var wellKnownOAuthPaths = map[string]bool{
	wellKnownProtectedResource + "/":         true,
	wellKnownProtectedResource + "/api/mcp/": true,
	wellKnownProtectedResource + "/mcp/":     true,
	wellKnownAuthServer + "/":                false,
	wellKnownAuthServer + "/api/":            false,
	wellKnownOpenIDConfig + "/":              false,
	wellKnownOpenIDConfig + "/api/":          false,
}

// isRootWellKnownPath reports whether a path outside APIPrefix is OAuth metadata
func isRootWellKnownPath(p string) bool {
	return strings.HasPrefix(p, "/.well-known/oauth-") || strings.HasPrefix(p, wellKnownOpenIDConfig)
}

// isOAuthPublicPath reports whether a path is a public OAuth endpoint: those authenticate the
// client, not the user, and ignore cookies and Authorization headers
func isOAuthPublicPath(p string) bool {
	switch p {
	case oauthPathAuthorize, oauthPathToken, oauthPathRegister, oauthPathRevoke:
		return true
	}
	_, ok := wellKnownOAuthPaths[p]
	return ok
}

// registerOAuthRoutes adds the OAuth endpoints and metadata
func (rs *RestService) registerOAuthRoutes(echoRest *echo.Echo) {
	for p, resource := range wellKnownOAuthPaths {
		if resource {
			echoRest.GET(p, rs.protectedResourceMetadata)
		} else {
			echoRest.GET(p, rs.authServerMetadata)
		}
	}
	echoRest.GET(oauthPathAuthorize, rs.oauthAuthorize)
	echoRest.POST(oauthPathToken, rs.oauthToken)
	echoRest.POST(oauthPathRegister, rs.oauthRegister)
	echoRest.POST(oauthPathRevoke, rs.oauthRevoke)
	echoRest.GET("/oauth/requests/:"+urlParamID+"/", rs.getOAuthRequest)
	echoRest.POST("/oauth/requests/:"+urlParamID+"/approve/", rs.approveOAuthRequest)
	echoRest.POST("/oauth/requests/:"+urlParamID+"/deny/", rs.denyOAuthRequest)
}

// oauthIssuer is the issuer and base of the OAuth endpoints: the backend URL
func (rs *RestService) oauthIssuer(c *echo.Context) string {
	return rs.backendURL(c)
}

// mcpResource is the resource indicator of the MCP endpoint (RFC 8707)
func (rs *RestService) mcpResource(c *echo.Context) string {
	return rs.backendURL(c) + "/mcp"
}

// protectedResourceMetadataURL is where clients find the metadata of the MCP endpoint: below
// APIPrefix, so it works without routes for the root /.well-known/ paths
func (rs *RestService) protectedResourceMetadataURL(c *echo.Context) string {
	return rs.backendURL(c) + wellKnownProtectedResource + "/mcp"
}

// isMCPResource compares a resource indicator with the MCP endpoint
func (rs *RestService) isMCPResource(c *echo.Context, resource string) bool {
	return strings.EqualFold(strings.TrimRight(resource, "/"), rs.mcpResource(c))
}

// ProtectedResourceMetadata is RFC 9728 metadata
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name,omitempty"`
}

// AuthServerMetadata is RFC 8414 metadata
type AuthServerMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint"`
	RevocationEndpoint                     string   `json:"revocation_endpoint"`
	ScopesSupported                        []string `json:"scopes_supported"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	ResponseModesSupported                 []string `json:"response_modes_supported"`
	GrantTypesSupported                    []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocumentSupported      bool     `json:"client_id_metadata_document_supported"`
}

var oauthScopes = []string{auth.ScopeRead, auth.ScopeWrite}

// oauthEnabled answers 404 when authentication is off: there are no accounts to grant
func (rs *RestService) oauthEnabled(c *echo.Context) error {
	if !rs.authenticator.Enabled() {
		return responded(apiError(c, http.StatusNotFound, CodeNotFound, "authentication is disabled (server.auth.disabled)", nil))
	}
	return nil
}

// GET /.well-known/oauth-protected-resource
func (rs *RestService) protectedResourceMetadata(c *echo.Context) error {
	if err := rs.oauthEnabled(c); err != nil {
		return err
	}
	return c.JSONPretty(http.StatusOK, &ProtectedResourceMetadata{
		Resource:               rs.mcpResource(c),
		AuthorizationServers:   []string{rs.oauthIssuer(c)},
		ScopesSupported:        oauthScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           rs.instanceName(c),
	}, jsonIndent)
}

// GET /.well-known/oauth-authorization-server
func (rs *RestService) authServerMetadata(c *echo.Context) error {
	if err := rs.oauthEnabled(c); err != nil {
		return err
	}
	issuer := rs.oauthIssuer(c)
	return c.JSONPretty(http.StatusOK, &AuthServerMetadata{
		Issuer:                                 issuer,
		AuthorizationEndpoint:                  issuer + "/oauth/authorize",
		TokenEndpoint:                          issuer + "/oauth/token",
		RegistrationEndpoint:                   issuer + "/oauth/register",
		RevocationEndpoint:                     issuer + "/oauth/revoke",
		ScopesSupported:                        oauthScopes,
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:          []string{"S256"},
	}, jsonIndent)
}

// oauthErrorResponse is an OAuth error (RFC 6749 section 5.2)
type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// oauthJSON writes a JSON answer of the token or registration endpoint, never cached
func oauthJSON(c *echo.Context, status int, v any) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	c.Response().Header().Set("Pragma", "no-cache")
	return c.JSON(status, v)
}

// oauthFail writes the error of a token, registration or revocation request
func (rs *RestService) oauthFail(c *echo.Context, err error) error {
	var oe *auth.OAuthError
	if errors.As(err, &oe) {
		status := http.StatusBadRequest
		if oe.Code == auth.OAuthInvalidClient {
			status = http.StatusUnauthorized
		}
		return oauthJSON(c, status, &oauthErrorResponse{Error: oe.Code, ErrorDescription: oe.Description})
	}
	return rs.authError(c, err)
}

// limitOAuth applies the rate limit of the public OAuth endpoints
func (rs *RestService) limitOAuth(c *echo.Context) error {
	if err := rs.authenticator.AllowOAuth(c.RealIP()); err != nil {
		return responded(rs.authError(c, err))
	}
	return nil
}

// oauthPage answers a browser at the authorization endpoint when the client cannot be told:
// an unknown client or redirect URI must not lead to a redirect
func oauthPage(c *echo.Context, status int, message string) error {
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.String(status, "This app cannot connect: "+message+"\n")
}

// GET /oauth/authorize/: checks the request of a client and sends the browser to the consent
// page of the web client
func (rs *RestService) oauthAuthorize(c *echo.Context) error {
	if err := rs.oauthEnabled(c); err != nil {
		return err
	}
	if err := rs.limitOAuth(c); err != nil {
		return err
	}
	q := c.QueryParams()
	client, err := rs.authenticator.OAuthClientByID(q.Get("client_id"))
	if errors.Is(err, auth.ErrNotFound) {
		return oauthPage(c, http.StatusBadRequest, "the client_id is unknown. Register the client again.")
	}
	if err != nil {
		return rs.authError(c, err)
	}
	redirectURI := q.Get("redirect_uri")
	if !client.AllowsRedirect(redirectURI) {
		return oauthPage(c, http.StatusBadRequest, "the redirect_uri is not registered for this client.")
	}
	state := q.Get("state")
	fail := func(code, description string) error {
		return c.Redirect(http.StatusFound, auth.OAuthErrorRedirect(redirectURI, state, &auth.OAuthError{Code: code, Description: description}))
	}
	if q.Get("response_type") != "code" {
		return fail(auth.OAuthUnsupportedResponseType, "the only response_type is code")
	}
	if q.Get("code_challenge_method") != "S256" {
		return fail(auth.OAuthInvalidRequest, "PKCE with code_challenge_method S256 is required")
	}
	if resource := q.Get("resource"); resource != "" && !rs.isMCPResource(c, resource) {
		return fail(auth.OAuthInvalidTarget, "the only resource is "+rs.mcpResource(c))
	}
	req, err := rs.authenticator.StartOAuthRequest(client, redirectURI, q.Get("scope"), state, q.Get("code_challenge"), rs.mcpResource(c))
	var oe *auth.OAuthError
	if errors.As(err, &oe) {
		return fail(oe.Code, oe.Description)
	}
	if err != nil {
		return fail("temporarily_unavailable", "too many pending requests, try again later")
	}
	return c.Redirect(http.StatusFound, rs.consentURL(c, req.ID))
}

// consentURL is the page of the web client that shows a request
func (rs *RestService) consentURL(c *echo.Context, id string) string {
	base := rs.authenticator.ClientURL()
	if base == "" {
		base = rs.ownOrigin(c)
	}
	return base + oauthConsentPath + "?request=" + url.QueryEscape(id)
}

// OAuthTokenResponse is the answer of the token endpoint (RFC 6749 section 5.1)
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// oauthForm reads the form body of a token or revocation request
func oauthForm(c *echo.Context) (url.Values, error) {
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, maxOAuthRequestBytes)
	if err := req.ParseForm(); err != nil {
		return nil, &auth.OAuthError{Code: auth.OAuthInvalidRequest, Description: "the body must be application/x-www-form-urlencoded"}
	}
	return req.PostForm, nil
}

// clientIDOf is the client_id of the form, or the user of Basic auth with an empty secret
func clientIDOf(c *echo.Context, form url.Values) string {
	if id := form.Get("client_id"); id != "" {
		return id
	}
	if user, _, ok := c.Request().BasicAuth(); ok {
		return user
	}
	return ""
}

// POST /oauth/token/
func (rs *RestService) oauthToken(c *echo.Context) error {
	if err := rs.oauthEnabled(c); err != nil {
		return err
	}
	if err := rs.limitOAuth(c); err != nil {
		return err
	}
	form, err := oauthForm(c)
	if err != nil {
		return rs.oauthFail(c, err)
	}
	clientID := clientIDOf(c, form)
	if clientID == "" {
		return rs.oauthFail(c, &auth.OAuthError{Code: auth.OAuthInvalidClient, Description: "client_id is required"})
	}
	resource := form.Get("resource")
	if resource != "" && !rs.isMCPResource(c, resource) {
		return rs.oauthFail(c, &auth.OAuthError{Code: auth.OAuthInvalidTarget, Description: "the only resource is " + rs.mcpResource(c)})
	}
	if resource != "" {
		resource = rs.mcpResource(c)
	}
	ri := requestInfo(c)
	var tokens *auth.OAuthTokens
	switch form.Get("grant_type") {
	case "authorization_code":
		tokens, err = rs.authenticator.ExchangeOAuthCode(clientID, form.Get("code"), form.Get("redirect_uri"), form.Get("code_verifier"), resource, ri)
	case "refresh_token":
		tokens, err = rs.authenticator.RefreshOAuthToken(clientID, form.Get("refresh_token"), form.Get("scope"), ri)
	default:
		err = &auth.OAuthError{Code: auth.OAuthUnsupportedGrantType, Description: "the grant types are authorization_code and refresh_token"}
	}
	if err != nil {
		return rs.oauthFail(c, err)
	}
	return oauthJSON(c, http.StatusOK, &OAuthTokenResponse{AccessToken: tokens.AccessToken, TokenType: "Bearer", ExpiresIn: tokens.ExpiresIn,
		RefreshToken: tokens.RefreshToken, Scope: auth.OAuthScopeString(tokens.Scope)})
}

// OAuthClientResponse is the answer of the registration endpoint (RFC 7591 section 3.2.1)
type OAuthClientResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

// POST /oauth/register/
func (rs *RestService) oauthRegister(c *echo.Context) error {
	if err := rs.oauthEnabled(c); err != nil {
		return err
	}
	if err := rs.limitOAuth(c); err != nil {
		return err
	}
	var m auth.OAuthClientMetadata
	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, maxOAuthRequestBytes)
	if err := json.NewDecoder(req.Body).Decode(&m); err != nil {
		return rs.oauthFail(c, &auth.OAuthError{Code: auth.OAuthInvalidClientMetadata, Description: "the body must be a JSON object of client metadata"})
	}
	client, err := rs.authenticator.RegisterOAuthClient(m, requestInfo(c))
	if err != nil {
		return rs.oauthFail(c, err)
	}
	return oauthJSON(c, http.StatusCreated, &OAuthClientResponse{
		ClientID:                client.ID,
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   strings.Join(oauthScopes, " "),
	})
}

// POST /oauth/revoke/ (RFC 7009): ends the grant of a token
func (rs *RestService) oauthRevoke(c *echo.Context) error {
	if err := rs.oauthEnabled(c); err != nil {
		return err
	}
	if err := rs.limitOAuth(c); err != nil {
		return err
	}
	form, err := oauthForm(c)
	if err != nil {
		return rs.oauthFail(c, err)
	}
	if err = rs.authenticator.RevokeOAuthToken(form.Get("token"), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusOK)
}

// OAuthRequestView is a pending authorization request, for the consent page
type OAuthRequestView struct {
	ID         string `json:"id"`
	ClientID   string `json:"clientId"`
	ClientName string `json:"clientName"`
	// RedirectURI is where the browser goes after the decision
	RedirectURI string `json:"redirectUri"`
	// Scope is the requested scope, read or write
	Scope string `json:"scope"`
	// AllowedScopes are the scopes the user may grant: readers only read
	AllowedScopes []string  `json:"allowedScopes"`
	Resource      string    `json:"resource"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// OAuthDecisionRequest is the body of POST /oauth/requests/{id}/approve/
type OAuthDecisionRequest struct {
	// Scope lowers the requested scope to read; empty keeps it
	Scope string `json:"scope"`
}

// OAuthDecisionResponse tells the web client where to send the browser
type OAuthDecisionResponse struct {
	RedirectURL string `json:"redirectUrl"`
}

// GET /oauth/requests/{id}/
func (rs *RestService) getOAuthRequest(c *echo.Context) error {
	r, err := rs.authenticator.PendingOAuthRequest(c.Param(urlParamID))
	if err != nil {
		return rs.authError(c, err)
	}
	allowed := []string{auth.ScopeRead}
	if UserFromContext(c).CanWrite() {
		allowed = oauthScopes
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &OAuthRequestView{ID: r.ID, ClientID: r.Client.ID, ClientName: r.Client.Name, RedirectURI: r.RedirectURI,
		Scope: r.Scope, AllowedScopes: allowed, Resource: r.Resource, ExpiresAt: r.ExpiresAt.UTC()}, jsonIndent)
}

// POST /oauth/requests/{id}/approve/: like creating an app token, it needs a recent login
func (rs *RestService) approveOAuthRequest(c *echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	var r OAuthDecisionRequest
	if c.Request().ContentLength != 0 {
		if err := decodeJSON(c, &r); err != nil {
			return err
		}
	}
	location, err := rs.authenticator.ApproveOAuthRequest(UserFromContext(c), c.Param(urlParamID), r.Scope, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &OAuthDecisionResponse{RedirectURL: location}, jsonIndent)
}

// POST /oauth/requests/{id}/deny/
func (rs *RestService) denyOAuthRequest(c *echo.Context) error {
	location, err := rs.authenticator.DenyOAuthRequest(UserFromContext(c), c.Param(urlParamID), requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &OAuthDecisionResponse{RedirectURL: location}, jsonIndent)
}

// mcpChallenge is the WWW-Authenticate header of a 401 of the MCP endpoint: it points clients
// to the OAuth metadata (RFC 9728 section 5.1)
func (rs *RestService) mcpChallenge(c *echo.Context) string {
	v := `Bearer resource_metadata="` + rs.protectedResourceMetadataURL(c) + `", scope="read write"`
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		v += `, error="invalid_token"`
	}
	return v
}
