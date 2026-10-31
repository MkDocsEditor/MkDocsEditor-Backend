package backend

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

const (
	contextKeyUser       = "mkdocseditor.user"
	contextKeySession    = "mkdocseditor.session"
	contextKeyAuthMethod = "mkdocseditor.authMethod"

	EndpointPathWhoAmI   = "/auth/whoami/"
	EndpointPathWsTicket = "/auth/ws-ticket/"

	// session cookie names: with the __Host- prefix over HTTPS, without over plain HTTP
	cookieNameSecure   = "__Host-mkdocseditor_session"
	cookieNameInsecure = "mkdocseditor_session"
)

// Authentication methods of a request (whoami authMethod)
const (
	AuthMethodSession   = "session"
	AuthMethodAppToken  = "app-token"
	AuthMethodBasic     = "basic"
	AuthMethodAnonymous = "anonymous"
)

// Error codes of REST error responses, see docs/auth.md section 10
//
//nolint:gosec // error codes, not credentials
const (
	CodeBadRequest                = "bad-request"
	CodeInvalidUsername           = "invalid-username"
	CodeWeakPassword              = "weak-password"
	CodeInvalidLink               = "invalid-link"
	CodeInvalidSecondFactor       = "invalid-second-factor"
	CodePasskeyRegistrationFailed = "passkey-registration-failed"
	CodeUnauthenticated           = "unauthenticated"
	CodeInvalidCredentials        = "invalid-credentials"
	CodeChallengeExpired          = "challenge-expired"
	CodePasskeyVerificationFailed = "passkey-verification-failed"
	CodeForbidden                 = "forbidden"
	CodeAccountDisabled           = "account-disabled"
	CodeReauthRequired            = "reauth-required"
	CodeSecondFactorSetupRequired = "second-factor-setup-required"
	CodeCSRFCheckFailed           = "csrf-check-failed"
	CodeOriginNotAllowed          = "origin-not-allowed"
	CodeNotFound                  = "not-found"
	CodePasskeysDisabled          = "passkeys-disabled"
	CodeUsernameTaken             = "username-taken"
	CodeTOTPAlreadyEnabled        = "totp-already-enabled"
	CodeTOTPNotEnabled            = "totp-not-enabled"
	CodeLastLoginMethod           = "last-login-method"
	CodeSecondFactorRequired      = "second-factor-required"
	CodeConflict                  = "conflict"
	CodeRateLimited               = "rate-limited"
	CodeInternal                  = "internal"
)

// TicketResponse is the response of POST /auth/ws-ticket/
type TicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// UserFromContext returns the authenticated user of a request, or nil
func UserFromContext(c echo.Context) *auth.User {
	u, _ := c.Get(contextKeyUser).(*auth.User)
	return u
}

// SessionFromContext returns the session of a request authenticated by a session, or nil
func SessionFromContext(c echo.Context) *auth.Session {
	s, _ := c.Get(contextKeySession).(*auth.Session)
	return s
}

func authMethodFromContext(c echo.Context) string {
	m, _ := c.Get(contextKeyAuthMethod).(string)
	return m
}

// isWebsocketPath reports whether the request path is a websocket endpoint.
// Websockets may also authenticate after the upgrade (ticket in the hello message).
func isWebsocketPath(path string) bool {
	return strings.HasSuffix(path, "/ws/")
}

func isReadOnlyMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// isPublicPath reports whether a path is reachable without authentication
func isPublicPath(path string) bool {
	switch path {
	case EndpointPathAlive, "/auth/status/", "/.well-known/assetlinks.json/":
		return true
	}
	return strings.HasPrefix(path, "/auth/login/") || strings.HasPrefix(path, "/auth/passkey/login/") || strings.HasPrefix(path, "/auth/link/")
}

// restrictedPaths are reachable by a session that must still set up a second factor
func isRestrictedAllowed(method, path string) bool {
	switch path {
	case EndpointPathWhoAmI, "/auth/logout/", "/auth/status/", "/account/", "/account/totp/", "/account/totp/confirm/",
		"/account/passkeys/", "/account/passkeys/begin/", "/account/passkeys/finish/":
		return path != "/account/" || method == http.MethodGet
	}
	return strings.HasPrefix(path, "/auth/reauth/")
}

// errResponded is returned by helpers that already wrote an error response. The HTTP
// error handler ignores it.
var errResponded = errors.New("error response written")

// responded turns the result of writing a response into errResponded, so callers stop
func responded(err error) error {
	if err != nil {
		return err
	}
	return errResponded
}

// stop ends a middleware after a helper wrote the response
func stop(err error) error {
	if errors.Is(err, errResponded) {
		return nil
	}
	return err
}

// apiError writes an error response with a machine-readable code
func apiError(c echo.Context, status int, code string, message string, details map[string]any) error {
	return c.JSONPretty(status, &ErrorResult{Name: http.StatusText(status), Code: code, Message: message, Details: details}, indentationChar)
}

// authMiddleware authenticates every request (session cookie, bearer session, legacy basic
// auth), applies the CSRF checks and enforces the role of the user. See docs/auth.md
// sections 4 and 5.
func (rs *RestService) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		path := req.URL.Path
		if req.Method == http.MethodOptions || path == EndpointPathAlive {
			return next(c)
		}

		if !rs.authenticator.Enabled() {
			c.Set(contextKeyUser, auth.Anonymous())
			c.Set(contextKeyAuthMethod, AuthMethodAnonymous)
			return next(c)
		}

		authorization := req.Header.Get(echo.HeaderAuthorization)
		cookie := rs.sessionCookie(c)
		if err := rs.checkCSRF(c, authorization != "", cookie != ""); err != nil {
			return stop(err)
		}

		switch {
		case authorization != "":
			if err := rs.authenticateHeader(c, authorization); err != nil {
				return stop(err)
			}
		case cookie != "":
			// a websocket upgrade from a foreign page must not ride on the cookie
			if !isWebsocketPath(path) || rs.originAllowed(c, req.Header.Get(echo.HeaderOrigin)) {
				user, sess, err := rs.authenticator.AuthenticateSession(cookie, c.RealIP())
				if err != nil {
					rs.clearSessionCookie(c)
					if !isPublicPath(path) {
						return rs.unauthorized(c)
					}
				} else {
					c.Set(contextKeyUser, user)
					c.Set(contextKeySession, sess)
					c.Set(contextKeyAuthMethod, AuthMethodSession)
				}
			}
		}

		user := UserFromContext(c)
		if user == nil {
			if isPublicPath(path) || isWebsocketPath(path) {
				return next(c)
			}
			return rs.unauthorized(c)
		}
		if err := rs.authorize(c, user); err != nil {
			return stop(err)
		}
		return next(c)
	}
}

// authenticateHeader handles the Authorization header. An invalid header is a 401, the
// cookie is not tried.
func (rs *RestService) authenticateHeader(c echo.Context, authorization string) error {
	scheme, value, _ := strings.Cut(authorization, " ")
	value = strings.TrimSpace(value)
	switch strings.ToLower(scheme) {
	case "bearer":
		if auth.ValidSessionToken(value) {
			user, sess, err := rs.authenticator.AuthenticateSession(value, c.RealIP())
			if err != nil {
				return responded(rs.unauthorized(c))
			}
			c.Set(contextKeyUser, user)
			c.Set(contextKeySession, sess)
			c.Set(contextKeyAuthMethod, AuthMethodSession)
			return nil
		}
		return responded(rs.unauthorized(c))
	case "basic":
		if !rs.authenticator.LegacyBasicAuth() {
			return responded(rs.unauthorized(c))
		}
		username, password, ok := c.Request().BasicAuth()
		if !ok {
			return responded(rs.unauthorized(c))
		}
		user := rs.authenticator.AuthenticateBasic(username, password)
		if user == nil {
			// failed attempts count against the per-IP limit
			if err := rs.limitIP(c); err != nil {
				return err
			}
			return responded(rs.unauthorized(c))
		}
		c.Set(contextKeyUser, user)
		c.Set(contextKeyAuthMethod, AuthMethodBasic)
		return nil
	}
	return responded(rs.unauthorized(c))
}

// authorize applies the route group rules of docs/auth.md section 4
func (rs *RestService) authorize(c echo.Context, user *auth.User) error {
	req := c.Request()
	path := req.URL.Path
	method := authMethodFromContext(c)
	isAccount := strings.HasPrefix(path, "/account/")
	isAdmin := strings.HasPrefix(path, "/admin/")
	needsSession := isAccount || isAdmin || path == "/auth/logout/" || strings.HasPrefix(path, "/auth/reauth/")

	if needsSession && method != AuthMethodSession {
		return responded(apiError(c, http.StatusForbidden, CodeForbidden, "this endpoint needs a session (log in with a password or passkey)", nil))
	}
	if method == AuthMethodSession && rs.authenticator.SecondFactorSetupRequired(user) && !isRestrictedAllowed(req.Method, path) {
		return responded(apiError(c, http.StatusForbidden, CodeSecondFactorSetupRequired, "set up a second factor (TOTP or a passkey) first", nil))
	}
	if isAdmin && !user.IsAdmin() {
		return responded(apiError(c, http.StatusForbidden, CodeForbidden, "only admins may do this", nil))
	}
	if !user.CanWrite() && !isReadOnlyMethod(req.Method) && !isAccount && !strings.HasPrefix(path, "/auth/") {
		return responded(apiError(c, http.StatusForbidden, CodeForbidden,
			"user '"+user.Username+"' has the role '"+user.Role+"' and may not change anything", nil))
	}
	return nil
}

// checkCSRF requires X-Requested-With and an allowed Origin on state-changing requests that
// are not authenticated by an Authorization header
func (rs *RestService) checkCSRF(c echo.Context, hasAuthorization bool, hasCookie bool) error {
	req := c.Request()
	if isReadOnlyMethod(req.Method) || hasAuthorization {
		return nil
	}
	if req.Header.Get("X-Requested-With") == "" {
		return responded(apiError(c, http.StatusForbidden, CodeCSRFCheckFailed, "the header X-Requested-With is required", nil))
	}
	origin := req.Header.Get(echo.HeaderOrigin)
	if origin == "" {
		if hasCookie {
			return responded(apiError(c, http.StatusForbidden, CodeOriginNotAllowed, "the Origin header is required with the session cookie", nil))
		}
		return nil
	}
	if !rs.originAllowed(c, origin) {
		return responded(apiError(c, http.StatusForbidden, CodeOriginNotAllowed, "the origin "+origin+" is not allowed", nil))
	}
	return nil
}

// originAllowed reports whether an Origin is the backend's own origin or a configured one.
// An empty origin (native clients) is allowed.
func (rs *RestService) originAllowed(c echo.Context, origin string) bool {
	if origin == "" {
		return true
	}
	if strings.EqualFold(origin, rs.ownOrigin(c)) {
		return true
	}
	for _, allowed := range rs.allowedOrigins {
		if strings.EqualFold(strings.TrimRight(allowed, "/"), origin) {
			return true
		}
	}
	return false
}

func (rs *RestService) ownOrigin(c echo.Context) string {
	scheme := "http"
	if rs.isHTTPS(c) {
		scheme = "https"
	}
	return scheme + "://" + c.Request().Host
}

// isHTTPS reports whether the client uses HTTPS: directly, or through a trusted proxy
func (rs *RestService) isHTTPS(c echo.Context) bool {
	req := c.Request()
	if req.TLS != nil {
		return true
	}
	return rs.fromTrustedProxy(req) && strings.EqualFold(req.Header.Get(echo.HeaderXForwardedProto), "https")
}

func (rs *RestService) fromTrustedProxy(req *http.Request) bool {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range rs.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// cookieSecure decides the Secure flag (and the cookie name) of the session cookie
func (rs *RestService) cookieSecure(c echo.Context) bool {
	switch rs.authenticator.CookieSecure() {
	case auth.CookieSecureAlways:
		return true
	case auth.CookieSecureNever:
		return false
	}
	return rs.isHTTPS(c)
}

// sessionCookie returns the session token of the request cookie, or ""
func (rs *RestService) sessionCookie(c echo.Context) string {
	for _, name := range []string{cookieNameSecure, cookieNameInsecure} {
		if ck, err := c.Cookie(name); err == nil && ck.Value != "" {
			return ck.Value
		}
	}
	return ""
}

func (rs *RestService) setSessionCookie(c echo.Context, token string, expires time.Time) {
	secure := rs.cookieSecure(c)
	name := cookieNameInsecure
	if secure {
		name = cookieNameSecure
	}
	maxAge := int(time.Until(expires).Seconds())
	// Secure is off only for plain HTTP (development), see server.auth.cookieSecure
	c.SetCookie(&http.Cookie{ //nolint:gosec // see above
		Name: name, Value: token, Path: "/", MaxAge: maxAge, Expires: expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func (rs *RestService) clearSessionCookie(c echo.Context) {
	for _, name := range []string{cookieNameSecure, cookieNameInsecure} {
		if _, err := c.Cookie(name); err == nil {
			c.SetCookie(&http.Cookie{ //nolint:gosec // clears a cookie
				Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
				Secure: name == cookieNameSecure, SameSite: http.SameSiteLaxMode,
			})
		}
	}
}

// unauthorized writes a 401 response. With legacy basic auth, requests without
// "X-Requested-With" (not browser scripts) get a WWW-Authenticate header.
func (rs *RestService) unauthorized(c echo.Context) error {
	if rs.authenticator.LegacyBasicAuth() && c.Request().Header.Get("X-Requested-With") == "" {
		c.Response().Header().Set(echo.HeaderWWWAuthenticate, `Basic realm="MkDocsEditor", charset="UTF-8"`)
	}
	return apiError(c, http.StatusUnauthorized, CodeUnauthenticated, "missing or invalid credentials", nil)
}

// requestInfo describes the client for audit events and rate limits
func requestInfo(c echo.Context) auth.RequestInfo {
	return auth.RequestInfo{IP: c.RealIP(), UserAgent: c.Request().UserAgent()}
}

// limitIP applies the per-IP rate limit of the login endpoints
func (rs *RestService) limitIP(c echo.Context) error {
	if err := rs.authenticator.AllowIP(c.RealIP()); err != nil {
		return responded(rs.authError(c, err))
	}
	return nil
}

// authError maps errors of the auth package to responses
func (rs *RestService) authError(c echo.Context, err error) error {
	var rl *auth.RateLimitError
	var weak *auth.WeakPasswordError
	switch {
	case errors.As(err, &rl):
		c.Response().Header().Set("Retry-After", itoa(rl.Seconds()))
		return apiError(c, http.StatusTooManyRequests, CodeRateLimited, err.Error(), map[string]any{"retryAfter": rl.Seconds()})
	case errors.As(err, &weak):
		return apiError(c, http.StatusBadRequest, CodeWeakPassword, err.Error(), map[string]any{
			"reason": weak.Reason, "minLength": auth.PasswordMinLength, "maxLength": auth.PasswordMaxLength})
	case errors.Is(err, auth.ErrInvalidCredentials):
		return apiError(c, http.StatusUnauthorized, CodeInvalidCredentials, err.Error(), nil)
	case errors.Is(err, auth.ErrAccountDisabled):
		return apiError(c, http.StatusForbidden, CodeAccountDisabled, err.Error(), nil)
	case errors.Is(err, auth.ErrChallengeExpired):
		return apiError(c, http.StatusUnauthorized, CodeChallengeExpired, err.Error(), nil)
	case errors.Is(err, auth.ErrInvalidLink):
		return apiError(c, http.StatusBadRequest, CodeInvalidLink, err.Error(), nil)
	case errors.Is(err, auth.ErrInvalidUsername):
		return apiError(c, http.StatusBadRequest, CodeInvalidUsername, err.Error(), nil)
	case errors.Is(err, auth.ErrUsernameTaken):
		return apiError(c, http.StatusConflict, CodeUsernameTaken, err.Error(), nil)
	case errors.Is(err, auth.ErrInvalidDisplayName), errors.Is(err, auth.ErrInvalidNote), errors.Is(err, auth.ErrInvalidColor), errors.Is(err, auth.ErrInvalidEmail):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), nil)
	case errors.Is(err, auth.ErrNotFound):
		return apiError(c, http.StatusNotFound, CodeNotFound, "not found", nil)
	case errors.Is(err, auth.ErrUnauthenticated):
		return rs.unauthorized(c)
	}
	c.Logger().Errorf("auth: %v", err)
	return apiError(c, http.StatusInternalServerError, CodeInternal, "internal error", nil)
}
