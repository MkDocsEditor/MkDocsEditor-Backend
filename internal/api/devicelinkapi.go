package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/labstack/echo/v5"
)

// request and response bodies of server info and device pairing, see docs/auth.md
type (
	ServerInfoResponse struct {
		Name            string        `json:"name"`
		Version         string        `json:"version"`
		ProtocolVersion int           `json:"protocolVersion"`
		BackendURL      *string       `json:"backendUrl"`
		WikiURL         *string       `json:"wikiUrl"`
		WebClientURL    *string       `json:"webClientUrl"`
		SetupRequired   bool          `json:"setupRequired"`
		AuthDisabled    bool          `json:"authDisabled"`
		Passkeys        PasskeyStatus `json:"passkeys"`
		LoginMethods    []string      `json:"loginMethods"`
		Require2FA      bool          `json:"require2FA"`
		Features        []string      `json:"features"`
		// Push is set when the server sends Web Push messages
		Push *PushInfo `json:"push"`
	}

	CreateDeviceLinkRequest struct {
		DeviceName string `json:"deviceName"`
	}

	CreatedDeviceLinkResponse struct {
		DeviceLink *auth.DeviceLink `json:"deviceLink"`
		Token      string           `json:"token"`
		ExpiresAt  time.Time        `json:"expiresAt"`
		ConnectURL *string          `json:"connectUrl"`
	}

	RedeemDeviceLinkRequest struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		sessionRequestFields
	}

	// ConnectPayload is the fragment of a connect link (base64url JSON without padding)
	ConnectPayload struct {
		V          int       `json:"v"`
		BackendURL string    `json:"backendUrl"`
		Username   string    `json:"username"`
		Token      string    `json:"token"`
		ExpiresAt  time.Time `json:"expiresAt"`
	}
)

// ConnectPayloadVersion is the version of the connect link payload
const ConnectPayloadVersion = 1

// FeaturePageMeta means documents in the tree have title and meta, see docs/page-meta.md
const FeaturePageMeta = "page-meta"

// FeatureWikiConfig means the server has GET /wiki/config/, see docs/page-meta.md
const FeatureWikiConfig = "wiki-config"

// FeatureChanges means the server has GET /document/{id}/changes/ and PUT
// /document/{id}/seen/, see docs/sync-protocol.md section 11
const FeatureChanges = "changes"

// FeatureHistory means git is enabled and history entries have summary and agents, with
// format=json for diffs and agent edits in commits of their own, see docs/sync-protocol.md
// section 11
const FeatureHistory = "history"

// FeatureMovedLookup in GET /server-info/ says that the server has GET /moved/?id=…&id=…
const FeatureMovedLookup = "moved-lookup"

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// backendURL is server.backendUrl, else the scheme and host of the request
func (rs *RestService) backendURL(c *echo.Context) string {
	if u := strings.TrimRight(rs.server.BackendURL, "/"); u != "" {
		return u
	}
	return rs.ownOrigin(c) + APIPrefix
}

// instanceName is server.instanceName, else the host of the wiki, of the web client or of the
// request
func (rs *RestService) instanceName(c *echo.Context) string {
	name := strings.TrimSpace(rs.server.InstanceName)
	for _, candidate := range []string{hostOf(strings.TrimRight(rs.server.WikiURL, "/")), hostOf(rs.authenticator.ClientURL())} {
		if name == "" {
			name = candidate
		}
	}
	if name == "" {
		name = c.Request().Host
		if h := hostOf("http://" + name); h != "" {
			name = h
		}
	}
	return name
}

// GET /server-info/
func (rs *RestService) serverInfo(c *echo.Context) error {
	srv := rs.server
	wikiURL := strings.TrimRight(srv.WikiURL, "/")
	clientURL := rs.authenticator.ClientURL()
	name := rs.instanceName(c)
	enabled := rs.authenticator.Enabled()
	methods := []string{}
	if enabled {
		methods = append(methods, "password")
		if rs.authenticator.PasskeysEnabled() {
			methods = append(methods, "passkey")
		}
		methods = append(methods, "device-link")
	}
	features := []string{FeatureMovedLookup, FeatureJoinPresence, FeatureInstancePresence, FeatureAvatars, FeaturePins, FeatureFeed, FeaturePinOrder, FeaturePageMeta, FeatureWikiConfig, FeatureFeedRanges, FeatureQRCodes, FeatureForwardAuth, FeatureChanges}
	if rs.git != nil {
		features = append(features, FeatureHistory, FeatureRevert)
	}
	if rs.authenticator.AccessRequestsAvailable() {
		features = append(features, FeatureAccessRequests)
	}
	if rs.push != nil {
		features = append(features, FeaturePush)
	}
	if enabled {
		features = append(features, FeatureOAuth)
	}
	return c.JSONPretty(http.StatusOK, &ServerInfoResponse{
		Name:            name,
		Version:         global.Version,
		ProtocolVersion: collab.ProtocolVersion,
		BackendURL:      optionalString(rs.backendURL(c)),
		WikiURL:         optionalString(wikiURL),
		WebClientURL:    optionalString(clientURL),
		SetupRequired:   rs.authenticator.SetupRequired(),
		AuthDisabled:    !enabled,
		Passkeys:        PasskeyStatus{Enabled: rs.authenticator.PasskeysEnabled(), RPID: rs.authenticator.RPID()},
		LoginMethods:    methods,
		Require2FA:      enabled && rs.authenticator.Require2FA(),
		Features:        features,
		Push:            rs.pushInfo(),
	}, jsonIndent)
}

// connectURL builds {clientUrl}/connect#<payload>, nil without clientUrl
func (rs *RestService) connectURL(c *echo.Context, u *auth.User, token string, expiresAt time.Time) *string {
	clientURL := rs.authenticator.ClientURL()
	if clientURL == "" {
		return nil
	}
	data, err := json.Marshal(ConnectPayload{V: ConnectPayloadVersion, BackendURL: rs.backendURL(c), Username: u.Username, Token: token, ExpiresAt: expiresAt})
	if err != nil {
		return nil
	}
	link := clientURL + "/connect#" + base64.RawURLEncoding.EncodeToString(data)
	return &link
}

// GET /account/device-links/
func (rs *RestService) listDeviceLinks(c *echo.Context) error {
	links, err := rs.authenticator.DeviceLinks(UserFromContext(c).ID)
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, links, jsonIndent)
}

// POST /account/device-links/
func (rs *RestService) createDeviceLink(c *echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	var r CreateDeviceLinkRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	u := UserFromContext(c)
	l, token, err := rs.authenticator.CreateDeviceLink(u, SessionFromContext(c), r.DeviceName, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSONPretty(http.StatusOK, &CreatedDeviceLinkResponse{DeviceLink: l, Token: token, ExpiresAt: l.ExpiresAt,
		ConnectURL: rs.connectURL(c, u, token, l.ExpiresAt)}, jsonIndent)
}

// GET /account/device-links/{id}/
func (rs *RestService) getDeviceLink(c *echo.Context) error {
	l, err := rs.authenticator.DeviceLink(UserFromContext(c).ID, c.Param(urlParamID))
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, l, jsonIndent)
}

// DELETE /account/device-links/{id}/
func (rs *RestService) revokeDeviceLink(c *echo.Context) error {
	if err := rs.authenticator.RevokeDeviceLink(UserFromContext(c), c.Param(urlParamID), requestInfo(c)); err != nil {
		return rs.authError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /auth/device-link/redeem/
func (rs *RestService) redeemDeviceLink(c *echo.Context) error {
	if !rs.authenticator.Enabled() {
		return apiError(c, http.StatusForbidden, CodeForbidden, "authentication is disabled (server.auth.disabled), there are no accounts", nil)
	}
	if err := rs.limitIP(c); err != nil {
		return err
	}
	var r RedeemDeviceLinkRequest
	if err := decodeJSON(c, &r); err != nil {
		return err
	}
	if r.Token == "" {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "token is required", map[string]any{"field": "token"})
	}
	if r.SessionType == "" {
		r.SessionType = auth.SessionBearer
	}
	result, err := rs.authenticator.RedeemDeviceLink(r.Token, r.Username, rs.sessionParams(c, r.sessionRequestFields))
	if err != nil {
		return rs.authError(c, err)
	}
	return rs.writeLoginResult(c, result)
}
