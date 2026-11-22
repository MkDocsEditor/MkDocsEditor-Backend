package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitremote"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitremote/forgejotest"
)

// withGitRemote restarts the backend with a connection to a fake Forgejo
func (e *syncEnv) withGitRemote(user string) *forgejotest.Fake {
	e.t.Helper()
	f := forgejotest.New(e.t)
	cfg := configuration.GitOAuthConfiguration{Provider: "forgejo", URL: f.URL, ClientID: forgejotest.ClientID, User: user,
		RefreshTokenLifetime: configuration.DefaultGitOAuthRefreshTokenLifetime}
	m, err := gitremote.New(cfg, forgejotest.ClientSecret, "markus/Wiki", e.authn)
	if err != nil {
		e.t.Fatal(err)
	}
	m.SetHTTPClient(f.Client())
	e.stop()
	e.gitRemote = m
	e.start()
	return f
}

func (e *syncEnv) gitRemoteStatus(user string) GitRemoteResponse {
	e.t.Helper()
	r := e.doBody(http.MethodGet, "/admin/git-remote/", user, "")
	if r.Code != http.StatusOK {
		e.t.Fatalf("status: %d %s", r.Code, r.Body.String())
	}
	var s GitRemoteResponse
	if err := json.Unmarshal(r.Body.Bytes(), &s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// callback calls the callback as the browser of user and returns the redirect target
func (e *syncEnv) callback(user string, query url.Values) string {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.http.URL+APIPrefix+EndpointPathGitRemoteCallback+"?"+query.Encode(), nil)
	if user != "" {
		req.Header.Set("Authorization", authHeader(e.t, e.authn, user, testPasswords[user]))
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("callback answered %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// startConnect starts the flow as alice and approves it at the fake
func (e *syncEnv) startConnect(f *forgejotest.Fake) (code, state string) {
	e.t.Helper()
	r := e.doBody(http.MethodPost, "/admin/git-remote/connect/", "alice", "")
	if r.Code != http.StatusOK {
		e.t.Fatalf("connect: %d %s", r.Code, r.Body.String())
	}
	var c GitRemoteConnectResponse
	_ = json.Unmarshal(r.Body.Bytes(), &c)
	if !strings.Contains(c.AuthorizeURL, url.QueryEscape(e.http.URL+APIPrefix+EndpointPathGitRemoteCallback)) {
		e.t.Errorf("authorize URL %s lacks the redirect URI", c.AuthorizeURL)
	}
	code, state, err := f.Approve(c.AuthorizeURL)
	if err != nil {
		e.t.Fatal(err)
	}
	return code, state
}

func TestGitRemoteNotConfigured(t *testing.T) {
	e := newSyncEnv(t)
	if s := e.gitRemoteStatus("alice"); s.State != GitRemoteNotConfigured || s.Provider != nil {
		t.Errorf("status = %+v", s)
	}
	if r := e.doBody(http.MethodPost, "/admin/git-remote/connect/", "alice", ""); r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), CodeNotConfigured) {
		t.Errorf("connect = %d %s", r.Code, r.Body.String())
	}
	if loc := e.callback("alice", url.Values{"code": {"x"}, "state": {"y"}}); loc != "/admin/settings?git-remote=error&reason=not-configured" {
		t.Errorf("callback = %s", loc)
	}
	// only admins
	if r := e.doBody(http.MethodGet, "/admin/git-remote/", "bob", ""); r.Code != http.StatusForbidden {
		t.Errorf("editor status = %d", r.Code)
	}
}

func TestGitRemoteConnect(t *testing.T) {
	e := newSyncEnv(t)
	f := e.withGitRemote("mkdocseditor")
	s := e.gitRemoteStatus("alice")
	if s.State != GitRemoteDisconnected || *s.Provider != "forgejo" || *s.ProviderName != "Forgejo" || *s.ProviderURL != f.URL ||
		*s.GrantsURL != f.URL+"/user/settings/applications" || s.ScopedGrants || *s.ExpectedUser != "mkdocseditor" || *s.Repo != "markus/Wiki" {
		t.Fatalf("status = %+v", s)
	}

	e.reauth("alice")
	// a state nobody started, and the right state in another admin's session
	if loc := e.callback("alice", url.Values{"code": {"x"}, "state": {"forged"}}); loc != "/admin/settings?git-remote=error&reason=state" {
		t.Errorf("forged state = %s", loc)
	}
	code, state := e.startConnect(f)
	if loc := e.callback("", url.Values{"code": {code}, "state": {state}}); loc != "/admin/settings?git-remote=error&reason=session" {
		t.Errorf("without session = %s", loc)
	}
	// the state is used up
	if loc := e.callback("alice", url.Values{"code": {code}, "state": {state}}); loc != "/admin/settings?git-remote=error&reason=state" {
		t.Errorf("reused state = %s", loc)
	}

	// approved by the wrong Forgejo user
	f.Login = "markus"
	code, state = e.startConnect(f)
	if loc := e.callback("alice", url.Values{"code": {code}, "state": {state}}); loc != "/admin/settings?git-remote=error&login=markus&reason=wrong-user" {
		t.Errorf("wrong user = %s", loc)
	}
	// denied at Forgejo
	_, state = e.startConnect(f)
	if loc := e.callback("alice", url.Values{"error": {"access_denied"}, "state": {state}}); loc != "/admin/settings?git-remote=error&reason=denied" {
		t.Errorf("denied = %s", loc)
	}

	f.Login = "mkdocseditor"
	code, state = e.startConnect(f)
	if loc := e.callback("alice", url.Values{"code": {code}, "state": {state}}); loc != "/admin/settings?git-remote=connected" {
		t.Fatalf("connect = %s", loc)
	}
	s = e.gitRemoteStatus("alice")
	if s.State != GitRemoteConnected || s.RemoteUser == nil || s.RemoteUser.Login != "mkdocseditor" || s.CanPush == nil || !*s.CanPush ||
		s.ConnectedBy == nil || *s.ConnectedBy != "alice" || s.RefreshExpiresAt == nil {
		t.Fatalf("connected status = %+v", s)
	}
	// the tokens are sealed in the database, never in the answer
	if r := e.doBody(http.MethodGet, "/admin/git-remote/", "alice", ""); strings.Contains(r.Body.String(), "Token") {
		t.Errorf("answer has tokens: %s", r.Body.String())
	}

	// revoked in Forgejo: expired after the next refresh
	f.Revoke()
	if s = e.gitRemoteStatus("alice"); s.State != GitRemoteConnected || s.CanPush != nil {
		t.Errorf("after revoke, before a refresh = %+v", s)
	}

	r := e.doBody(http.MethodDelete, "/admin/git-remote/", "alice", "")
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"state": "disconnected"`) {
		t.Fatalf("disconnect = %d %s", r.Code, r.Body.String())
	}

	var types []string
	for _, ev := range e.auditEvents(t) {
		if strings.HasPrefix(ev.Type, "git.") {
			types = append(types, ev.Type)
		}
	}
	want := []string{auth.AuditGitDisconnected, auth.AuditGitConnected, auth.AuditGitConnectRefused, auth.AuditGitConnectRefused}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", types, want)
	}
}

func TestGitRemoteRefusesAdmin(t *testing.T) {
	e := newSyncEnv(t)
	f := e.withGitRemote("mkdocseditor")
	f.IsAdmin = true
	e.reauth("alice")
	code, state := e.startConnect(f)
	if loc := e.callback("alice", url.Values{"code": {code}, "state": {state}}); loc != "/admin/settings?git-remote=error&login=mkdocseditor&reason=admin-user" {
		t.Errorf("admin = %s", loc)
	}
	f.IsAdmin = false
	f.Pushers = map[string]bool{}
	code, state = e.startConnect(f)
	if loc := e.callback("alice", url.Values{"code": {code}, "state": {state}}); loc != "/admin/settings?git-remote=error&login=mkdocseditor&reason=not-pushable" {
		t.Errorf("not pushable = %s", loc)
	}
	if s := e.gitRemoteStatus("alice"); s.State != GitRemoteDisconnected {
		t.Errorf("status = %+v", s)
	}
}

// reauth confirms the password of user, for the actions that need a recent sign-in
func (e *syncEnv) reauth(user string) {
	e.t.Helper()
	if r := e.doBody(http.MethodPost, "/auth/reauth/", user, `{"password": "`+testPasswords[user]+`"}`); r.Code != http.StatusOK {
		e.t.Fatalf("reauth: %d %s", r.Code, r.Body.String())
	}
}

// auditEvents returns the audit log
func (e *syncEnv) auditEvents(t *testing.T) []*auth.AuditEntry {
	t.Helper()
	events, _, err := e.authn.AuditLog(auth.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return events
}
