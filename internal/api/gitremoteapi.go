package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitremote"
	"github.com/labstack/echo/v5"
)

// The connection to the git host for pushing: an admin approves an OAuth2 application of the
// git host, the backend keeps the tokens and pushes with them. See docs/sync-protocol.md
// section 10 and docs/auth.md.

const (
	// EndpointPathGitRemoteCallback is where the git host sends the admin's browser back. It is
	// no admin path: the handler answers a missing session with a redirect, not with JSON.
	EndpointPathGitRemoteCallback = "/git-remote/callback/"
	// CodeNotConfigured means git.oauth is not set
	CodeNotConfigured = "not-configured"
	// gitRemoteSettingsPath is the page of the web app the callback redirects to
	gitRemoteSettingsPath = "/admin/settings"
	gitRemoteStateTTL     = 10 * time.Minute
	gitRemoteCheckTimeout = 5 * time.Second
)

// States of GitRemoteResponse
const (
	GitRemoteNotConfigured = "not-configured"
	GitRemoteDisconnected  = "disconnected"
	GitRemoteConnected     = "connected"
	GitRemoteExpired       = "expired"
	GitRemoteError         = "error"
)

// Reasons of a failed callback, besides those of gitremote.Refusal
const (
	gitRemoteReasonDenied  = "denied"
	gitRemoteReasonState   = "state"
	gitRemoteReasonSession = "session"
)

type (
	// GitRemoteResponse is the answer of GET /admin/git-remote/
	GitRemoteResponse struct {
		State            string                 `json:"state"`
		PushEnabled      bool                   `json:"pushEnabled"`
		Provider         *string                `json:"provider"`
		ProviderName     *string                `json:"providerName"`
		ProviderURL      *string                `json:"providerUrl"`
		GrantsURL        *string                `json:"grantsUrl"`
		ScopedGrants     bool                   `json:"scopedGrants"`
		Repo             *string                `json:"repo"`
		ExpectedUser     *string                `json:"expectedUser"`
		AllowAdmin       bool                   `json:"allowAdmin"`
		RemoteUser       *GitRemoteUser         `json:"remoteUser"`
		CanPush          *bool                  `json:"canPush"`
		ConnectedAt      *string                `json:"connectedAt"`
		ConnectedBy      *string                `json:"connectedBy"`
		LastRefreshAt    *string                `json:"lastRefreshAt"`
		RefreshExpiresAt *string                `json:"refreshExpiresAt"`
		LastRefreshError *GitRemoteRefreshError `json:"lastRefreshError"`
		LastPushAt       *string                `json:"lastPushAt"`
		LastPushError    *GitRemotePushError    `json:"lastPushError"`
		PendingCommits   *int                   `json:"pendingCommits"`
	}

	// GitRemoteUser is the user of the git host the connection belongs to
	GitRemoteUser struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatarUrl"`
		IsAdmin   bool   `json:"isAdmin"`
	}

	// GitRemoteRefreshError is a failed refresh; permanent means the grant is gone
	GitRemoteRefreshError struct {
		At        string `json:"at"`
		Message   string `json:"message"`
		Permanent bool   `json:"permanent"`
	}

	// GitRemotePushError is the last failed push
	GitRemotePushError struct {
		At      string `json:"at"`
		Message string `json:"message"`
	}

	// GitRemoteConnectResponse is the answer of POST /admin/git-remote/connect/
	GitRemoteConnectResponse struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}

	// gitRemoteFlow is a started connection, by its state parameter
	gitRemoteFlow struct {
		sessionID   string
		accountID   string
		verifier    string
		redirectURI string
		expires     time.Time
	}

	gitRemoteFlows struct {
		mu    sync.Mutex
		flows map[string]*gitRemoteFlow
	}
)

// SetGitRemote enables the connection to the git host. Call it before Start.
func (rs *RestService) SetGitRemote(m *gitremote.Manager) {
	rs.gitRemote = m
}

// add stores a flow and drops the expired ones
func (f *gitRemoteFlows) add(state string, flow *gitRemoteFlow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flows == nil {
		f.flows = map[string]*gitRemoteFlow{}
	}
	now := time.Now()
	for k, v := range f.flows {
		if now.After(v.expires) {
			delete(f.flows, k)
		}
	}
	f.flows[state] = flow
}

// take removes and returns the flow of a state, nil if it is unknown or expired
func (f *gitRemoteFlows) take(state string) *gitRemoteFlow {
	f.mu.Lock()
	defer f.mu.Unlock()
	flow := f.flows[state]
	delete(f.flows, state)
	if flow == nil || time.Now().After(flow.expires) {
		return nil
	}
	return flow
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timePtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}

// GET /admin/git-remote/
func (rs *RestService) getGitRemote(c *echo.Context) error {
	resp, err := rs.gitRemoteStatus(c.Request().Context())
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, resp, jsonIndent)
}

func (rs *RestService) gitRemoteStatus(ctx context.Context) (*GitRemoteResponse, error) {
	resp := &GitRemoteResponse{State: GitRemoteNotConfigured}
	if rs.git != nil {
		resp.PushEnabled = rs.git.PushEnabled()
		status := rs.git.PushStatus()
		resp.LastPushAt = timePtr(status.At)
		if status.Error != "" {
			resp.LastPushError = &GitRemotePushError{At: formatTime(status.ErrorAt), Message: status.Error}
		}
		if n, ok := rs.git.PendingPush(); ok {
			resp.PendingCommits = &n
		}
	}
	m := rs.gitRemote
	if m == nil {
		return resp, nil
	}
	p, cfg := m.Provider(), m.Config()
	resp.Provider, resp.ProviderName, resp.ProviderURL = strPtr(p.ID()), strPtr(p.Name()), strPtr(cfg.URL)
	resp.GrantsURL, resp.ScopedGrants = strPtr(p.GrantsURL()), p.ScopedGrants()
	resp.Repo, resp.ExpectedUser, resp.AllowAdmin = strPtr(m.Repo()), strPtr(cfg.User), cfg.AllowAdmin

	g, err := m.Stored()
	if err != nil {
		return nil, err
	}
	if g == nil {
		resp.State = GitRemoteDisconnected
		return resp, nil
	}
	resp.RemoteUser = &GitRemoteUser{Login: g.Login, AvatarURL: g.AvatarURL, IsAdmin: g.IsAdmin}
	resp.ConnectedAt, resp.LastRefreshAt, resp.RefreshExpiresAt = timePtr(g.ConnectedAt), timePtr(g.LastRefreshAt), timePtr(g.RefreshExpiresAt)
	if u := rs.authenticator.UserByID(g.ConnectedBy); u != nil {
		resp.ConnectedBy = &u.Username
	}
	if g.ErrorMessage != "" {
		resp.LastRefreshError = &GitRemoteRefreshError{At: formatTime(g.ErrorAt), Message: g.ErrorMessage, Permanent: g.ErrorPermanent}
	}
	switch {
	case g.ErrorPermanent || (!g.RefreshExpiresAt.IsZero() && time.Now().After(g.RefreshExpiresAt)):
		resp.State = GitRemoteExpired
		return resp, nil
	case g.ErrorMessage != "":
		resp.State = GitRemoteError
	default:
		resp.State = GitRemoteConnected
	}
	checkCtx, cancel := context.WithTimeout(ctx, gitRemoteCheckTimeout)
	defer cancel()
	if push, err := m.CanPush(checkCtx); err == nil {
		resp.CanPush = &push
	}
	return resp, nil
}

// POST /admin/git-remote/connect/
func (rs *RestService) connectGitRemote(c *echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	m := rs.gitRemote
	if m == nil {
		return apiError(c, http.StatusNotFound, CodeNotConfigured, "pushing with OAuth2 is not configured (git.oauth)", nil)
	}
	if m.Repo() == "" {
		return apiError(c, http.StatusConflict, CodeConflict, "the push remote of the wiki is not an https URL on "+m.Config().URL, nil)
	}
	sess := SessionFromContext(c)
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	state := base64.RawURLEncoding.EncodeToString(b)
	verifier, challenge := gitremote.NewVerifier()
	redirectURI := rs.ownOrigin(c) + APIPrefix + EndpointPathGitRemoteCallback
	rs.gitRemoteFlows.add(state, &gitRemoteFlow{sessionID: sess.ID, accountID: sess.AccountID, verifier: verifier,
		redirectURI: redirectURI, expires: time.Now().Add(gitRemoteStateTTL)})
	return c.JSONPretty(http.StatusOK, &GitRemoteConnectResponse{AuthorizeURL: m.AuthorizeURL(redirectURI, state, challenge)}, jsonIndent)
}

// DELETE /admin/git-remote/
func (rs *RestService) deleteGitRemote(c *echo.Context) error {
	if err := rs.requireReauth(c); err != nil {
		return err
	}
	m := rs.gitRemote
	if m == nil {
		return apiError(c, http.StatusNotFound, CodeNotConfigured, "pushing with OAuth2 is not configured (git.oauth)", nil)
	}
	g, err := m.Disconnect()
	if err != nil {
		return rs.ReturnError(c, err)
	}
	if g != nil {
		rs.auditGitRemote(c, auth.AuditGitDisconnected, map[string]any{"login": g.Login, "repo": g.Repo})
	}
	resp, err := rs.gitRemoteStatus(c.Request().Context())
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, resp, jsonIndent)
}

// GET /git-remote/callback/?code=&state= (or ?error=&state=)
func (rs *RestService) gitRemoteCallback(c *echo.Context) error {
	m := rs.gitRemote
	if m == nil {
		return rs.gitRemoteRedirect(c, CodeNotConfigured, "")
	}
	flow := rs.gitRemoteFlows.take(c.QueryParam("state"))
	if flow == nil {
		return rs.gitRemoteRedirect(c, gitRemoteReasonState, "")
	}
	// the admin who started it, in the same session
	sess, user := SessionFromContext(c), UserFromContext(c)
	if sess == nil || user == nil || !user.IsAdmin() || sess.ID != flow.sessionID || user.ID != flow.accountID {
		return rs.gitRemoteRedirect(c, gitRemoteReasonSession, "")
	}
	if e := c.QueryParam("error"); e != "" || c.QueryParam("code") == "" {
		reason := gitremote.RefusedExchange
		if e == "access_denied" {
			reason = gitRemoteReasonDenied
		}
		rs.auditGitRemote(c, auth.AuditGitConnectRefused, map[string]any{"reason": reason})
		return rs.gitRemoteRedirect(c, reason, "")
	}
	g, err := m.Connect(c.Request().Context(), c.QueryParam("code"), flow.verifier, flow.redirectURI, user.ID)
	var refusal *gitremote.Refusal
	if errors.As(err, &refusal) {
		details := map[string]any{"reason": refusal.Reason}
		if refusal.Login != "" {
			details["login"] = refusal.Login
		}
		if refusal.Err != nil {
			details["error"] = refusal.Err.Error()
		}
		rs.auditGitRemote(c, auth.AuditGitConnectRefused, details)
		return rs.gitRemoteRedirect(c, refusal.Reason, refusal.Login)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.auditGitRemote(c, auth.AuditGitConnected, map[string]any{"login": g.Login, "repo": g.Repo})
	if rs.git != nil {
		// the commits that waited for the connection
		rs.git.PushNow()
	}
	return c.Redirect(http.StatusFound, gitRemoteSettingsPath+"?git-remote=connected")
}

func (rs *RestService) gitRemoteRedirect(c *echo.Context, reason, login string) error {
	q := url.Values{"git-remote": {"error"}, "reason": {reason}}
	if login != "" {
		q.Set("login", login)
	}
	return c.Redirect(http.StatusFound, gitRemoteSettingsPath+"?"+q.Encode())
}

func (rs *RestService) auditGitRemote(c *echo.Context, eventType string, details map[string]any) {
	if rs.gitRemote != nil {
		details["provider"] = rs.gitRemote.Provider().ID()
	}
	info := requestInfo(c)
	e := auth.AuditEvent{Type: eventType, IP: info.IP, UserAgent: info.UserAgent, Details: details}
	if u := UserFromContext(c); u != nil {
		e.ActorID = u.ID
	}
	rs.authenticator.Audit(e)
}
