package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

// clock is a settable time source for tests
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func ri() RequestInfo {
	return RequestInfo{IP: "192.0.2.1", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0"}
}
func sp(sessionType string) SessionParams { return SessionParams{Type: sessionType, RequestInfo: ri()} }

func countAudit(t *testing.T, s *Service, typ string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE type = ?", typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// setupService creates a service in setup mode with an admin "admin" created through the setup link
func setupService(t *testing.T, conf configuration.AuthConfiguration) (*Service, *clock, *User) {
	t.Helper()
	s := openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: conf})
	c := newClock()
	s.now = c.now
	_, token, err := s.CreateSetupLink()
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RedeemLink(token, NewAccount{Username: "admin", DisplayName: "The Admin", Password: "boss secret 2026!"}, sp(SessionBearer))
	if err != nil {
		t.Fatal(err)
	}
	return s, c, r.User
}

func TestCheckPassword(t *testing.T) {
	cases := map[string]string{
		"short":                         WeakTooShort,
		strings.Repeat("x", 257):        WeakTooLong,
		"aaaaaaaaaaaa":                  WeakRepetitive,
		"1234567890":                    WeakRepetitive,
		"9876543210":                    WeakRepetitive,
		"abcabcabcabc":                  WeakRepetitive,
		"password123":                   WeakCommon,
		"PASSWORD123":                   WeakCommon,
		"qwertyuiop":                    WeakCommon,
		"markus-secret-2026":            WeakContainsUsername,
		"my MkDocsEditor pass":          WeakContainsUsername,
		"correct horse battery staple!": "",
		"ünïcödé pässwörd ok":           "",
		"ten chars!":                    "",
	}
	for pw, want := range cases {
		err := CheckPassword(pw, "markus")
		var weak *WeakPasswordError
		switch {
		case want == "" && err != nil:
			t.Errorf("%q rejected: %v", pw, err)
		case want != "" && (!errors.As(err, &weak) || weak.Reason != want):
			t.Errorf("%q: %v, want %s", pw, err, want)
		}
	}
	// 256 characters are fine, counted in code points
	if err := CheckPassword(strings.Repeat("ä1b2", 64), ""); err != nil {
		t.Errorf("256 code points: %v", err)
	}
	if len(commonPasswords()) < 100000 {
		t.Errorf("blocklist has only %d entries", len(commonPasswords()))
	}
}

func TestSetupAndSessions(t *testing.T) {
	s := openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: configuration.AuthConfiguration{ClientURL: "https://edit.example.com/"}})
	c := newClock()
	s.now = c.now
	if !s.SetupRequired() {
		t.Fatal("setup not required")
	}
	_, old, err := s.CreateSetupLink()
	if err != nil {
		t.Fatal(err)
	}
	l, token, err := s.CreateSetupLink()
	if err != nil || l.Kind != LinkSetup || !strings.HasPrefix(token, PrefixLink) {
		t.Fatal(l, err)
	}
	if u := s.LinkURL(LinkSetup, token); u == nil || *u != "https://edit.example.com/setup#"+token {
		t.Errorf("url = %v", u)
	}
	// a newer setup link revokes the older one
	if _, _, err = s.InspectLink(old); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("old setup link: %v", err)
	}
	if insp, _, err := s.InspectLink(token); err != nil || insp.Role != RoleAdmin {
		t.Fatal(insp, err)
	}
	// rules are checked before anything is created
	if _, err = s.RedeemLink(token, NewAccount{Username: "bad name", Password: "a good long password"}, sp("")); !errors.Is(err, ErrInvalidUsername) {
		t.Errorf("bad username: %v", err)
	}
	if _, err = s.RedeemLink(token, NewAccount{Username: "markus", Password: "short"}, sp("")); err == nil {
		t.Error("weak password accepted")
	}
	r, err := s.RedeemLink(token, NewAccount{Username: "markus", DisplayName: " Markus ", Password: "a good long password"}, sp(""))
	if err != nil {
		t.Fatal(err)
	}
	if r.User.Role != RoleAdmin || r.User.DisplayName != "Markus" || r.Session.Type != SessionCookie || !ValidSessionToken(r.Token) {
		t.Fatalf("result = %+v %+v", r.User, r.Session)
	}
	if r.Session.DeviceName != "Firefox on Linux" {
		t.Errorf("device = %q", r.Session.DeviceName)
	}
	if s.SetupRequired() {
		t.Error("still in setup mode")
	}
	if _, err = s.RedeemLink(token, NewAccount{Username: "other", Password: "a good long password"}, sp("")); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("setup link reused: %v", err)
	}
	if _, _, err = s.CreateSetupLink(); !errors.Is(err, ErrSetupDone) {
		t.Errorf("setup link after setup: %v", err)
	}
	if countAudit(t, s, AuditSetupCompleted) != 1 || countAudit(t, s, AuditAccountCreated) != 1 {
		t.Error("audit events missing")
	}

	// the session works and slides
	u, sess, err := s.AuthenticateSession(r.Token, "192.0.2.9")
	if err != nil || u.Username != "markus" || sess.ID != r.Session.ID {
		t.Fatal(u, err)
	}
	c.advance(DefaultSessionIdleTimeout - time.Hour)
	if _, sess, err = s.AuthenticateSession(r.Token, ""); err != nil {
		t.Fatal("session expired too early", err)
	}
	if !sess.IdleExpiresAt.Equal(c.now().Add(DefaultSessionIdleTimeout)) {
		t.Errorf("idle expiry not extended: %v", sess.IdleExpiresAt)
	}
	// absolute lifetime
	for i := 0; i < 4; i++ {
		c.advance(DefaultSessionIdleTimeout - time.Hour)
		if _, _, err = s.AuthenticateSession(r.Token, ""); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrUnauthenticated) || !c.now().After(r.Session.CreatedAt.Add(DefaultSessionMaxLifetime)) {
		t.Errorf("absolute lifetime: %v at %v", err, c.now())
	}

	// idle expiry
	lr, err := s.LoginPassword("markus", "a good long password", sp(SessionBearer))
	if err != nil {
		t.Fatal(err)
	}
	c.advance(DefaultSessionIdleTimeout)
	if _, _, err = s.AuthenticateSession(lr.Token, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("idle session accepted: %v", err)
	}
	for _, bad := range []string{"", "mds_", "mds_" + strings.Repeat("A", 42) + "!", "mdp_" + strings.Repeat("A", 43), strings.Repeat("A", 47)} {
		if _, _, err = s.AuthenticateSession(bad, ""); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestLoginAndRevocation(t *testing.T) {
	s, c, admin := setupService(t, configuration.AuthConfiguration{})
	var events []RevokeEvent
	s.OnRevoke(func(e RevokeEvent) { events = append(events, e) })

	if _, err := s.LoginPassword("admin", "wrong password", sp("")); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := s.LoginPassword("nobody", "boss secret 2026!", sp("")); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user: %v", err)
	}
	if countAudit(t, s, AuditLoginFailed) != 2 {
		t.Error("failed logins not audited")
	}
	// usernames are case-insensitive
	a, err := s.LoginPassword("ADMIN", "boss secret 2026!", SessionParams{Type: SessionBearer, DeviceName: "Pixel 8", RequestInfo: ri()})
	if err != nil || a.Session.DeviceName != "Pixel 8" {
		t.Fatal(a, err)
	}
	b, _ := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	sessions, err := s.Sessions(admin.ID)
	if err != nil || len(sessions) != 3 {
		t.Fatalf("sessions = %d, %v", len(sessions), err)
	}
	if err = s.RevokeSession("someone-else", a.Session.ID, admin, ri()); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke of a foreign session: %v", err)
	}
	if err = s.RevokeSession(admin.ID, a.Session.ID, admin, ri()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AuthenticateSession(a.Token, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Error("revoked session works")
	}
	n, err := s.RevokeSessions(admin.ID, b.Session.ID, admin, ri())
	if err != nil || n != 1 {
		t.Errorf("revoke others = %d, %v", n, err)
	}
	if _, _, err = s.AuthenticateSession(b.Token, ""); err != nil {
		t.Error("kept session revoked")
	}
	s.Logout(b.Session, ri())
	if _, _, err = s.AuthenticateSession(b.Token, ""); err == nil {
		t.Error("session works after logout")
	}
	if len(events) != 3 || events[0].SessionID != a.Session.ID || events[1].ExceptSessionID != b.Session.ID {
		t.Errorf("events = %+v", events)
	}

	// tickets of an ended session are refused
	d, _ := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	ticket, _, _ := s.IssueTicket(d.User, d.Session.ID)
	s.Logout(d.Session, ri())
	if _, _, err = s.RedeemTicket(ticket); err == nil {
		t.Error("ticket of an ended session accepted")
	}
	_ = c
}

func TestAccountRateLimit(t *testing.T) {
	s, c, _ := setupService(t, configuration.AuthConfiguration{})
	for i := 0; i < 5; i++ {
		if _, err := s.LoginPassword("admin", "wrong password", sp("")); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// the sixth attempt waits 1 s, even with the right password
	_, err := s.LoginPassword("admin", "boss secret 2026!", sp(""))
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Seconds() != 1 {
		t.Fatalf("expected a rate limit, got %v", err)
	}
	c.advance(time.Second)
	if _, err = s.LoginPassword("admin", "wrong password", sp("")); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	// backoff doubles
	if _, err = s.LoginPassword("admin", "boss secret 2026!", sp("")); !errors.As(err, &rl) || rl.Seconds() != 2 {
		t.Fatalf("expected 2 s, got %v", err)
	}
	c.advance(2 * time.Second)
	if _, err = s.LoginPassword("admin", "boss secret 2026!", sp("")); err != nil {
		t.Fatal(err)
	}
	// a success resets the counter
	if _, err = s.LoginPassword("admin", "wrong password", sp("")); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	// unknown usernames are limited the same way
	for i := 0; i < 5; i++ {
		_, _ = s.LoginPassword("ghost", "x", sp(""))
	}
	if _, err = s.LoginPassword("ghost", "x", sp("")); !errors.As(err, &rl) {
		t.Errorf("unknown user not limited: %v", err)
	}
	// the cap
	for i := 0; i < 30; i++ {
		c.advance(time.Hour)
		_, _ = s.LoginPassword("ghost", "x", sp(""))
	}
	if _, err = s.LoginPassword("ghost", "x", sp("")); !errors.As(err, &rl) || rl.RetryAfter > 15*time.Minute {
		t.Errorf("cap: %v", err)
	}
}

func TestIPRateLimit(t *testing.T) {
	s, c, _ := setupService(t, configuration.AuthConfiguration{RateLimits: configuration.RateLimitConfiguration{PerIPBurst: 3, PerIPRefill: 10 * time.Second}})
	for i := 0; i < 3; i++ {
		if err := s.AllowIP("192.0.2.1"); err != nil {
			t.Fatal(i, err)
		}
	}
	err := s.AllowIP("192.0.2.1")
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Seconds() != 10 {
		t.Fatalf("expected a limit, got %v", err)
	}
	if err = s.AllowIP("192.0.2.2"); err != nil {
		t.Error("other address limited")
	}
	c.advance(10 * time.Second)
	if err = s.AllowIP("192.0.2.1"); err != nil {
		t.Error("bucket not refilled")
	}

	off, _, _ := setupService(t, configuration.AuthConfiguration{RateLimits: configuration.RateLimitConfiguration{Disabled: true, PerIPBurst: 1}})
	for i := 0; i < 10; i++ {
		if err = off.AllowIP("192.0.2.1"); err != nil {
			t.Fatal("disabled limits apply", err)
		}
		_, _ = off.LoginPassword("admin", "wrong", sp(""))
	}
	if _, err = off.LoginPassword("admin", "boss secret 2026!", sp("")); err != nil {
		t.Error("disabled account limit applies", err)
	}
}

func TestInvitesAndResets(t *testing.T) {
	s, c, admin := setupService(t, configuration.AuthConfiguration{})
	l, token, err := s.CreateInvite(admin, RoleEditor, "for Anna", 0, ri())
	if err != nil || !l.ExpiresAt.Equal(c.now().Add(DefaultInviteLifetime)) || l.CreatedBy.Username != "admin" {
		t.Fatal(l, err)
	}
	if _, _, err = s.CreateInvite(admin, "root", "", 0, ri()); err == nil {
		t.Error("unknown role accepted")
	}
	if _, _, err = s.CreateInvite(admin, RoleReader, strings.Repeat("x", 201), 0, ri()); !errors.Is(err, ErrInvalidNote) {
		t.Error("long note accepted")
	}
	long, _, _ := s.CreateInvite(admin, RoleReader, "", 365*24*time.Hour, ri())
	if !long.ExpiresAt.Equal(c.now().Add(MaxInviteLifetime)) {
		t.Errorf("expiry not capped: %v", long.ExpiresAt)
	}
	insp, _, err := s.InspectLink(token)
	if err != nil || insp.Note != "for Anna" || insp.Role != RoleEditor {
		t.Fatal(insp, err)
	}
	if _, err = s.RedeemLink(token, NewAccount{Username: "Admin", Password: "flowers in spring"}, sp("")); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("taken username: %v", err)
	}
	r, err := s.RedeemLink(token, NewAccount{Username: "anna", Password: "flowers in spring"}, sp(SessionBearer))
	if err != nil || r.User.Role != RoleEditor || r.User.DisplayName != "anna" {
		t.Fatal(r, err)
	}
	links, _ := s.Links(LinkInvite, "")
	if len(links) != 2 || links[1].Status != LinkUsed || links[1].UsedBy.Username != "anna" {
		t.Errorf("links = %+v", links)
	}
	if pending, _ := s.Links(LinkInvite, LinkPending); len(pending) != 1 {
		t.Errorf("pending = %d", len(pending))
	}

	// revoke and expiry
	_, t2, _ := s.CreateInvite(admin, RoleReader, "", time.Hour, ri())
	l3, t3, _ := s.CreateInvite(admin, RoleReader, "", time.Hour, ri())
	if err = s.RevokeLink(LinkInvite, l3.ID, admin, ri()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.InspectLink(t3); !errors.Is(err, ErrInvalidLink) {
		t.Error("revoked invite works")
	}
	if err = s.RevokeLink(LinkReset, l3.ID, admin, ri()); !errors.Is(err, ErrNotFound) {
		t.Error("revoke with the wrong kind")
	}
	c.advance(time.Hour)
	if _, _, err = s.InspectLink(t2); !errors.Is(err, ErrInvalidLink) {
		t.Error("expired invite works")
	}

	// reset: revokes all sessions, sets the password
	other, _ := s.LoginPassword("anna", "flowers in spring", sp(""))
	var events []RevokeEvent
	s.OnRevoke(func(e RevokeEvent) { events = append(events, e) })
	rl, rt, err := s.CreateResetLink(admin, r.User.ID, 0, false, ri())
	if err != nil || !rl.ExpiresAt.Equal(c.now().Add(DefaultResetLifetime)) {
		t.Fatal(rl, err)
	}
	_, rt2, _ := s.CreateResetLink(admin, r.User.ID, 48*time.Hour, false, ri())
	if _, _, err = s.InspectLink(rt); !errors.Is(err, ErrInvalidLink) {
		t.Error("older reset link still works")
	}
	insp, target, err := s.InspectLink(rt2)
	if err != nil || insp.Kind != LinkReset || target.Username != "anna" || !insp.ExpiresAt.Equal(c.now().Add(MaxResetLifetime)) {
		t.Fatal(insp, target, err)
	}
	if _, err = s.RedeemLink(rt2, NewAccount{Password: "anna is great"}, sp("")); !errors.As(err, new(*WeakPasswordError)) {
		t.Errorf("password with the username accepted: %v", err)
	}
	nr, err := s.RedeemLink(rt2, NewAccount{Username: "ignored", Password: "a brand new secret"}, sp(""))
	if err != nil || nr.User.Username != "anna" {
		t.Fatal(nr, err)
	}
	for _, tok := range []string{r.Token, other.Token} {
		if _, _, err = s.AuthenticateSession(tok, ""); err == nil {
			t.Error("session survived the reset")
		}
	}
	if _, _, err = s.AuthenticateSession(nr.Token, ""); err != nil {
		t.Error("new session does not work")
	}
	if _, err = s.LoginPassword("anna", "flowers in spring", sp("")); err == nil {
		t.Error("old password works")
	}
	if _, err = s.LoginPassword("anna", "a brand new secret", sp("")); err != nil {
		t.Error("new password does not work", err)
	}
	if len(events) != 1 || events[0].ExceptSessionID != nr.Session.ID {
		t.Errorf("events = %+v", events)
	}
	if _, _, err = s.CreateResetLink(admin, "nope", 0, false, ri()); !errors.Is(err, ErrNotFound) {
		t.Error("reset link for an unknown account")
	}
}

func FuzzTokenFormat(f *testing.F) {
	f.Add("mds_" + strings.Repeat("A", 43))
	f.Add("mdp_abc")
	f.Add("")
	f.Fuzz(func(t *testing.T, token string) {
		if ValidSessionToken(token) && (len(token) != 47 || !strings.HasPrefix(token, PrefixSession)) {
			t.Errorf("accepted %q", token)
		}
	})
}
