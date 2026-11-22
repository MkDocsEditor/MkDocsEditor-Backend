package gitremote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/metrics"
)

const (
	// refreshAhead refreshes the access token when it ends within this time
	refreshAhead = 5 * time.Minute
	// keepAliveInterval refreshes at least this often, so the refresh token never runs out
	keepAliveInterval = 24 * time.Hour
	// checkInterval is how often Run looks whether a refresh is due
	checkInterval  = time.Hour
	requestTimeout = 15 * time.Second
)

// Reasons a connection is refused, the reason of the callback redirect
const (
	RefusedWrongUser   = "wrong-user"
	RefusedAdminUser   = "admin-user"
	RefusedNotPushable = "not-pushable"
	RefusedExchange    = "exchange"
)

var (
	// ErrNotConnected means no connection is stored
	ErrNotConnected = errors.New("the git host is not connected: an admin connects it in the web app")
	// ErrExpired means the grant is gone (invalid_grant): an admin must connect again
	ErrExpired = errors.New("the connection to the git host expired or was revoked: an admin must connect again")
)

// Store keeps the connection, implemented by auth.Service
type Store interface {
	GitRemote() (*auth.GitRemote, error)
	SaveGitRemote(g *auth.GitRemote) error
	SetGitRemoteError(at time.Time, message string, permanent bool) error
	DeleteGitRemote() (bool, error)
	Audit(e auth.AuditEvent)
}

// Manager holds the connection to the git host
type Manager struct {
	cfg      configuration.GitOAuthConfiguration
	secret   string
	provider Provider
	repo     string
	store    Store
	hc       *http.Client
	now      func() time.Time

	// mu serializes refreshes: refresh tokens are single-use
	mu sync.Mutex
}

// New creates the manager. repo is owner/name of the repository pushed to, "" if the push
// remote is not on the git host.
func New(cfg configuration.GitOAuthConfiguration, secret string, repo string, store Store) (*Manager, error) {
	p, err := NewProvider(cfg.Provider, cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.URL == "" || cfg.ClientID == "" {
		return nil, errors.New("git.oauth needs url and clientId")
	}
	m := &Manager{cfg: cfg, secret: secret, provider: p, repo: repo, store: store,
		hc: &http.Client{Timeout: requestTimeout}, now: time.Now}
	m.updateMetrics()
	return m, nil
}

// SetHTTPClient replaces the HTTP client (tests)
func (m *Manager) SetHTTPClient(hc *http.Client) { m.hc = hc }

func (m *Manager) Provider() Provider                          { return m.provider }
func (m *Manager) Config() configuration.GitOAuthConfiguration { return m.cfg }

// Repo is owner/name of the repository, "" if the push remote is not on the git host
func (m *Manager) Repo() string { return m.repo }

// NewVerifier returns a PKCE code verifier and its S256 challenge (RFC 7636)
func NewVerifier() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL is the URL the admin's browser goes to
func (m *Manager) AuthorizeURL(redirectURI, state, challenge string) string {
	return m.provider.AuthorizeURL(m.cfg.ClientID, redirectURI, state, challenge)
}

// Refusal is a connection that was refused, with the login of the git host if known
type Refusal struct {
	Reason string
	Login  string
	Err    error
}

func (r *Refusal) Error() string {
	if r.Err != nil {
		return r.Reason + ": " + r.Err.Error()
	}
	return r.Reason
}

// Connect exchanges an authorization code, checks the user and stores the connection. A
// refused connection is a *Refusal; its tokens are dropped.
func (m *Manager) Connect(ctx context.Context, code, verifier, redirectURI, accountID string) (*auth.GitRemote, error) {
	tok, err := m.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}})
	if err != nil {
		return nil, &Refusal{Reason: RefusedExchange, Err: err}
	}
	u, err := m.provider.User(ctx, m.hc, tok.AccessToken)
	if err != nil {
		return nil, &Refusal{Reason: RefusedExchange, Err: err}
	}
	switch {
	case m.cfg.User != "" && !strings.EqualFold(u.Login, m.cfg.User):
		return nil, &Refusal{Reason: RefusedWrongUser, Login: u.Login}
	case u.IsAdmin && !m.cfg.AllowAdmin:
		return nil, &Refusal{Reason: RefusedAdminUser, Login: u.Login}
	}
	push, err := m.provider.CanPush(ctx, m.hc, tok.AccessToken, m.repo)
	if err != nil {
		return nil, &Refusal{Reason: RefusedExchange, Login: u.Login, Err: err}
	}
	if !push {
		return nil, &Refusal{Reason: RefusedNotPushable, Login: u.Login}
	}
	now := m.now().UTC()
	g := &auth.GitRemote{Login: u.Login, IsAdmin: u.IsAdmin, AvatarURL: u.AvatarURL, Repo: m.repo,
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, AccessExpiresAt: tok.expiresAt(now),
		RefreshExpiresAt: m.refreshExpiry(now), ConnectedAt: now, ConnectedBy: accountID, LastRefreshAt: now}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = m.store.SaveGitRemote(g); err != nil {
		return nil, err
	}
	m.updateMetricsLocked(g)
	log.Printf("Connected %s as %s for pushing to %s", m.provider.Name(), u.Login, m.repo)
	return g, nil
}

// Disconnect deletes the stored connection. The grant stays on the git host.
func (m *Manager) Disconnect() (*auth.GitRemote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.store.GitRemote()
	if err != nil && !errors.Is(err, auth.ErrNoGitRemote) {
		return nil, err
	}
	if _, err = m.store.DeleteGitRemote(); err != nil {
		return nil, err
	}
	m.updateMetricsLocked(nil)
	if g != nil {
		log.Printf("Disconnected %s (%s)", m.provider.Name(), g.Login)
	}
	return g, nil
}

// Stored returns the stored connection, nil if there is none
func (m *Manager) Stored() (*auth.GitRemote, error) {
	g, err := m.store.GitRemote()
	if errors.Is(err, auth.ErrNoGitRemote) {
		return nil, nil
	}
	return g, err
}

// CanPush checks the push permission of the stored connection live
func (m *Manager) CanPush(ctx context.Context) (bool, error) {
	_, token, err := m.Credential(ctx)
	if err != nil {
		return false, err
	}
	return m.provider.CanPush(ctx, m.hc, token, m.repo)
}

// Credential returns the username and password git pushes with, refreshing the access token
// when it ends soon
func (m *Manager) Credential(ctx context.Context) (username, password string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.store.GitRemote()
	if errors.Is(err, auth.ErrNoGitRemote) {
		return "", "", ErrNotConnected
	}
	if err != nil {
		return "", "", err
	}
	if g.ErrorPermanent {
		return "", "", ErrExpired
	}
	if m.now().Add(refreshAhead).After(g.AccessExpiresAt) {
		if g, err = m.refreshLocked(ctx, g); err != nil {
			return "", "", err
		}
	}
	return m.provider.GitUsername(g.Login), g.AccessToken, nil
}

// RefreshIfDue refreshes when the last refresh is older than a day, so the refresh token
// stays valid while nobody pushes
func (m *Manager) RefreshIfDue(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.store.GitRemote()
	if errors.Is(err, auth.ErrNoGitRemote) {
		return nil
	}
	if err != nil || g.ErrorPermanent || m.now().Sub(g.LastRefreshAt) < keepAliveInterval {
		return err
	}
	_, err = m.refreshLocked(ctx, g)
	return err
}

// Run refreshes when due until ctx ends
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		if err := m.RefreshIfDue(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Unable to refresh the %s token: %v", m.provider.Name(), err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshLocked uses the refresh token and stores the new pair at once: the old refresh token
// is invalid after the call (Forgejo INVALIDATE_REFRESH_TOKENS)
func (m *Manager) refreshLocked(ctx context.Context, g *auth.GitRemote) (*auth.GitRemote, error) {
	now := m.now().UTC()
	tok, err := m.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.RefreshToken}})
	if err != nil {
		permanent := errors.Is(err, errInvalidGrant)
		result := metrics.ResultError
		if permanent {
			result = metrics.ResultExpired
		}
		metrics.GitRemoteRefreshes.WithLabelValues(result).Inc()
		changed := g.ErrorPermanent != permanent || g.ErrorAt.IsZero()
		if serr := m.store.SetGitRemoteError(now, err.Error(), permanent); serr != nil {
			log.Printf("Unable to record the failed refresh: %v", serr)
		}
		if permanent {
			if changed {
				m.store.Audit(auth.AuditEvent{Type: auth.AuditGitRefreshFailed, Details: map[string]any{
					"provider": m.provider.ID(), "login": g.Login, "error": err.Error()}})
			}
			g.ErrorPermanent = true
			m.updateMetricsLocked(g)
			return nil, ErrExpired
		}
		if changed {
			log.Printf("Unable to refresh the %s token, retrying later: %v", m.provider.Name(), err)
		}
		if now.Before(g.AccessExpiresAt) {
			// the old access token still works
			return g, nil
		}
		return nil, fmt.Errorf("unable to refresh the %s token: %w", m.provider.Name(), err)
	}
	g.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		g.RefreshToken = tok.RefreshToken
		g.RefreshExpiresAt = m.refreshExpiry(now)
	}
	g.AccessExpiresAt, g.LastRefreshAt = tok.expiresAt(now), now
	g.ErrorAt, g.ErrorMessage, g.ErrorPermanent = time.Time{}, "", false
	if err = m.store.SaveGitRemote(g); err != nil {
		// the new refresh token is lost: the next refresh fails with invalid_grant
		log.Printf("Unable to store the refreshed %s token: %v", m.provider.Name(), err)
		return nil, err
	}
	metrics.GitRemoteRefreshes.WithLabelValues(metrics.ResultOK).Inc()
	m.updateMetricsLocked(g)
	return g, nil
}

func (m *Manager) refreshExpiry(now time.Time) time.Time {
	return now.Add(time.Duration(m.cfg.RefreshTokenLifetime) * time.Hour)
}

func (m *Manager) updateMetrics() {
	g, _ := m.Stored()
	m.updateMetricsLocked(g)
}

func (m *Manager) updateMetricsLocked(g *auth.GitRemote) {
	if g == nil || g.ErrorPermanent {
		metrics.GitRemoteConnected.Set(0)
		metrics.GitRemoteRefreshExpiry.Set(0)
		return
	}
	metrics.GitRemoteConnected.Set(1)
	metrics.GitRemoteRefreshExpiry.Set(float64(g.RefreshExpiresAt.Unix()))
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (t *tokenResponse) expiresAt(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return now.Add(time.Hour)
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// errInvalidGrant is the token endpoint's invalid_grant: the code or refresh token is no longer valid
var errInvalidGrant = errors.New("invalid_grant")

// token calls the token endpoint with the client credentials
func (m *Manager) token(ctx context.Context, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", m.cfg.ClientID)
	form.Set("client_secret", m.secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.provider.TokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var tok tokenResponse
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if jerr := json.Unmarshal(body, &tok); jerr != nil && resp.StatusCode == http.StatusOK {
		return nil, fmt.Errorf("invalid token response: %w", jerr)
	}
	if tok.Error == "invalid_grant" {
		return nil, fmt.Errorf("%w: %s", errInvalidGrant, tok.Description)
	}
	if resp.StatusCode != http.StatusOK || tok.Error != "" || tok.AccessToken == "" {
		msg := tok.Error
		if tok.Description != "" {
			msg += ": " + tok.Description
		}
		if msg == "" {
			msg = (&StatusError{Code: resp.StatusCode, URL: m.provider.TokenURL()}).Error()
		}
		return nil, errors.New(msg)
	}
	return &tok, nil
}
