// Package forgejotest is a fake Forgejo for tests: the OAuth2 authorize and token endpoints
// with PKCE and single-use refresh tokens, and the user and repository API.
package forgejotest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
)

type (
	// Fake is the fake server
	Fake struct {
		*httptest.Server

		mu sync.Mutex
		// Login is the user who approves the next authorization, IsAdmin his admin flag
		Login   string
		IsAdmin bool
		// Pushers are the logins with push access to Repo
		Repo    string
		Pushers map[string]bool
		// ExpiresIn is the lifetime of access tokens in seconds
		ExpiresIn int
		// Down answers every token request with 503
		Down bool

		codes   map[string]grant // authorization code -> grant
		access  map[string]string
		refresh map[string]string // refresh token -> login, removed when used
		// Refreshes counts the successful refreshes
		Refreshes int
	}

	grant struct {
		login     string
		challenge string
		redirect  string
	}
)

// New starts a fake Forgejo with the user mkdocseditor, who may push to markus/Wiki
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{Login: "mkdocseditor", Repo: "markus/Wiki", Pushers: map[string]bool{"mkdocseditor": true}, ExpiresIn: 3600,
		codes: map[string]grant{}, access: map[string]string{}, refresh: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", f.token)
	mux.HandleFunc("GET /api/v1/user", f.user)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{name}", f.repo)
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Close)
	return f
}

func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Approve does what the browser does at the authorize URL: it returns the code and the state
// of the redirect to the callback
func (f *Fake) Approve(authorizeURL string) (code, state string, err error) {
	resp, err := f.Client().Get(authorizeURL)
	if err != nil {
		return "", "", err
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Request.URL.Query().Get("redirected"))
	if err != nil {
		return "", "", err
	}
	return loc.Query().Get("code"), loc.Query().Get("state"), nil
}

// authorize issues a code for Login and answers with the redirect target as a query value,
// so Approve can read it without following it
func (f *Fake) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	code := token()
	f.codes[code] = grant{login: f.Login, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	f.mu.Unlock()
	target := q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	http.Redirect(w, r, "/done?"+url.Values{"redirected": {target}}.Encode(), http.StatusFound)
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	_ = r.ParseForm()
	if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	var login string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || g.redirect != r.PostForm.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "bad code"})
			return
		}
		login = g.login
	case "refresh_token":
		var ok bool
		login, ok = f.refresh[r.PostForm.Get("refresh_token")]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "token was already used"})
			return
		}
		// single-use, like INVALIDATE_REFRESH_TOKENS
		delete(f.refresh, r.PostForm.Get("refresh_token"))
		f.Refreshes++
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	access, refresh := token(), token()
	f.access[access] = login
	f.refresh[refresh] = login
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "bearer", "expires_in": f.ExpiresIn})
}

// Revoke makes all refresh tokens invalid, like removing the grant in Forgejo
func (f *Fake) Revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh = map[string]string{}
	f.access = map[string]string{}
}

func (f *Fake) loginOf(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	login, ok := f.access[tok]
	return login, ok
}

func (f *Fake) user(w http.ResponseWriter, r *http.Request) {
	login, ok := f.loginOf(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	admin := f.IsAdmin
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"login": login, "avatar_url": f.URL + "/avatars/" + login, "is_admin": admin})
}

func (f *Fake) repo(w http.ResponseWriter, r *http.Request) {
	login, ok := f.loginOf(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.PathValue("owner")+"/"+r.PathValue("name") != f.Repo {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	push := f.Pushers[login]
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"full_name": f.Repo, "permissions": map[string]bool{"pull": true, "push": push}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
