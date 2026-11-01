package backend

import (
	"net/http"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

// request and response bodies of the account endpoints, see docs/auth.md section 9
type (
	SecondFactorRequest struct {
		Challenge    string `json:"challenge"`
		Code         string `json:"code"`
		RecoveryCode string `json:"recoveryCode"`
		sessionRequestFields
	}

	ReauthRequest struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}

	ReauthResponse struct {
		ReauthenticatedUntil time.Time `json:"reauthenticatedUntil"`
	}

	AccountResponse struct {
		UserView
		Email                string     `json:"email"`
		CreatedAt            time.Time  `json:"createdAt"`
		HasPassword          bool       `json:"hasPassword"`
		TOTPEnabled          bool       `json:"totpEnabled"`
		RecoveryCodesLeft    int        `json:"recoveryCodesLeft"`
		PasskeyCount         int        `json:"passkeyCount"`
		Require2FA           bool       `json:"require2FA"`
		ReauthenticatedUntil *time.Time `json:"reauthenticatedUntil"`
	}

	AccountUpdateRequest struct {
		Username    *string `json:"username"`
		DisplayName *string `json:"displayName"`
		Color       *string `json:"color"`
		Email       *string `json:"email"`
	}

	PasswordChangeRequest struct {
		CurrentPassword *string `json:"currentPassword"`
		NewPassword     string  `json:"newPassword"`
	}

	TOTPConfirmRequest struct {
		Code string `json:"code"`
	}

	RecoveryCodesResponse struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
)

// requireReauth answers 403 reauth-required unless the session authenticated recently
func (rs *RestService) requireReauth(c echo.Context) error {
	if !rs.authenticator.RecentlyAuthenticated(SessionFromContext(c)) {
		return responded(apiError(c, http.StatusForbidden, CodeReauthRequired, "confirm your identity again (POST /auth/reauth/)", nil))
	}
	return nil
}

// POST /auth/login/second-factor/
func (rs *RestService) loginSecondFactor(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r SecondFactorRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Challenge == "" || (r.Code == "") == (r.RecoveryCode == "") {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "challenge and either code or recoveryCode are required", nil)
	}
	result, err := rs.authenticator.LoginSecondFactor(r.Challenge, r.Code, r.RecoveryCode, rs.sessionParams(c, r.sessionRequestFields))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeLoginResult(c, result)
}

// POST /auth/reauth/
func (rs *RestService) reauth(c echo.Context) error {
	var r ReauthRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Password == "" && r.Code == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "password or code is required", nil)
	}
	until, err := rs.authenticator.Reauthenticate(UserFromContext(c), SessionFromContext(c), r.Password, r.Code, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &ReauthResponse{ReauthenticatedUntil: until}, jsonIndent)
}

// GET /account/
func (rs *RestService) getAccount(c echo.Context) error {
	return rs.writeAccount(c, UserFromContext(c))
}

func (rs *RestService) writeAccount(c echo.Context, u *auth.User) error {
	d, err := rs.authenticator.AccountDetails(u.ID)
	if err != nil {
		return rs.authError(c, err)
	}
	resp := &AccountResponse{UserView: *rs.userView(u), Email: d.Email, CreatedAt: d.CreatedAt, HasPassword: d.HasPassword,
		TOTPEnabled: d.TOTPEnabled, RecoveryCodesLeft: d.RecoveryCodesLeft, PasskeyCount: d.PasskeyCount,
		Require2FA: rs.authenticator.Require2FA()}
	if sess := SessionFromContext(c); rs.authenticator.RecentlyAuthenticated(sess) {
		until := auth.ReauthenticatedUntil(sess)
		resp.ReauthenticatedUntil = &until
	}
	return c.JSONPretty(http.StatusOK, resp, jsonIndent)
}

// PATCH /account/
func (rs *RestService) updateAccount(c echo.Context) error {
	var r AccountUpdateRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	user := UserFromContext(c)
	if r.Username != nil && *r.Username != user.Username {
		if err := rs.requireReauth(c); err != nil {
			return err
		}
	}
	u, err := rs.authenticator.UpdateProfile(user.ID, auth.ProfileUpdate{Username: r.Username, DisplayName: r.DisplayName, Color: r.Color, Email: r.Email},
		user, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeAccount(c, u)
}

// PUT /account/password/
func (rs *RestService) changePassword(c echo.Context) error {
	var r PasswordChangeRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.NewPassword == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "newPassword is required", nil)
	}
	if r.CurrentPassword == nil {
		if err := rs.requireReauth(c); err != nil {
			return err
		}
	}
	if err := rs.authenticator.ChangePassword(UserFromContext(c), SessionFromContext(c), r.CurrentPassword, r.NewPassword, requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// DELETE /account/password/
func (rs *RestService) removePassword(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	if err := rs.authenticator.RemovePassword(UserFromContext(c), SessionFromContext(c), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /account/totp/
func (rs *RestService) beginTOTP(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	e, err := rs.authenticator.BeginTOTP(UserFromContext(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, e, jsonIndent)
}

// POST /account/totp/confirm/
func (rs *RestService) confirmTOTP(c echo.Context) error {
	var r TOTPConfirmRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	codes, err := rs.authenticator.ConfirmTOTP(UserFromContext(c), r.Code, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &RecoveryCodesResponse{RecoveryCodes: codes}, jsonIndent)
}

// DELETE /account/totp/
func (rs *RestService) disableTOTP(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	if err := rs.authenticator.DisableTOTP(UserFromContext(c), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /account/recovery-codes/
func (rs *RestService) regenerateRecoveryCodes(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	codes, err := rs.authenticator.RegenerateRecoveryCodes(UserFromContext(c), requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &RecoveryCodesResponse{RecoveryCodes: codes}, jsonIndent)
}
