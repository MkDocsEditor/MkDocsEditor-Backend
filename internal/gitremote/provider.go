// Package gitremote connects the backend to an OAuth2 application of the git host, so it can
// push with a token an admin granted: the authorization code flow with PKCE, the stored tokens,
// their refresh, and the credential for git. What differs between git hosts is behind
// Provider. See docs/sync-protocol.md section 10.
package gitremote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type (
	// Provider is what differs between git hosts
	Provider interface {
		// ID is the value of git.oauth.provider, Name the display name
		ID() string
		Name() string
		// AuthorizeURL is the URL the admin's browser is sent to
		AuthorizeURL(clientID, redirectURI, state, challenge string) string
		// TokenURL is the token endpoint (RFC 6749 section 3.2)
		TokenURL() string
		// User returns the user an access token belongs to
		User(ctx context.Context, hc *http.Client, accessToken string) (*RemoteUser, error)
		// CanPush reports whether the user of an access token may push to a repository (owner/name)
		CanPush(ctx context.Context, hc *http.Client, accessToken, repo string) (bool, error)
		// GitUsername is the username git sends with the access token as password
		GitUsername(login string) string
		// GrantsURL is the page where a user removes the grant, "" if there is none
		GrantsURL() string
		// ScopedGrants is true if a grant can be narrower than all rights of its user
		ScopedGrants() bool
	}

	// RemoteUser is a user of the git host
	RemoteUser struct {
		Login     string
		AvatarURL string
		IsAdmin   bool
	}
)

// NewProvider returns the provider of git.oauth.provider for a git host
func NewProvider(id, baseURL string) (Provider, error) {
	switch id {
	case "forgejo", "gitea":
		return &forgejo{base: strings.TrimRight(baseURL, "/"), id: id}, nil
	}
	return nil, fmt.Errorf("unknown git.oauth.provider %q: supported is forgejo (also for Gitea)", id)
}

// forgejo is Forgejo and Gitea: same OAuth2 endpoints and API
type forgejo struct {
	base string
	id   string
}

func (f *forgejo) ID() string { return f.id }

func (f *forgejo) Name() string {
	if f.id == "gitea" {
		return "Gitea"
	}
	return "Forgejo"
}

func (f *forgejo) AuthorizeURL(clientID, redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return f.base + "/login/oauth/authorize?" + q.Encode()
}

func (f *forgejo) TokenURL() string { return f.base + "/login/oauth/access_token" }

func (f *forgejo) User(ctx context.Context, hc *http.Client, accessToken string) (*RemoteUser, error) {
	var u struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		IsAdmin   bool   `json:"is_admin"`
	}
	if err := apiGet(ctx, hc, f.base+"/api/v1/user", accessToken, &u); err != nil {
		return nil, err
	}
	if u.Login == "" {
		return nil, errors.New("the git host returned no login")
	}
	return &RemoteUser{Login: u.Login, AvatarURL: u.AvatarURL, IsAdmin: u.IsAdmin}, nil
}

func (f *forgejo) CanPush(ctx context.Context, hc *http.Client, accessToken, repo string) (bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false, fmt.Errorf("invalid repository %q, want owner/name", repo)
	}
	var r struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := apiGet(ctx, hc, f.base+"/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), accessToken, &r); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return r.Permissions.Push, nil
}

// GitUsername: Forgejo takes the token as password with the user's login
func (f *forgejo) GitUsername(login string) string { return login }

func (f *forgejo) GrantsURL() string { return f.base + "/user/settings/applications" }

// ScopedGrants: Forgejo has no OAuth2 scopes, a grant has all rights of its user
func (f *forgejo) ScopedGrants() bool { return false }

// StatusError is an unexpected HTTP status of the git host
type StatusError struct {
	Code int
	URL  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s answered %d %s", e.URL, e.Code, http.StatusText(e.Code))
}

// apiGet fetches JSON with a bearer token
func apiGet(ctx context.Context, hc *http.Client, u, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return &StatusError{Code: resp.StatusCode, URL: u}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// RepoOf returns owner/name of a push URL on the git host at baseURL, "" if the URL is not an
// https URL below it
func RepoOf(baseURL, pushURL string) string {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return ""
	}
	u, err := url.Parse(pushURL)
	if err != nil || u.Scheme != "https" || base.Scheme != "https" || !strings.EqualFold(u.Host, base.Host) {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, base.Path+"/")
	if !ok {
		return ""
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return owner + "/" + name
}

// RedactURL drops the user info of a URL for the log
func RedactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	return u.String()
}
