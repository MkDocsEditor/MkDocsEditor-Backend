package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v5"
)

const (
	// EndpointPathForward is the forwardAuth endpoint of a reverse proxy, see docs/auth.md
	// section 4
	EndpointPathForward = "/auth/forward/"
	// FeatureForwardAuth means GET /auth/forward/ exists
	FeatureForwardAuth = "forward-auth"

	headerForwardedURI    = "X-Forwarded-Uri"
	headerForwardedMethod = "X-Forwarded-Method"
	headerAuthUser        = "X-Auth-User"
	// forwardLoginPath is the login page of the web client on the protected host
	forwardLoginPath = "/login"
	maxForwardNext   = 2048
)

// GET /auth/forward/: 200 for an account that may read, else a redirect to the login page of
// the web client (browsers) or 401 (everything else)
func (rs *RestService) forwardAuth(c *echo.Context) error {
	user := UserFromContext(c)
	setupRequired := user != nil && authMethodFromContext(c) == AuthMethodSession && rs.authenticator.SecondFactorSetupRequired(user)
	if user != nil && !setupRequired {
		c.Response().Header().Set(headerAuthUser, user.Username)
		return c.NoContent(http.StatusOK)
	}
	req := c.Request()
	method, uri, accept := req.Method, "", req.Header.Get(echo.HeaderAccept)
	if rs.fromTrustedProxy(req) {
		if m := req.Header.Get(headerForwardedMethod); m != "" {
			method = m
		}
		uri = req.Header.Get(headerForwardedURI)
	}
	if (method != http.MethodGet && method != http.MethodHead) || !strings.Contains(accept, "text/html") {
		return c.String(http.StatusUnauthorized, "login required\n")
	}
	location := forwardLoginPath
	if next, ok := safeNext(uri); ok {
		location += "?next=" + url.QueryEscape(next)
	}
	return c.Redirect(http.StatusFound, location)
}

// safeNext accepts a path with query on the same host: it starts with one "/", has no scheme
// or host and no backslash or control characters
func safeNext(uri string) (string, bool) {
	if uri == "" || len(uri) > maxForwardNext || !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") ||
		strings.ContainsAny(uri, "\\\r\n\t") {
		return "", false
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "", false
	}
	return uri, true
}
