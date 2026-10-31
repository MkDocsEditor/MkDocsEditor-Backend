package backend

import (
	"encoding/json"
	"net/http"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v4"
)

// request and response bodies of the passkey endpoints, see docs/auth.md section 8
type (
	PasskeyLoginBeginRequest struct {
		Challenge string `json:"challenge"`
	}

	// CeremonyResponse holds the options for navigator.credentials or Credential Manager
	CeremonyResponse struct {
		CeremonyID string `json:"ceremonyId"`
		PublicKey  any    `json:"publicKey"`
	}

	PasskeyLoginFinishRequest struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
		sessionRequestFields
	}

	LinkPasskeyBeginRequest struct {
		Token       string `json:"token"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	}

	LinkPasskeyFinishRequest struct {
		CeremonyID  string          `json:"ceremonyId"`
		Credential  json.RawMessage `json:"credential"`
		PasskeyName string          `json:"passkeyName"`
		sessionRequestFields
	}

	PasskeyFinishRequest struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
		Name       string          `json:"name"`
	}

	PasskeyRenameRequest struct {
		Name string `json:"name"`
	}
)

func (rs *RestService) writeCeremony(c echo.Context, id string, options any, err error) error {
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &CeremonyResponse{CeremonyID: id, PublicKey: options}, indentationChar)
}

func requireCredential(c echo.Context, id string, credential json.RawMessage) error {
	if id == "" || len(credential) == 0 || string(credential) == "null" {
		return responded(apiError(c, http.StatusBadRequest, CodeBadRequest, "ceremonyId and credential are required", nil))
	}
	return nil
}

// POST /auth/passkey/login/begin/
func (rs *RestService) passkeyLoginBegin(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r PasskeyLoginBeginRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	id, options, err := rs.authenticator.BeginPasskeyLogin(r.Challenge)
	return rs.writeCeremony(c, id, options, err)
}

// POST /auth/passkey/login/finish/
func (rs *RestService) passkeyLoginFinish(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r PasskeyLoginFinishRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if err := requireCredential(c, r.CeremonyID, r.Credential); err != nil {
		return err
	}
	result, err := rs.authenticator.FinishPasskeyLogin(r.CeremonyID, r.Credential, rs.sessionParams(c, r.sessionRequestFields))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeLoginResult(c, result)
}

// POST /auth/link/passkey/begin/
func (rs *RestService) linkPasskeyBegin(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r LinkPasskeyBeginRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	id, options, err := rs.authenticator.BeginLinkPasskey(r.Token, auth.NewAccount{Username: r.Username, DisplayName: r.DisplayName})
	return rs.writeCeremony(c, id, options, err)
}

// POST /auth/link/passkey/finish/
func (rs *RestService) linkPasskeyFinish(c echo.Context) error {
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r LinkPasskeyFinishRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if err := requireCredential(c, r.CeremonyID, r.Credential); err != nil {
		return err
	}
	result, err := rs.authenticator.FinishLinkPasskey(r.CeremonyID, r.Credential, r.PasskeyName, rs.sessionParams(c, r.sessionRequestFields))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeLoginResult(c, result)
}

// POST /auth/reauth/passkey/begin/
func (rs *RestService) reauthPasskeyBegin(c echo.Context) error {
	id, options, err := rs.authenticator.BeginReauthPasskey(UserFromContext(c), SessionFromContext(c))
	return rs.writeCeremony(c, id, options, err)
}

// POST /auth/reauth/passkey/finish/
func (rs *RestService) reauthPasskeyFinish(c echo.Context) error {
	var r PasskeyFinishRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if err := requireCredential(c, r.CeremonyID, r.Credential); err != nil {
		return err
	}
	until, err := rs.authenticator.FinishReauthPasskey(UserFromContext(c), SessionFromContext(c), r.CeremonyID, r.Credential, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, &ReauthResponse{ReauthenticatedUntil: until}, indentationChar)
}

// GET /account/passkeys/
func (rs *RestService) listPasskeys(c echo.Context) error {
	passkeys, err := rs.authenticator.Passkeys(UserFromContext(c).ID)
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, passkeys, indentationChar)
}

// POST /account/passkeys/begin/
func (rs *RestService) addPasskeyBegin(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	id, options, err := rs.authenticator.BeginAddPasskey(UserFromContext(c))
	return rs.writeCeremony(c, id, options, err)
}

// POST /account/passkeys/finish/
func (rs *RestService) addPasskeyFinish(c echo.Context) error {
	var r PasskeyFinishRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if err := requireCredential(c, r.CeremonyID, r.Credential); err != nil {
		return err
	}
	p, err := rs.authenticator.FinishAddPasskey(UserFromContext(c), r.CeremonyID, r.Credential, r.Name, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, p, indentationChar)
}

// PATCH /account/passkeys/{id}/
func (rs *RestService) renamePasskey(c echo.Context) error {
	var r PasskeyRenameRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	p, err := rs.authenticator.RenamePasskey(UserFromContext(c), c.Param(urlParamId), r.Name, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, p, indentationChar)
}

// DELETE /account/passkeys/{id}/
func (rs *RestService) deletePasskey(c echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	if err := rs.authenticator.DeletePasskey(UserFromContext(c), c.Param(urlParamId), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// GET /.well-known/assetlinks.json
func (rs *RestService) assetLinks(c echo.Context) error {
	data := rs.authenticator.AssetLinks()
	if data == nil {
		return apiError(c, http.StatusNotFound, CodeNotFound, "not found", nil)
	}
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, data)
}
