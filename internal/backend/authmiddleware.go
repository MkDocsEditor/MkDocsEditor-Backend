package backend

import (
	"net/http"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

const (
	contextKeyUser = "mkdocseditor.user"

	EndpointPathWhoAmI   = "/auth/whoami/"
	EndpointPathWsTicket = "/auth/ws-ticket/"
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

// isWebsocketPath reports whether the request path is a websocket endpoint.
// Websockets may also authenticate after the upgrade (ticket in the hello message).
func isWebsocketPath(path string) bool {
	return strings.HasSuffix(path, "/ws/")
}

func isReadOnlyMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// authMiddleware authenticates every request with HTTP basic auth against the configured users
// and enforces the role of the user.
func (rs *RestService) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		if req.Method == http.MethodOptions || req.URL.Path == EndpointPathAlive {
			return next(c)
		}

		if !rs.authenticator.Enabled() {
			c.Set(contextKeyUser, auth.Anonymous())
			return next(c)
		}

		username, password, hasBasic := req.BasicAuth()
		if hasBasic {
			user := rs.authenticator.Authenticate(username, password)
			if user == nil {
				return rs.unauthorized(c)
			}
			c.Set(contextKeyUser, user)
		} else if !isWebsocketPath(req.URL.Path) {
			return rs.unauthorized(c)
		}

		if user := UserFromContext(c); user != nil && !user.CanWrite() &&
			!isReadOnlyMethod(req.Method) && req.URL.Path != EndpointPathWsTicket {
			return c.JSONPretty(http.StatusForbidden, &ErrorResult{
				Name:    "Forbidden",
				Message: "user '" + user.Username + "' has the role '" + user.Role + "' and may not change anything",
			}, indentationChar)
		}
		return next(c)
	}
}

// unauthorized writes a 401 response. Requests sent with "X-Requested-With" (browser scripts)
// get no WWW-Authenticate header, so the browser does not show its own login dialog.
func (rs *RestService) unauthorized(c echo.Context) error {
	if c.Request().Header.Get("X-Requested-With") == "" {
		c.Response().Header().Set(echo.HeaderWWWAuthenticate, `Basic realm="MkDocsEditor", charset="UTF-8"`)
	}
	return c.JSONPretty(http.StatusUnauthorized, &ErrorResult{
		Name:    "Unauthorized",
		Message: "missing or invalid credentials",
	}, indentationChar)
}

// returns the authenticated user
func (rs *RestService) whoAmI(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, UserFromContext(c), indentationChar)
}

// issues a single-use ticket for a websocket connection
func (rs *RestService) issueWsTicket(c echo.Context) error {
	user := UserFromContext(c)
	if user == nil {
		return rs.unauthorized(c)
	}
	value, expires, err := rs.authenticator.IssueTicket(user)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &TicketResponse{Ticket: value, ExpiresAt: expires}, indentationChar)
}
