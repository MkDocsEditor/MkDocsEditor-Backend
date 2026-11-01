package backend

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

// request and response bodies of the account endpoints, see docs/auth.md sections 8 and 9
type (
	sessionRequestFields struct {
		SessionType string `json:"sessionType"`
		DeviceName  string `json:"deviceName"`
	}

	LoginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
		sessionRequestFields
	}

	LinkTokenRequest struct {
		Token string `json:"token"`
	}

	LinkRedeemRequest struct {
		Token       string `json:"token"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
		sessionRequestFields
	}

	// UserView is a user with the state of its second factor setup
	UserView struct {
		auth.User
		SecondFactorSetupRequired bool `json:"secondFactorSetupRequired"`
		PasskeyCount              int  `json:"passkeyCount"`
	}

	SessionResponse struct {
		Status  string        `json:"status"`
		User    *UserView     `json:"user"`
		Session *auth.Session `json:"session"`
		Token   string        `json:"token,omitempty"`
	}

	SecondFactorRequiredResponse struct {
		Status    string    `json:"status"`
		Challenge string    `json:"challenge"`
		Methods   []string  `json:"methods"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	WhoAmIResponse struct {
		UserView
		AuthMethod string `json:"authMethod"`
		SessionID  string `json:"sessionId,omitempty"`
	}

	PasswordPolicy struct {
		MinLength int `json:"minLength"`
		MaxLength int `json:"maxLength"`
	}

	PasskeyStatus struct {
		Enabled bool   `json:"enabled"`
		RPID    string `json:"rpId,omitempty"`
	}

	AuthStatusResponse struct {
		SetupRequired   bool           `json:"setupRequired"`
		AuthDisabled    bool           `json:"authDisabled"`
		Passkeys        PasskeyStatus  `json:"passkeys"`
		PasswordPolicy  PasswordPolicy `json:"passwordPolicy"`
		LegacyBasicAuth bool           `json:"legacyBasicAuth"`
	}

	LinkInspectResponse struct {
		Kind               string    `json:"kind"`
		Role               string    `json:"role,omitempty"`
		Note               string    `json:"note,omitempty"`
		Username           string    `json:"username,omitempty"`
		DisplayName        string    `json:"displayName,omitempty"`
		ResetSecondFactors bool      `json:"resetSecondFactors,omitempty"`
		ExpiresAt          time.Time `json:"expiresAt"`
	}

	CreateInviteRequest struct {
		Role           string `json:"role"`
		ExpiresInHours int    `json:"expiresInHours"`
		Note           string `json:"note"`
	}

	CreatedLinkResponse struct {
		Invite *auth.Link `json:"invite,omitempty"`
		Link   *auth.Link `json:"link,omitempty"`
		Token  string     `json:"token"`
		URL    *string    `json:"url"`
	}

	RevokedResponse struct {
		Revoked int `json:"revoked"`
	}
)

// decodeJSON reads a JSON request body of at most 64 KiB. An empty body is an empty object.
func decodeJSON(c echo.Context, v any) error {
	data, err := io.ReadAll(io.LimitReader(c.Request().Body, 64<<10+1))
	if err != nil {
		return responded(apiError(c, http.StatusBadRequest, CodeBadRequest, "unable to read the request body", nil))
	}
	if len(data) > 64<<10 {
		return responded(apiError(c, http.StatusRequestEntityTooLarge, CodeBadRequest, "the request body is too large", nil))
	}
	if len(data) == 0 {
		return nil
	}
	if err = json.Unmarshal(data, v); err != nil {
		return responded(apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid JSON: "+err.Error(), nil))
	}
	return nil
}

func (rs *RestService) sessionParams(c echo.Context, f sessionRequestFields) auth.SessionParams {
	return auth.SessionParams{Type: f.SessionType, DeviceName: f.DeviceName, RequestInfo: requestInfo(c)}
}

func (rs *RestService) userView(u *auth.User) *UserView {
	v := &UserView{User: *u, SecondFactorSetupRequired: rs.authenticator.SecondFactorSetupRequired(u)}
	if u.ID != auth.AnonymousID {
		v.PasskeyCount = rs.authenticator.PasskeyCount(u.ID)
	}
	return v
}

// writeLoginResult answers with a session (setting the cookie for cookie sessions) or the
// second factor challenge
func (rs *RestService) writeLoginResult(c echo.Context, r *auth.LoginResult) error {
	if r.Session == nil {
		return c.JSONPretty(http.StatusOK, &SecondFactorRequiredResponse{Status: "second-factor-required",
			Challenge: r.Challenge, Methods: r.Methods, ExpiresAt: r.ExpiresAt}, indentationChar)
	}
	resp := &SessionResponse{Status: "ok", User: rs.userView(r.User), Session: r.Session}
	if r.Session.Type == auth.SessionBearer {
		resp.Token = r.Token
	} else {
		rs.setSessionCookie(c, r.Token, r.Session.ExpiresAt)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, resp, indentationChar)
}

// GET /auth/status/
func (rs *RestService) authStatus(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, &AuthStatusResponse{
		SetupRequired:   rs.authenticator.SetupRequired(),
		AuthDisabled:    !rs.authenticator.Enabled(),
		Passkeys:        PasskeyStatus{Enabled: rs.authenticator.PasskeysEnabled(), RPID: rs.authenticator.RPID()},
		PasswordPolicy:  PasswordPolicy{MinLength: auth.PasswordMinLength, MaxLength: auth.PasswordMaxLength},
		LegacyBasicAuth: rs.authenticator.LegacyBasicAuth(),
	}, indentationChar)
}

// POST /auth/login/
func (rs *RestService) login(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r LoginRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Username == "" || r.Password == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "username and password are required", nil)
	}
	result, err := rs.authenticator.LoginPassword(r.Username, r.Password, rs.sessionParams(c, r.sessionRequestFields))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeLoginResult(c, result)
}

// POST /auth/logout/
func (rs *RestService) logout(c echo.Context) error {
	if sess := SessionFromContext(c); sess != nil {
		rs.authenticator.Logout(sess, requestInfo(c))
	}
	rs.clearSessionCookie(c)
	return c.NoContent(http.StatusNoContent)
}

// GET /auth/whoami/
func (rs *RestService) whoAmI(c echo.Context) error {
	user := UserFromContext(c)
	resp := &WhoAmIResponse{UserView: *rs.userView(user), AuthMethod: authMethodFromContext(c)}
	if sess := SessionFromContext(c); sess != nil {
		resp.SessionID = sess.ID
	}
	return c.JSONPretty(http.StatusOK, resp, indentationChar)
}

// POST /auth/ws-ticket/ issues a single-use ticket for a websocket connection
func (rs *RestService) issueWsTicket(c echo.Context) error {
	user := UserFromContext(c)
	if user == nil {
		return rs.unauthorized(c)
	}
	sessionID := ""
	if sess := SessionFromContext(c); sess != nil {
		sessionID = sess.ID
	}
	value, expires, err := rs.authenticator.IssueTicket(user, sessionID)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &TicketResponse{Ticket: value, ExpiresAt: expires}, indentationChar)
}

// POST /auth/link/inspect/
func (rs *RestService) inspectLink(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r LinkTokenRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	l, target, err := rs.authenticator.InspectLink(r.Token)
	if err != nil {
		return rs.authError(c, err)
	}
	resp := &LinkInspectResponse{Kind: l.Kind, ExpiresAt: l.ExpiresAt}
	if l.Kind == auth.LinkReset {
		resp.Username, resp.DisplayName, resp.ResetSecondFactors = target.Username, target.DisplayName, l.ResetSecondFactors
	} else {
		resp.Role, resp.Note = l.Role, l.Note
	}
	return c.JSONPretty(http.StatusOK, resp, indentationChar)
}

// POST /auth/link/redeem/
func (rs *RestService) redeemLink(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r LinkRedeemRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Password == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "password is required", nil)
	}
	result, err := rs.authenticator.RedeemLink(r.Token, auth.NewAccount{Username: r.Username, DisplayName: r.DisplayName, Password: r.Password},
		rs.sessionParams(c, r.sessionRequestFields))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeLoginResult(c, result)
}

// GET /account/sessions/
func (rs *RestService) accountSessions(c echo.Context) error {
	return rs.writeSessions(c, UserFromContext(c).ID)
}

func (rs *RestService) writeSessions(c echo.Context, accountID string) error {
	sessions, err := rs.authenticator.Sessions(accountID)
	if err != nil {
		return rs.authError(c, err)
	}
	if cur := SessionFromContext(c); cur != nil {
		for i, s := range sessions {
			if s.ID == cur.ID {
				s.Current = true
				// current session first
				copy(sessions[1:i+1], sessions[:i])
				sessions[0] = s
				break
			}
		}
	}
	return c.JSONPretty(http.StatusOK, sessions, indentationChar)
}

// DELETE /account/sessions/{id}/
func (rs *RestService) revokeAccountSession(c echo.Context) error {
	user := UserFromContext(c)
	id := c.Param(urlParamID)
	if err := rs.authenticator.RevokeSession(user.ID, id, user, requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	if cur := SessionFromContext(c); cur != nil && cur.ID == id {
		rs.clearSessionCookie(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /account/sessions/revoke-others/
func (rs *RestService) revokeOtherSessions(c echo.Context) error {
	user := UserFromContext(c)
	n, err := rs.authenticator.RevokeSessions(user.ID, SessionFromContext(c).ID, user, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &RevokedResponse{Revoked: n}, indentationChar)
}

// GET /admin/invites/
func (rs *RestService) listInvites(c echo.Context) error {
	return rs.listLinks(c, auth.LinkInvite)
}

func (rs *RestService) listLinks(c echo.Context, kind string) error {
	links, err := rs.authenticator.Links(kind, c.QueryParam("status"))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, links, indentationChar)
}

// POST /admin/invites/
func (rs *RestService) createInvite(c echo.Context) error {
	var r CreateInviteRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.ExpiresInHours < 0 || r.ExpiresInHours > int(auth.MaxInviteLifetime/time.Hour) {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "expiresInHours must be between 1 and 720", nil)
	}
	if auth.ValidateRole(r.Role) != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "role must be admin, editor or reader", nil)
	}
	l, token, err := rs.authenticator.CreateInvite(UserFromContext(c), r.Role, r.Note, time.Duration(r.ExpiresInHours)*time.Hour, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &CreatedLinkResponse{Invite: l, Token: token, URL: rs.authenticator.LinkURL(auth.LinkInvite, token)}, indentationChar)
}

// DELETE /admin/invites/{id}/
func (rs *RestService) revokeInvite(c echo.Context) error {
	if err := rs.authenticator.RevokeLink(auth.LinkInvite, c.Param(urlParamID), UserFromContext(c), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
