package gitremote

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitremote/forgejotest"
)

// memStore is a Store in memory
type memStore struct {
	mu     sync.Mutex
	g      *auth.GitRemote
	events []auth.AuditEvent
}

func (s *memStore) GitRemote() (*auth.GitRemote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g == nil {
		return nil, auth.ErrNoGitRemote
	}
	g := *s.g
	return &g, nil
}

func (s *memStore) SaveGitRemote(g *auth.GitRemote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *g
	s.g = &c
	return nil
}

func (s *memStore) SetGitRemoteError(at time.Time, message string, permanent bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.g != nil {
		s.g.ErrorAt, s.g.ErrorMessage, s.g.ErrorPermanent = at, message, permanent
	}
	return nil
}

func (s *memStore) DeleteGitRemote() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	had := s.g != nil
	s.g = nil
	return had, nil
}

func (s *memStore) Audit(e auth.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func newTestManager(t *testing.T, f *forgejotest.Fake, user string) (*Manager, *memStore) {
	t.Helper()
	store := &memStore{}
	cfg := configuration.GitOAuthConfiguration{Provider: "forgejo", URL: f.URL, ClientID: forgejotest.ClientID, User: user,
		RefreshTokenLifetime: configuration.DefaultGitOAuthRefreshTokenLifetime}
	m, err := New(cfg, forgejotest.ClientSecret, "markus/Wiki", store)
	if err != nil {
		t.Fatal(err)
	}
	m.SetHTTPClient(f.Client())
	return m, store
}

// connect runs the flow up to Connect
func connect(t *testing.T, m *Manager, f *forgejotest.Fake) (*auth.GitRemote, error) {
	t.Helper()
	verifier, challenge := NewVerifier()
	redirect := "https://wiki.example.com/api/git-remote/callback/"
	code, state, err := f.Approve(m.AuthorizeURL(redirect, "the-state", challenge))
	if err != nil || state != "the-state" || code == "" {
		t.Fatalf("approve: code %q, state %q, %v", code, state, err)
	}
	return m.Connect(context.Background(), code, verifier, redirect, "account-1")
}

func TestConnectAndRefresh(t *testing.T) {
	f := forgejotest.New(t)
	m, store := newTestManager(t, f, "mkdocseditor")
	g, err := connect(t, m, f)
	if err != nil {
		t.Fatal(err)
	}
	if g.Login != "mkdocseditor" || g.Repo != "markus/Wiki" || g.ConnectedBy != "account-1" || g.AccessToken == "" {
		t.Fatalf("connection = %+v", g)
	}
	user, password, err := m.Credential(context.Background())
	if err != nil || user != "mkdocseditor" || password != g.AccessToken {
		t.Fatalf("credential = %q %q %v", user, password, err)
	}
	if push, err := m.CanPush(context.Background()); err != nil || !push {
		t.Errorf("can push = %v, %v", push, err)
	}

	// an access token that ends soon is refreshed; the new refresh token is stored
	now := time.Now()
	m.now = func() time.Time { return now.Add(59 * time.Minute) }
	_, password, err = m.Credential(context.Background())
	if err != nil || password == g.AccessToken || f.Refreshes != 1 {
		t.Fatalf("refreshed credential = %q, %v, refreshes %d", password, err, f.Refreshes)
	}
	// a day later the keep-alive refresh runs, with the rotated refresh token
	m.now = func() time.Time { return now.Add(25 * time.Hour) }
	if err = m.RefreshIfDue(context.Background()); err != nil || f.Refreshes != 2 {
		t.Fatalf("keep-alive refresh: %v, refreshes %d", err, f.Refreshes)
	}
	if err = m.RefreshIfDue(context.Background()); err != nil || f.Refreshes != 2 {
		t.Errorf("refreshed again: %v, refreshes %d", err, f.Refreshes)
	}

	// a temporary failure keeps the connection
	f.Down = true
	m.now = func() time.Time { return now.Add(50 * time.Hour) }
	if err = m.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("refresh against a down server succeeded")
	}
	if s, _ := store.GitRemote(); s.ErrorPermanent || s.ErrorMessage == "" {
		t.Errorf("after a temporary failure: %+v", s)
	}
	f.Down = false
	if err = m.RefreshIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.GitRemote(); s.ErrorMessage != "" {
		t.Errorf("error not cleared: %+v", s)
	}

	// the grant is removed in Forgejo: expired, audited once
	f.Revoke()
	m.now = func() time.Time { return now.Add(100 * time.Hour) }
	if _, _, err = m.Credential(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("after revoke: %v", err)
	}
	if _, _, err = m.Credential(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("second credential after revoke: %v", err)
	}
	if len(store.events) != 1 || store.events[0].Type != auth.AuditGitRefreshFailed {
		t.Errorf("events = %+v", store.events)
	}

	if g, err = m.Disconnect(); err != nil || g == nil {
		t.Fatal(err)
	}
	if _, _, err = m.Credential(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Errorf("after disconnect: %v", err)
	}
}

func TestConnectRefusals(t *testing.T) {
	f := forgejotest.New(t)
	m, store := newTestManager(t, f, "mkdocseditor")
	refusal := func(want, login string) {
		t.Helper()
		_, err := connect(t, m, f)
		var r *Refusal
		if !errors.As(err, &r) || r.Reason != want || r.Login != login {
			t.Errorf("connect = %v, want %s (%s)", err, want, login)
		}
		if store.g != nil {
			t.Errorf("a refused connection was stored")
		}
	}
	f.Login = "markus"
	refusal(RefusedWrongUser, "markus")
	f.Login, f.IsAdmin = "mkdocseditor", true
	refusal(RefusedAdminUser, "mkdocseditor")
	f.IsAdmin = false
	f.Pushers = map[string]bool{}
	refusal(RefusedNotPushable, "mkdocseditor")

	// a wrong code verifier fails the exchange
	f.Pushers["mkdocseditor"] = true
	_, challenge := NewVerifier()
	code, _, _ := f.Approve(m.AuthorizeURL("https://wiki.example.com/cb", "s", challenge))
	if _, err := m.Connect(context.Background(), code, "wrong", "https://wiki.example.com/cb", "a"); err == nil || !strings.HasPrefix(err.Error(), RefusedExchange) {
		t.Errorf("wrong verifier = %v", err)
	}
}

func TestRepoOf(t *testing.T) {
	for _, c := range []struct{ base, push, want string }{
		{"https://git.example.com", "https://git.example.com/markus/Wiki.git", "markus/Wiki"},
		{"https://git.example.com/", "https://GIT.example.com/markus/Wiki", "markus/Wiki"},
		{"https://example.com/git", "https://example.com/git/markus/Wiki.git", "markus/Wiki"},
		{"https://git.example.com", "ssh://git@git.example.com/markus/Wiki.git", ""},
		{"https://git.example.com", "git@git.example.com:markus/Wiki.git", ""},
		{"https://git.example.com", "https://other.example.com/markus/Wiki.git", ""},
		{"https://git.example.com", "https://git.example.com/markus/sub/Wiki.git", ""},
	} {
		if got := RepoOf(c.base, c.push); got != c.want {
			t.Errorf("RepoOf(%q, %q) = %q, want %q", c.base, c.push, got, c.want)
		}
	}
}
