package backend

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

// request and response bodies of the admin and app token endpoints, see docs/auth.md section 9
type (
	AdminUserUpdateRequest struct {
		Role        *string `json:"role"`
		Status      *string `json:"status"`
		Username    *string `json:"username"`
		DisplayName *string `json:"displayName"`
		Color       *string `json:"color"`
		Email       *string `json:"email"`
	}

	ResetLinkRequest struct {
		ExpiresInMinutes   int  `json:"expiresInMinutes"`
		ResetSecondFactors bool `json:"resetSecondFactors"`
	}

	SettingsUpdateRequest struct {
		Require2FA *bool `json:"require2FA"`
	}

	AuditResponse struct {
		Events []*auth.AuditEntry `json:"events"`
		Next   *int64             `json:"next"`
	}

	AppTokenRequest struct {
		Name          string `json:"name"`
		Scope         string `json:"scope"`
		ExpiresInDays int    `json:"expiresInDays"`
	}

	CreatedAppTokenResponse struct {
		AppToken *auth.AppToken `json:"appToken"`
		Token    string         `json:"token"`
	}
)

// GET /admin/users/
func (rs *RestService) adminListUsers(c echo.Context) error {
	var statuses []string
	for _, s := range strings.Split(c.QueryParam("status"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			statuses = append(statuses, s)
		}
	}
	users, err := rs.authenticator.AdminUsers(statuses)
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, users, indentationChar)
}

// PATCH /admin/users/{id}/
func (rs *RestService) adminUpdateUser(c echo.Context) error {
	var r AdminUserUpdateRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Role != nil || r.Status != nil {
		if err := rs.requireReauth(c); err != nil {
			return err
		}
	}
	if r.Role != nil && auth.ValidateRole(*r.Role) != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "role must be admin, editor or reader", map[string]any{"field": "role"})
	}
	if r.Status != nil && *r.Status != auth.StatusActive && *r.Status != auth.StatusDisabled {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "status must be active or disabled", map[string]any{"field": "status"})
	}
	u, err := rs.authenticator.UpdateUser(UserFromContext(c), c.Param(urlParamId), auth.AdminUpdate{Role: r.Role, Status: r.Status,
		ProfileUpdate: auth.ProfileUpdate{Username: r.Username, DisplayName: r.DisplayName, Color: r.Color, Email: r.Email}}, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, u, indentationChar)
}

// DELETE /admin/users/{id}/
func (rs *RestService) adminDeleteUser(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	if err := rs.authenticator.DeleteUser(UserFromContext(c), c.Param(urlParamId), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// GET /admin/users/{id}/sessions/
func (rs *RestService) adminUserSessions(c echo.Context) error {
	if _, err := rs.authenticator.AdminUser(c.Param(urlParamId)); err != nil {
		return rs.authError(c, err)
	}
	return rs.writeSessions(c, c.Param(urlParamId))
}

// POST /admin/users/{id}/sessions/revoke/
func (rs *RestService) adminRevokeUserSessions(c echo.Context) error {
	id := c.Param(urlParamId)
	if _, err := rs.authenticator.AdminUser(id); err != nil {
		return rs.authError(c, err)
	}
	except := ""
	if cur := SessionFromContext(c); cur != nil && id == UserFromContext(c).ID {
		// revoking one's own sessions keeps the current one
		except = cur.ID
	}
	n, err := rs.authenticator.RevokeSessions(id, except, UserFromContext(c), requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &RevokedResponse{Revoked: n}, indentationChar)
}

// DELETE /admin/sessions/{id}/
func (rs *RestService) adminRevokeSession(c echo.Context) error {
	id := c.Param(urlParamId)
	if err := rs.authenticator.RevokeSession("", id, UserFromContext(c), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	if cur := SessionFromContext(c); cur != nil && cur.ID == id {
		rs.clearSessionCookie(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /admin/users/{id}/reset-link/
func (rs *RestService) adminCreateResetLink(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	var r ResetLinkRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.ExpiresInMinutes < 0 || r.ExpiresInMinutes > int(auth.MaxResetLifetime/time.Minute) {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "expiresInMinutes must be between 1 and 1440", map[string]any{"field": "expiresInMinutes"})
	}
	l, token, err := rs.authenticator.CreateResetLink(UserFromContext(c), c.Param(urlParamId), time.Duration(r.ExpiresInMinutes)*time.Minute,
		r.ResetSecondFactors, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &CreatedLinkResponse{Link: l, Token: token, URL: rs.authenticator.LinkURL(auth.LinkReset, token)}, indentationChar)
}

// GET /admin/reset-links/
func (rs *RestService) adminListResetLinks(c echo.Context) error {
	return rs.listLinks(c, auth.LinkReset)
}

// DELETE /admin/reset-links/{id}/
func (rs *RestService) adminRevokeResetLink(c echo.Context) error {
	if err := rs.authenticator.RevokeLink(auth.LinkReset, c.Param(urlParamId), UserFromContext(c), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// GET /admin/settings/
func (rs *RestService) adminSettings(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.authenticator.Settings(), indentationChar)
}

// PATCH /admin/settings/
func (rs *RestService) adminUpdateSettings(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	var r SettingsUpdateRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Require2FA != nil && *r.Require2FA != rs.authenticator.Require2FA() {
		if err := rs.authenticator.SetRequire2FA(*r.Require2FA, UserFromContext(c), requestInfo(c)); err != nil {
			return rs.authError(c, err)
		}
	}
	return rs.adminSettings(c)
}

// GET /admin/audit/
func (rs *RestService) adminAudit(c echo.Context) error {
	q := auth.AuditQuery{Type: c.QueryParam("type"), AccountID: c.QueryParam("account")}
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return apiError(c, http.StatusBadRequest, CodeBadRequest, "limit must be between 1 and 500", map[string]any{"field": "limit"})
		}
		q.Limit = n
	}
	if v := c.QueryParam("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return apiError(c, http.StatusBadRequest, CodeBadRequest, "before must be an event id", map[string]any{"field": "before"})
		}
		q.Before = n
	}
	events, next, err := rs.authenticator.AuditLog(q)
	if err != nil {
		return rs.authError(c, err)
	}
	resp := &AuditResponse{Events: events}
	if next != 0 {
		resp.Next = &next
	}
	return c.JSONPretty(http.StatusOK, resp, indentationChar)
}

// GET /account/tokens/
func (rs *RestService) listAppTokens(c echo.Context) error {
	tokens, err := rs.authenticator.AppTokens(UserFromContext(c).ID)
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, tokens, indentationChar)
}

// POST /account/tokens/
func (rs *RestService) createAppToken(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	var r AppTokenRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	t, token, err := rs.authenticator.CreateAppToken(UserFromContext(c), r.Name, r.Scope, r.ExpiresInDays, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &CreatedAppTokenResponse{AppToken: t, Token: token}, indentationChar)
}

// DELETE /account/tokens/{id}/
func (rs *RestService) revokeAppToken(c echo.Context) error {
	if err := rs.authenticator.RevokeAppToken(UserFromContext(c), c.Param(urlParamId), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
