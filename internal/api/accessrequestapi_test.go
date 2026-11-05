package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/gorilla/websocket"
)

func firstDocument(s *wiki.Section) *wiki.Document {
	if len(s.Documents) > 0 {
		return s.Documents[0]
	}
	for _, sub := range s.Subsections {
		if d := firstDocument(sub); d != nil {
			return d
		}
	}
	return nil
}

func (e *accountEnv) askForAccess(c *client, body map[string]any) CreatedAccessRequestResponse {
	e.t.Helper()
	r := c.do(http.MethodPost, "/auth/access-requests/", body)
	expect(e.t, r, http.StatusCreated, "")
	var created CreatedAccessRequestResponse
	r.json(e.t, &created)
	return created
}

func (e *accountEnv) accessStatus(c *client, token string) AccessRequestStatusResponse {
	e.t.Helper()
	r := c.do(http.MethodPost, "/auth/access-requests/status/", map[string]any{"token": token})
	expect(e.t, r, http.StatusOK, "")
	var status AccessRequestStatusResponse
	r.json(e.t, &status)
	return status
}

func (e *accountEnv) pendingRequests(admin *client) []AccessRequestView {
	e.t.Helper()
	r := admin.do(http.MethodGet, "/admin/access-requests/?status=pending", nil)
	expect(e.t, r, http.StatusOK, "")
	var views []AccessRequestView
	r.json(e.t, &views)
	return views
}

func TestAccessRequestFlow(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()
	inviter := e.rs.authenticator.UserByUsername("markus")
	doc := firstDocument(e.rs.treeManager.Root())
	guest := e.app()

	created := e.askForAccess(guest, map[string]any{"name": "  Anna  ", "message": "Hi,\r\nMarkus sent me a link", "documentId": doc.ID, "invitedBy": inviter.ID})
	if !strings.HasPrefix(created.Token, "mda_") || created.Status != auth.AccessRequestPending || created.RequestID == "" {
		t.Errorf("created = %+v", created)
	}
	if got := time.Until(created.ExpiresAt); got < 167*time.Hour || got > 169*time.Hour {
		t.Errorf("expires in %v", got)
	}

	if s := e.accessStatus(guest, created.Token); s.Status != auth.AccessRequestPending || s.InviteToken != "" {
		t.Errorf("status = %+v", s)
	}
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/status/", map[string]any{"token": "mda_" + strings.Repeat("A", 43)}), http.StatusBadRequest, CodeInvalidAccessRequest)
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/status/", map[string]any{"token": "nonsense"}), http.StatusBadRequest, CodeInvalidAccessRequest)

	views := e.pendingRequests(admin)
	if len(views) != 1 {
		t.Fatalf("pending = %+v", views)
	}
	v := views[0]
	if v.ID != created.RequestID || v.Name != "Anna" || v.Message != "Hi,\nMarkus sent me a link" ||
		v.DocumentID == nil || *v.DocumentID != doc.ID || v.DocumentPath == nil || *v.DocumentPath != doc.RelPath ||
		v.InvitedByAccountID == nil || *v.InvitedByAccountID != inviter.ID || v.InvitedByName == nil || *v.InvitedByName != inviter.DisplayName {
		t.Errorf("view = %+v", v)
	}

	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+v.ID+"/approve/", map[string]any{}), http.StatusBadRequest, CodeBadRequest)
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/unknown/approve/", map[string]any{"role": "editor"}), http.StatusNotFound, CodeNotFound)

	r := admin.do(http.MethodPost, "/admin/access-requests/"+v.ID+"/approve/", map[string]any{"role": "editor", "expiresInHours": 24})
	expect(t, r, http.StatusOK, "")
	var approved ApprovedAccessRequestResponse
	r.json(t, &approved)
	if approved.Request.Status != auth.AccessRequestApproved || approved.Invite.Note != "Anna" || approved.Invite.Role != auth.RoleEditor ||
		approved.Invite.Kind != auth.LinkInvite || !strings.HasPrefix(approved.Token, "mdl_") || approved.URL == nil {
		t.Errorf("approved = %s", r.body)
	}

	// decided once
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+v.ID+"/approve/", map[string]any{"role": "admin"}), http.StatusConflict, CodeConflict)
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+v.ID+"/reject/", nil), http.StatusConflict, CodeConflict)
	if len(e.pendingRequests(admin)) != 0 {
		t.Error("the request is still pending")
	}

	// the requester gets the invite with the token of the request, and redeems it normally
	s := e.accessStatus(guest, created.Token)
	if s.Status != auth.AccessRequestApproved || s.InviteToken != approved.Token {
		t.Errorf("status = %+v", s)
	}
	expect(t, guest.do(http.MethodPost, "/auth/link/redeem/", map[string]any{"token": s.InviteToken, "username": "anna",
		"password": "plum tree 42 blossoms", "sessionType": auth.SessionBearer}), http.StatusOK, "")
	if s = e.accessStatus(guest, created.Token); s.Status != auth.AccessRequestApproved || s.InviteToken != "" {
		t.Errorf("status after the redemption = %+v", s)
	}

	// the audit log knows the decision and holds no text of the requester
	r = admin.do(http.MethodGet, "/admin/audit/?limit=500", nil)
	expect(t, r, http.StatusOK, "")
	for _, want := range []string{auth.AuditAccessRequestCreated, auth.AuditAccessRequestApproved, auth.AuditInviteCreated} {
		if !strings.Contains(string(r.body), want) {
			t.Errorf("no %s in the audit log", want)
		}
	}
	if strings.Contains(string(r.body), "Markus sent me") {
		t.Error("the message is in the audit log")
	}
}

// the answer must not reveal whether the claimed document or account exist
func TestAccessRequestClaimsAreNotChecked(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()
	inviter := e.rs.authenticator.UserByUsername("markus")
	doc := firstDocument(e.rs.treeManager.Root())
	guest := e.app()

	keys := func(body map[string]any) string {
		r := guest.do(http.MethodPost, "/auth/access-requests/", body)
		expect(t, r, http.StatusCreated, "")
		var m map[string]any
		r.json(t, &m)
		var names []string
		for k := range m {
			names = append(names, k)
		}
		slices.Sort(names)
		return strings.Join(names, ",")
	}
	known := keys(map[string]any{"name": "a", "documentId": doc.ID, "invitedBy": inviter.ID})
	unknown := keys(map[string]any{"name": "b", "documentId": "nope", "invitedBy": "nobody"})
	if known != unknown || known != "expiresAt,requestId,status,token" {
		t.Errorf("known = %s, unknown = %s", known, unknown)
	}

	for _, v := range e.pendingRequests(admin) {
		if v.Name == "b" && (v.DocumentID != nil || v.DocumentPath != nil || v.InvitedByAccountID != nil || v.InvitedByName != nil) {
			t.Errorf("unknown claims resolved: %+v", v)
		}
		if v.Name == "a" && (v.DocumentID == nil || v.InvitedByName == nil) {
			t.Errorf("known claims not resolved: %+v", v)
		}
	}
}

func TestAccessRequestValidation(t *testing.T) {
	e := newAccountEnv(t)
	e.setupAdmin()
	guest := e.app()
	for name, body := range map[string]map[string]any{
		"no name":      {"message": "hi"},
		"blank name":   {"name": "   "},
		"long name":    {"name": strings.Repeat("x", 81)},
		"line break":   {"name": "a\nb"},
		"long message": {"name": "a", "message": strings.Repeat("x", 501)},
		"control":      {"name": "a", "message": "a\x1bb"},
		"long claim":   {"name": "a", "documentId": strings.Repeat("x", 129)},
	} {
		if r := guest.do(http.MethodPost, "/auth/access-requests/", body); r.status != http.StatusBadRequest {
			t.Errorf("%s: status = %d", name, r.status)
		}
	}
	e.askForAccess(guest, map[string]any{"name": strings.Repeat("ü", 80), "message": strings.Repeat("ü", 500)})
}

func TestAccessRequestDecisionsAndExpiry(t *testing.T) {
	e := newAccountEnv(t)
	clk := e.clock()
	admin := e.setupAdmin()
	guest := e.app()

	rejected := e.askForAccess(guest, map[string]any{"name": "rejected"})
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+rejected.RequestID+"/reject/", nil), http.StatusNoContent, "")
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+rejected.RequestID+"/reject/", nil), http.StatusNoContent, "")
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+rejected.RequestID+"/approve/", map[string]any{"role": "reader"}), http.StatusConflict, CodeConflict)
	if s := e.accessStatus(guest, rejected.Token); s.Status != auth.AccessRequestRejected || s.InviteToken != "" {
		t.Errorf("rejected = %+v", s)
	}

	// a revoked invite ends the request
	revoked := e.askForAccess(guest, map[string]any{"name": "revoked"})
	r := admin.do(http.MethodPost, "/admin/access-requests/"+revoked.RequestID+"/approve/", map[string]any{"role": "reader"})
	expect(t, r, http.StatusOK, "")
	var approved ApprovedAccessRequestResponse
	r.json(t, &approved)
	expect(t, admin.do(http.MethodDelete, "/admin/invites/"+approved.Invite.ID+"/", nil), http.StatusNoContent, "")
	if s := e.accessStatus(guest, revoked.Token); s.Status != auth.AccessRequestRejected || s.InviteToken != "" {
		t.Errorf("revoked = %+v", s)
	}

	// so does an invite that expires
	shortInvite := e.askForAccess(guest, map[string]any{"name": "short"})
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+shortInvite.RequestID+"/approve/", map[string]any{"role": "reader", "expiresInHours": 1}), http.StatusOK, "")
	waiting := e.askForAccess(guest, map[string]any{"name": "waiting"})
	clk.advance(2 * time.Hour)
	if s := e.accessStatus(guest, shortInvite.Token); s.Status != auth.AccessRequestExpired || s.InviteToken != "" {
		t.Errorf("short invite = %+v", s)
	}

	// a request nobody decides expires
	clk.advance(7 * 24 * time.Hour)
	if s := e.accessStatus(guest, waiting.Token); s.Status != auth.AccessRequestExpired {
		t.Errorf("waiting = %+v", s)
	}
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+waiting.RequestID+"/approve/", map[string]any{"role": "reader"}), http.StatusConflict, CodeConflict)
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+waiting.RequestID+"/reject/", nil), http.StatusConflict, CodeConflict)
	if len(e.pendingRequests(admin)) != 0 {
		t.Error("an expired request is pending")
	}
	r = admin.do(http.MethodGet, "/admin/access-requests/?status=expired", nil)
	expect(t, r, http.StatusOK, "")
	var expired []AccessRequestView
	r.json(t, &expired)
	if len(expired) != 2 {
		t.Errorf("expired = %s", r.body)
	}
	expect(t, admin.do(http.MethodGet, "/admin/access-requests/?status=nonsense", nil), http.StatusBadRequest, CodeBadRequest)
}

func TestAccessRequestLimitOfPending(t *testing.T) {
	e := newAccountEnv(t)
	clk := e.clock()
	admin := e.setupAdmin()
	guest := e.app()
	var first CreatedAccessRequestResponse
	for i := range 50 {
		created := e.askForAccess(guest, map[string]any{"name": fmt.Sprintf("n%d", i)})
		if i == 0 {
			first = created
		}
	}
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/", map[string]any{"name": "too many"}), http.StatusTooManyRequests, CodeTooManyAccessRequests)

	// a decision frees a place, and so does the expiry
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+first.RequestID+"/reject/", nil), http.StatusNoContent, "")
	e.askForAccess(guest, map[string]any{"name": "one more"})
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/", map[string]any{"name": "too many"}), http.StatusTooManyRequests, CodeTooManyAccessRequests)
	clk.advance(7*24*time.Hour + time.Minute)
	e.askForAccess(guest, map[string]any{"name": "after the expiry"})
}

func TestAccessRequestAvailability(t *testing.T) {
	e := newAccountEnv(t)
	guest := e.app()
	features := func() []string {
		r := guest.do(http.MethodGet, "/server-info/", nil)
		expect(t, r, http.StatusOK, "")
		var info ServerInfoResponse
		r.json(t, &info)
		return info.Features
	}

	// nobody could answer before the setup
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/", map[string]any{"name": "a"}), http.StatusForbidden, CodeAccessRequestsDisabled)
	if slices.Contains(features(), FeatureAccessRequests) {
		t.Error("offered before the setup")
	}

	admin := e.setupAdmin()
	if f := features(); !slices.Contains(f, FeatureAccessRequests) || !slices.Contains(f, FeatureJoinPresence) {
		t.Errorf("features = %v", f)
	}
	created := e.askForAccess(guest, map[string]any{"name": "a"})

	// the public status says nothing about pending requests
	for _, path := range []string{"/server-info/", "/auth/status/"} {
		r := guest.do(http.MethodGet, path, nil)
		if strings.Contains(strings.ToLower(string(r.body)), "pending") {
			t.Errorf("%s reveals the pending requests: %s", path, r.body)
		}
	}

	expect(t, admin.do(http.MethodPost, "/auth/reauth/", map[string]any{"password": "a long admin secret"}), http.StatusOK, "")
	r := admin.do(http.MethodPatch, "/admin/settings/", map[string]any{"accessRequestsEnabled": false})
	expect(t, r, http.StatusOK, "")
	var settings map[string]any
	r.json(t, &settings)
	if settings["accessRequestsEnabled"] != false {
		t.Errorf("settings = %s", r.body)
	}
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/", map[string]any{"name": "b"}), http.StatusForbidden, CodeAccessRequestsDisabled)
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/status/", map[string]any{"token": created.Token}), http.StatusForbidden, CodeAccessRequestsDisabled)
	if slices.Contains(features(), FeatureAccessRequests) {
		t.Error("offered while disabled")
	}
	// open requests can still be decided
	if len(e.pendingRequests(admin)) != 1 {
		t.Error("the open request is gone")
	}
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+created.RequestID+"/reject/", nil), http.StatusNoContent, "")

	r = admin.do(http.MethodPatch, "/admin/settings/", map[string]any{"accessRequestsEnabled": true})
	expect(t, r, http.StatusOK, "")
	e.askForAccess(guest, map[string]any{"name": "c"})
}

func TestAccessRequestAdminOnly(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()
	editor := e.invite(admin, auth.RoleEditor, "anna", false)
	created := e.askForAccess(e.app(), map[string]any{"name": "a"})

	// an app token of an admin is not enough, as for every /admin/ endpoint
	r := admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "script", "scope": "write"})
	expect(t, r, http.StatusOK, "")
	var token struct {
		Token string `json:"token"`
	}
	r.json(t, &token)
	script := e.app()
	script.bearer = token.Token

	for name, c := range map[string]*client{"editor": editor, "app token": script} {
		for _, call := range []struct {
			method, path string
		}{
			{http.MethodGet, "/admin/access-requests/"},
			{http.MethodPost, "/admin/access-requests/" + created.RequestID + "/approve/"},
			{http.MethodPost, "/admin/access-requests/" + created.RequestID + "/reject/"},
		} {
			if r := c.do(call.method, call.path, map[string]any{"role": "admin"}); r.status != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d", name, call.method, call.path, r.status)
			}
		}
	}
	if r := e.app().do(http.MethodGet, "/admin/access-requests/", nil); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", r.status)
	}
}

func TestAccessRequestRateLimits(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.Auth.RateLimits = configuration.RateLimitConfiguration{}
	})
	e.setupAdmin()
	guest := e.app()
	var created CreatedAccessRequestResponse
	for i := range 5 {
		created = e.askForAccess(guest, map[string]any{"name": fmt.Sprintf("n%d", i)})
	}
	r := guest.do(http.MethodPost, "/auth/access-requests/", map[string]any{"name": "sixth"})
	expect(t, r, http.StatusTooManyRequests, CodeRateLimited)
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}

	// the status has a bucket of its own
	for range 10 {
		e.accessStatus(guest, created.Token)
	}
	expect(t, guest.do(http.MethodPost, "/auth/access-requests/status/", map[string]any{"token": created.Token}), http.StatusTooManyRequests, CodeRateLimited)

	// and neither touches the login
	expect(t, guest.do(http.MethodPost, "/auth/login/", map[string]any{"username": "markus", "password": "wrong"}), http.StatusUnauthorized, CodeInvalidCredentials)
}

// accessRequestListener collects the access-requests messages of an events websocket
type accessRequestListener struct {
	mu     sync.Mutex
	counts []int
}

func (l *accessRequestListener) last() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.counts) == 0 {
		return -1
	}
	return l.counts[len(l.counts)-1]
}

func (e *accountEnv) listenForAccessRequests(bearer string) *accessRequestListener {
	e.t.Helper()
	ws, err := e.dialEvents(http.Header{"Authorization": {"Bearer " + bearer}}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = ws.Close() })
	l := &accessRequestListener{}
	go readAccessRequests(ws, l)
	return l
}

func readAccessRequests(ws *websocket.Conn, l *accessRequestListener) {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m collab.AccessRequestsMessage
		if json.Unmarshal(data, &m) == nil && m.Type == collab.MsgAccessRequests {
			l.mu.Lock()
			l.counts = append(l.counts, m.Pending)
			l.mu.Unlock()
		}
	}
}

func TestAccessRequestNotification(t *testing.T) {
	e := newAccountEnv(t)
	e.rs.sync.HeartbeatTimeout = 30 * time.Second
	clk := e.clock()
	admin := e.setupAdmin()
	adminApp := e.app()
	adminApp.login("markus", "a long admin secret")
	editor := e.invite(admin, auth.RoleEditor, "anna", false)
	reader := e.invite(admin, auth.RoleReader, "kid", false)

	r := admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "script", "scope": "write"})
	expect(t, r, http.StatusOK, "")
	var token struct {
		Token string `json:"token"`
	}
	r.json(t, &token)

	guest := e.app()
	first := e.askForAccess(guest, map[string]any{"name": "a"})

	listener := e.listenForAccessRequests(adminApp.bearer)
	eventually(t, "the count after hello", func() bool { return listener.last() == 1 })

	others := []*accessRequestListener{
		e.listenForAccessRequests(editor.bearer),
		e.listenForAccessRequests(reader.bearer),
		e.listenForAccessRequests(token.Token),
	}

	second := e.askForAccess(guest, map[string]any{"name": "b"})
	eventually(t, "the new request", func() bool { return listener.last() == 2 })
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+first.RequestID+"/reject/", nil), http.StatusNoContent, "")
	eventually(t, "the rejection", func() bool { return listener.last() == 1 })
	expect(t, admin.do(http.MethodPost, "/admin/access-requests/"+second.RequestID+"/approve/", map[string]any{"role": "reader"}), http.StatusOK, "")
	eventually(t, "the approval", func() bool { return listener.last() == 0 })

	// an expired request does not count
	e.askForAccess(guest, map[string]any{"name": "c"})
	eventually(t, "the third request", func() bool { return listener.last() == 1 })
	clk.advance(7*24*time.Hour + time.Minute)
	e.askForAccess(guest, map[string]any{"name": "d"})
	e.askForAccess(guest, map[string]any{"name": "e"})
	eventually(t, "only the newer requests", func() bool { return listener.last() == 2 })

	for i, l := range others {
		if got := l.last(); got != -1 {
			t.Errorf("listener %d got an access request count: %d", i, got)
		}
	}
}
