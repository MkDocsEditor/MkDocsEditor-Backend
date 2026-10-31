package backend

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

func adminUserByName(t *testing.T, admin *client, username string) auth.AdminUser {
	t.Helper()
	r := admin.do(http.MethodGet, "/admin/users/", nil)
	expect(t, r, http.StatusOK, "")
	var users []auth.AdminUser
	r.json(t, &users)
	for _, u := range users {
		if u.Username == username {
			return u
		}
	}
	t.Fatalf("no user %s in %s", username, r.body)
	return auth.AdminUser{}
}

func TestAdminUsers(t *testing.T) {
	e := newAccountEnv(t)
	e.rs.sync.HeartbeatTimeout = 30 * time.Second
	clk := e.clock()
	admin := e.setupAdmin()
	anna := e.invite(admin, auth.RoleEditor, "anna", false)
	kid := e.invite(admin, auth.RoleReader, "kid", true)

	// non-admins get nothing
	expect(t, anna.do(http.MethodGet, "/admin/users/", nil), http.StatusForbidden, CodeForbidden)

	u := adminUserByName(t, admin, "anna")
	if u.Role != auth.RoleEditor || u.Status != auth.StatusActive || !u.HasPassword || u.SessionCount != 1 || u.LastLoginAt.IsZero() {
		t.Errorf("anna = %+v", u)
	}
	me := adminUserByName(t, admin, "markus")

	// guards: no self demotion, no disabling or deleting oneself
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+me.ID+"/", map[string]any{"role": "editor"}), http.StatusConflict, CodeConflict)
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+me.ID+"/", map[string]any{"status": "disabled"}), http.StatusConflict, CodeConflict)
	expect(t, admin.do(http.MethodDelete, "/admin/users/"+me.ID+"/", nil), http.StatusConflict, CodeConflict)
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+u.ID+"/", map[string]any{"role": "root"}), http.StatusBadRequest, CodeBadRequest)
	expect(t, admin.do(http.MethodPatch, "/admin/users/nope/", map[string]any{"displayName": "x"}), http.StatusNotFound, CodeNotFound)

	// role changes need a step-up and close the user's websockets with 4003
	ws, err := e.dialEvents(http.Header{"Authorization": {"Bearer " + anna.bearer}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectOnline(t, ws)
	clk.advance(11 * time.Minute)
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+u.ID+"/", map[string]any{"role": "admin"}), http.StatusForbidden, CodeReauthRequired)
	r := admin.do(http.MethodPatch, "/admin/users/"+u.ID+"/", map[string]any{"displayName": "Anna B."})
	expect(t, r, http.StatusOK, "")
	expect(t, admin.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"}), http.StatusOK, "")
	r = admin.do(http.MethodPatch, "/admin/users/"+u.ID+"/", map[string]any{"role": "admin"})
	expect(t, r, http.StatusOK, "")
	var updated auth.AdminUser
	r.json(t, &updated)
	if updated.Role != auth.RoleAdmin || updated.DisplayName != "Anna B." {
		t.Errorf("updated = %s", r.body)
	}
	if _, code := readUntilClose(t, ws, 5*time.Second); code != CloseForbidden {
		t.Errorf("after the role change: close code %d", code)
	}
	// the session stays and has the new role
	r = anna.do(http.MethodGet, "/auth/whoami/", nil)
	var who WhoAmIResponse
	r.json(t, &who)
	if who.Role != auth.RoleAdmin {
		t.Errorf("whoami = %s", r.body)
	}

	// demoting another admin works
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+u.ID+"/", map[string]any{"role": "editor"}), http.StatusOK, "")

	// disabling revokes sessions; enabling does not bring them back
	k := adminUserByName(t, admin, "kid")
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+k.ID+"/", map[string]any{"status": "disabled"}), http.StatusOK, "")
	expect(t, kid.do(http.MethodGet, "/auth/whoami/", nil), http.StatusUnauthorized, CodeUnauthenticated)
	expect(t, kid.do(http.MethodPost, "/auth/login/", map[string]any{"username": "kid", "password": "plum tree 42 blossoms"}), http.StatusForbidden, CodeAccountDisabled)
	expect(t, admin.do(http.MethodPost, "/admin/users/"+k.ID+"/reset-link/", map[string]any{}), http.StatusForbidden, CodeAccountDisabled)
	expect(t, admin.do(http.MethodPatch, "/admin/users/"+k.ID+"/", map[string]any{"status": "active"}), http.StatusOK, "")
	kid.login("kid", "plum tree 42 blossoms")

	// sessions of a user
	r = admin.do(http.MethodGet, "/admin/users/"+u.ID+"/sessions/", nil)
	var sessions []auth.Session
	r.json(t, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %s", r.body)
	}
	expect(t, admin.do(http.MethodDelete, "/admin/sessions/"+sessions[0].ID+"/", nil), http.StatusNoContent, "")
	expect(t, anna.do(http.MethodGet, "/auth/whoami/", nil), http.StatusUnauthorized, CodeUnauthenticated)
	r = admin.do(http.MethodPost, "/admin/users/"+k.ID+"/sessions/revoke/", nil)
	var revoked RevokedResponse
	r.json(t, &revoked)
	if revoked.Revoked != 1 {
		t.Errorf("revoked = %s", r.body)
	}

	// reset links
	r = admin.do(http.MethodPost, "/admin/users/"+u.ID+"/reset-link/", map[string]any{"expiresInMinutes": 30})
	expect(t, r, http.StatusOK, "")
	var created CreatedLinkResponse
	r.json(t, &created)
	if created.Link == nil || created.URL == nil || !strings.HasPrefix(*created.URL, testWebOrigin+"/reset#mdl_") {
		t.Fatalf("reset link = %s", r.body)
	}
	expect(t, admin.do(http.MethodPost, "/admin/users/"+u.ID+"/reset-link/", map[string]any{"expiresInMinutes": 5000}), http.StatusBadRequest, CodeBadRequest)
	r = admin.do(http.MethodGet, "/admin/reset-links/?status=pending", nil)
	var links []auth.Link
	r.json(t, &links)
	if len(links) != 1 || links[0].ID != created.Link.ID {
		t.Errorf("reset links = %s", r.body)
	}
	expect(t, admin.do(http.MethodDelete, "/admin/reset-links/"+created.Link.ID+"/", nil), http.StatusNoContent, "")
	expect(t, e.app().do(http.MethodPost, "/auth/link/inspect/", map[string]any{"token": created.Token}), http.StatusBadRequest, CodeInvalidLink)

	// deletion leaves a tombstone
	expect(t, admin.do(http.MethodDelete, "/admin/users/"+u.ID+"/", nil), http.StatusNoContent, "")
	r = admin.do(http.MethodGet, "/admin/users/?status=deleted", nil)
	var deleted []auth.AdminUser
	r.json(t, &deleted)
	if len(deleted) != 1 || deleted[0].Username != "anna" || deleted[0].HasPassword || deleted[0].Email != "" {
		t.Errorf("deleted = %s", r.body)
	}
	expect(t, admin.do(http.MethodDelete, "/admin/users/"+u.ID+"/", nil), http.StatusNotFound, CodeNotFound)
	expect(t, e.app().do(http.MethodPost, "/auth/login/", map[string]any{"username": "anna", "password": "plum tree 42 blossoms"}),
		http.StatusUnauthorized, CodeInvalidCredentials)
	if err := e.rs.authenticator.UsernameAvailable("anna"); err == nil {
		t.Error("the username of a deleted account is free again")
	}
}

func TestAdminSettingsAndAudit(t *testing.T) {
	e := newAccountEnv(t)
	clk := e.clock()
	admin := e.setupAdmin()

	r := admin.do(http.MethodGet, "/admin/settings/", nil)
	var settings map[string]any
	r.json(t, &settings)
	if len(settings) != 1 || settings["require2FA"] != false {
		t.Errorf("settings = %s", r.body)
	}
	clk.advance(11 * time.Minute)
	expect(t, admin.do(http.MethodPatch, "/admin/settings/", map[string]any{"require2FA": true}), http.StatusForbidden, CodeReauthRequired)
	expect(t, admin.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"}), http.StatusOK, "")
	r = admin.do(http.MethodPatch, "/admin/settings/", map[string]any{"require2FA": true})
	expect(t, r, http.StatusOK, "")
	if !strings.Contains(string(r.body), `"require2FA": true`) || !e.rs.authenticator.Require2FA() {
		t.Errorf("settings = %s", r.body)
	}
	// the admin has no second factor now: restricted, the settings are out of reach
	expect(t, admin.do(http.MethodGet, "/admin/settings/", nil), http.StatusForbidden, CodeSecondFactorSetupRequired)
	_ = e.rs.authenticator.SetRequire2FA(false, nil, auth.RequestInfo{})

	for i := 0; i < 3; i++ {
		e.app().do(http.MethodPost, "/auth/login/", map[string]any{"username": "markus", "password": fmt.Sprintf("wrong password %d", i)})
	}
	r = admin.do(http.MethodGet, "/admin/audit/?type=login.failed&limit=2", nil)
	expect(t, r, http.StatusOK, "")
	var page AuditResponse
	r.json(t, &page)
	if len(page.Events) != 2 || page.Next == nil || page.Events[0].Type != auth.AuditLoginFailed || page.Events[0].Target == nil ||
		page.Events[0].Target.Username != "markus" || page.Events[0].IP == "" {
		t.Fatalf("audit = %s", r.body)
	}
	r = admin.do(http.MethodGet, fmt.Sprintf("/admin/audit/?type=login.failed&limit=2&before=%d", *page.Next), nil)
	var rest AuditResponse
	r.json(t, &rest)
	if len(rest.Events) != 1 || rest.Next != nil || rest.Events[0].ID >= *page.Next {
		t.Errorf("second page = %s", r.body)
	}
	me := adminUserByName(t, admin, "markus")
	r = admin.do(http.MethodGet, "/admin/audit/?account="+me.ID, nil)
	var mine AuditResponse
	r.json(t, &mine)
	types := map[string]bool{}
	for _, ev := range mine.Events {
		types[ev.Type] = true
	}
	for _, want := range []string{auth.AuditSetupCompleted, auth.AuditAccountCreated, auth.AuditReauthSucceeded, auth.AuditSettingsChanged, auth.AuditLoginFailed} {
		if !types[want] {
			t.Errorf("audit of the admin misses %s: %v", want, types)
		}
	}
	expect(t, admin.do(http.MethodGet, "/admin/audit/?limit=501", nil), http.StatusBadRequest, CodeBadRequest)
}

func TestAppTokens(t *testing.T) {
	e := newAccountEnv(t)
	e.rs.sync.HeartbeatTimeout = 30 * time.Second
	clk := e.clock()
	admin := e.setupAdmin()

	expect(t, admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "ci", "scope": "admin"}), http.StatusBadRequest, CodeBadRequest)
	r := admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "ci", "scope": "write", "expiresInDays": 30})
	expect(t, r, http.StatusOK, "")
	var write CreatedAppTokenResponse
	r.json(t, &write)
	if !strings.HasPrefix(write.Token, "mdp_") || write.AppToken.ExpiresAt == nil || write.AppToken.Scope != "write" {
		t.Fatalf("token = %s", r.body)
	}
	r = admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "backup", "scope": "read"})
	var read CreatedAppTokenResponse
	r.json(t, &read)
	if read.AppToken.ExpiresAt != nil {
		t.Errorf("read token = %s", r.body)
	}

	w := &client{env: e, bearer: write.Token}
	ro := &client{env: e, bearer: read.Token}
	r = w.do(http.MethodGet, "/auth/whoami/", nil)
	var who WhoAmIResponse
	r.json(t, &who)
	if who.AuthMethod != AuthMethodAppToken || who.Role != auth.RoleAdmin {
		t.Errorf("whoami = %s", r.body)
	}
	// app tokens reach content, never the account or admin API
	expect(t, w.do(http.MethodGet, "/actors/", nil), http.StatusOK, "")
	expect(t, w.do(http.MethodGet, "/account/", nil), http.StatusForbidden, CodeForbidden)
	expect(t, w.do(http.MethodGet, "/admin/users/", nil), http.StatusForbidden, CodeForbidden)
	expect(t, w.do(http.MethodPost, "/auth/ws-ticket/", nil), http.StatusOK, "")
	// read scope: reader rights, no tickets
	r = ro.do(http.MethodGet, "/auth/whoami/", nil)
	r.json(t, &who)
	if who.Role != auth.RoleReader {
		t.Errorf("read token whoami = %s", r.body)
	}
	expect(t, ro.do(http.MethodGet, "/actors/", nil), http.StatusOK, "")
	expect(t, ro.do(http.MethodPost, "/auth/ws-ticket/", nil), http.StatusForbidden, CodeForbidden)

	// basic auth with username and app token
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/auth/whoami/", nil)
	req.SetBasicAuth("markus", write.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("basic auth with app token: %d", resp.StatusCode)
	}
	req.SetBasicAuth("someone", write.Token)
	if resp, err = http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("app token with another username: %d", resp.StatusCode)
		}
	}

	// list, expiry, revoke
	r = admin.do(http.MethodGet, "/account/tokens/", nil)
	var tokens []auth.AppToken
	r.json(t, &tokens)
	if len(tokens) != 2 || tokens[0].Name != "backup" || tokens[1].LastUsedAt == nil {
		t.Errorf("tokens = %s", r.body)
	}
	expect(t, admin.do(http.MethodDelete, "/account/tokens/"+read.AppToken.ID+"/", nil), http.StatusNoContent, "")
	expect(t, ro.do(http.MethodGet, "/auth/whoami/", nil), http.StatusUnauthorized, CodeUnauthenticated)
	clk.advance(31 * 24 * time.Hour)
	expect(t, w.do(http.MethodGet, "/auth/whoami/", nil), http.StatusUnauthorized, CodeUnauthenticated)

	// a password change ends all app tokens
	admin.login("markus", "a long admin secret")
	r = admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "again", "scope": "write"})
	r.json(t, &write)
	w.bearer = write.Token
	expect(t, w.do(http.MethodGet, "/auth/whoami/", nil), http.StatusOK, "")
	expect(t, admin.do(http.MethodPut, "/account/password/", map[string]any{"currentPassword": "a long admin secret", "newPassword": "a fresh admin phrase"}),
		http.StatusNoContent, "")
	expect(t, w.do(http.MethodGet, "/auth/whoami/", nil), http.StatusUnauthorized, CodeUnauthenticated)
}
