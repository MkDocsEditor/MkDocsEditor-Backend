package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	// cheap hashes keep the tests fast; TestArgon2Parameters checks the real ones
	currentArgon2 = argon2Params{memory: 64, time: 1, threads: 1}
	os.Exit(m.Run())
}

func hash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func openTest(t *testing.T, dir string, conf configuration.ServerConfiguration) *Service {
	t.Helper()
	s, err := Open(Options{StateDir: dir, Server: conf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func users(u ...configuration.UserConfiguration) configuration.ServerConfiguration {
	return configuration.ServerConfiguration{Auth: configuration.AuthConfiguration{Users: u}}
}

func TestImportValidation(t *testing.T) {
	good := hash(t, "pw")
	cases := map[string]configuration.ServerConfiguration{
		"empty username": users(configuration.UserConfiguration{PasswordHash: good}),
		"colon":          users(configuration.UserConfiguration{Username: "a:b", PasswordHash: good}),
		"plain password": users(configuration.UserConfiguration{Username: "a", PasswordHash: "pw"}),
		"unknown role":   users(configuration.UserConfiguration{Username: "a", PasswordHash: good, Role: "root"}),
		"bad color":      users(configuration.UserConfiguration{Username: "a", PasswordHash: good, Color: "red"}),
		"duplicate":      users(configuration.UserConfiguration{Username: "a", PasswordHash: good}, configuration.UserConfiguration{Username: "a", PasswordHash: good}),
		"duplicate case": users(configuration.UserConfiguration{Username: "a", PasswordHash: good}, configuration.UserConfiguration{Username: "A", PasswordHash: good}),
		"legacy duplicate": {
			Auth:      configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{Username: "a", PasswordHash: good}}},
			BasicAuth: configuration.AuthenticationConfiguration{User: "a", Password: "x"},
		},
	}
	for name, c := range cases {
		if s, err := Open(Options{StateDir: t.TempDir(), Server: c}); err == nil {
			_ = s.Close()
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestImportAndBasicAuth(t *testing.T) {
	dir := t.TempDir()
	conf := configuration.ServerConfiguration{
		Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{
			{Username: "markus", PasswordHash: hash(t, "secret"), Role: "admin", DisplayName: "Markus", Email: "m@example.com"},
			{Username: "kid", PasswordHash: "$2y$" + hash(t, "kidpw")[4:], Role: "reader", Color: "#123456"},
		}},
		BasicAuth: configuration.AuthenticationConfiguration{User: "legacy", Password: "old"},
	}
	a := openTest(t, dir, conf)
	if !a.Enabled() || len(a.Users()) != 3 {
		t.Fatalf("users = %v", a.Users())
	}

	u := a.AuthenticateBasic("markus", "secret")
	if u == nil || u.DisplayName != "Markus" || u.Email != "m@example.com" || !u.CanWrite() || len(u.ID) != 26 {
		t.Fatalf("markus = %+v", u)
	}
	// the bcrypt hash was replaced by Argon2id
	var h string
	if err := a.db.QueryRow("SELECT password_hash FROM accounts WHERE id = ?", u.ID).Scan(&h); err != nil || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash after login = %q, %v", h, err)
	}
	if a.AuthenticateBasic("markus", "secret") == nil {
		t.Error("cached authentication failed")
	}
	if a.AuthenticateBasic("markus", "wrong") != nil {
		t.Error("wrong password accepted after a cached success")
	}
	if a.AuthenticateBasic("nobody", "secret") != nil {
		t.Error("unknown user accepted")
	}
	kid := a.AuthenticateBasic("kid", "kidpw")
	if kid == nil || kid.CanWrite() || kid.Color != "#123456" || kid.DisplayName != "kid" {
		t.Errorf("kid = %+v", kid)
	}
	legacy := a.AuthenticateBasic("legacy", "old")
	if legacy == nil || legacy.Role != RoleAdmin {
		t.Errorf("legacy = %+v", legacy)
	}
	if a.AuthenticateBasic("legacy", "olD") != nil {
		t.Error("wrong legacy password accepted")
	}
	var events int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE type = ?", AuditAccountImported).Scan(&events); err != nil || events != 3 {
		t.Errorf("import audit events = %d, %v", events, err)
	}
	_ = a.Close()

	// the import happens once: later configuration changes are ignored
	conf.Auth.Users = conf.Auth.Users[:1]
	conf.Auth.Users[0].PasswordHash = hash(t, "changed")
	b := openTest(t, dir, conf)
	if len(b.Users()) != 3 || b.AuthenticateBasic("markus", "changed") != nil || b.AuthenticateBasic("markus", "secret") == nil {
		t.Error("configuration was imported again")
	}
	if b.UserByID(u.ID).Username != "markus" {
		t.Error("account id changed")
	}
}

func TestNoUsers(t *testing.T) {
	a := openTest(t, t.TempDir(), configuration.ServerConfiguration{})
	if !a.Enabled() || !a.SetupRequired() {
		t.Error("without users the server must wait for the setup")
	}
	off := openTest(t, t.TempDir(), configuration.ServerConfiguration{Auth: configuration.AuthConfiguration{Disabled: true}})
	if off.Enabled() || off.SetupRequired() {
		t.Error("auth.disabled must turn authentication off")
	}
	if Anonymous().Role != RoleAdmin || a.UserByID(AnonymousID).Username != AnonymousUsername {
		t.Error("anonymous user must be able to write when authentication is off")
	}
}

func TestDatabaseFromNewerVersion(t *testing.T) {
	dir := t.TempDir()
	a := openTest(t, dir, configuration.ServerConfiguration{})
	if _, err := a.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	if _, err := Open(Options{StateDir: dir}); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("err = %v", err)
	}
}

func TestDatabaseFileMode(t *testing.T) {
	dir := t.TempDir()
	openTest(t, dir, configuration.ServerConfiguration{})
	info, err := os.Stat(filepath.Join(dir, DatabaseFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}
}

func TestImportActorsFile(t *testing.T) {
	dir := t.TempDir()
	actors := `{
  "aa01": {"username": "markus", "bound": "2026-01-01T00:00:00Z"},
  "aa02": {"username": ":disk", "bound": "2026-01-02T00:00:00Z"},
  "aa03": {"username": "gone", "bound": "2026-01-03T00:00:00Z"},
  "aa04": {"username": "gone", "bound": "2026-01-04T00:00:00Z"},
  "aa05": {"username": "anonymous", "bound": "2026-01-05T00:00:00Z"},
  "aa06": {"username": ":server", "bound": "2026-01-06T00:00:00Z"}
}`
	if err := os.WriteFile(filepath.Join(dir, ActorsFileName), []byte(actors), 0o600); err != nil {
		t.Fatal(err)
	}
	a := openTest(t, dir, users(configuration.UserConfiguration{Username: "markus", PasswordHash: hash(t, "pw"), Role: "admin"}))
	b, err := a.ActorBindings()
	if err != nil {
		t.Fatal(err)
	}
	markus := a.UserByUsername("markus")
	gone := a.UserByUsername("gone")
	if gone == nil || gone.Status != StatusDeleted || gone.ID == markus.ID {
		t.Fatalf("tombstone = %+v", gone)
	}
	want := map[string]string{"aa01": markus.ID, "aa02": ":disk", "aa03": gone.ID, "aa04": gone.ID, "aa05": AnonymousID, "aa06": ":server"}
	for id, subject := range want {
		if b[id].Subject != subject {
			t.Errorf("%s = %q, want %q", id, b[id].Subject, subject)
		}
	}
	if !b["aa01"].Bound.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("bound = %v", b["aa01"].Bound)
	}
	if _, err := os.Stat(filepath.Join(dir, ActorsFileName)); !os.IsNotExist(err) {
		t.Error("actors.json was not renamed")
	}
	if _, err := os.Stat(filepath.Join(dir, ActorsFileName+".imported")); err != nil {
		t.Error("actors.json.imported missing")
	}
	// tombstones do not enable authentication on their own and cannot log in
	if a.AuthenticateBasic("gone", "") != nil {
		t.Error("tombstone could log in")
	}

	// a new binding is stored
	if err := a.StoreActorBinding("bb01", ActorBinding{Subject: markus.ID, Bound: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := a.StoreActorBinding("bb01", ActorBinding{Subject: gone.ID, Bound: time.Now()}); err == nil {
		t.Error("binding overwritten")
	}
	_ = a.Close()

	// a file that appears again later is not imported a second time
	if err := os.WriteFile(filepath.Join(dir, ActorsFileName), []byte(`{"cc01": {"username": "markus"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a = openTest(t, dir, configuration.ServerConfiguration{})
	b, _ = a.ActorBindings()
	if _, ok := b["cc01"]; ok || b["bb01"].Subject != markus.ID {
		t.Errorf("bindings = %v", b)
	}
}

func TestTickets(t *testing.T) {
	a := openTest(t, t.TempDir(), users(configuration.UserConfiguration{Username: "markus", PasswordHash: hash(t, "pw")}))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	user := a.UserByUsername("markus")

	ticket, expires, err := a.IssueTicket(user, "")
	if err != nil || !expires.Equal(now.Add(TicketLifetime)) {
		t.Fatal(err, expires)
	}
	if u, _, err := a.RedeemTicket(ticket); err != nil || u.ID != user.ID {
		t.Fatal("redeem failed", err)
	}
	if _, _, err := a.RedeemTicket(ticket); err == nil {
		t.Error("ticket redeemed twice")
	}

	ticket, _, _ = a.IssueTicket(user, "")
	now = now.Add(TicketLifetime)
	if _, _, err := a.RedeemTicket(ticket); err == nil {
		t.Error("expired ticket accepted")
	}
	if _, _, err := a.RedeemTicket("made-up"); err == nil {
		t.Error("unknown ticket accepted")
	}
	if ticket, _, _ = a.IssueTicket(Anonymous(), ""); ticket == "" {
		t.Fatal("no ticket")
	}
	if u, _, err := a.RedeemTicket(ticket); err != nil || u.ID != AnonymousID {
		t.Error("anonymous ticket", u, err)
	}
}

func TestColorForUsername(t *testing.T) {
	c := ColorForUsername("markus")
	if c != ColorForUsername("markus") || !strings.HasPrefix(c, "#") {
		t.Error("colour is not stable")
	}
}

func TestHashPassword(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatal(h, err)
	}
	if ok, rehash := VerifyPassword(h, "correct horse"); !ok || rehash {
		t.Errorf("verify = %v, %v", ok, rehash)
	}
	if ok, _ := VerifyPassword(h, "correct horsE"); ok {
		t.Error("wrong password accepted")
	}
	// NFKC: the composed and decomposed forms of "é" are the same password
	h, _ = HashPassword("café au lait")
	if ok, _ := VerifyPassword(h, "café au lait"); !ok {
		t.Error("password is not normalized")
	}
	// old parameters ask for a rehash
	old := currentArgon2
	currentArgon2 = argon2Params{memory: 128, time: 1, threads: 1}
	h, _ = HashPassword("pw")
	currentArgon2 = old
	if ok, rehash := VerifyPassword(h, "pw"); !ok || !rehash {
		t.Errorf("old parameters: %v, %v", ok, rehash)
	}
	if ok, rehash := VerifyPassword(hash(t, "pw"), "pw"); !ok || !rehash {
		t.Errorf("bcrypt: %v, %v", ok, rehash)
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAA", "$argon2i$v=19$m=64,t=1,p=1$AAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=18$m=64,t=1,p=1$AAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA", "$argon2id$v=19$m=99999999,t=1,p=1$AAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA"} {
		if ok, _ := VerifyPassword(bad, "pw"); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestArgon2Parameters(t *testing.T) {
	// OWASP minimum for Argon2id: m=19 MiB, t=2, p=1
	if defaultArgon2.memory < 19*1024 || defaultArgon2.time < 2 || defaultArgon2.threads != 1 {
		t.Errorf("parameters = %+v", defaultArgon2)
	}
}

func FuzzParseArgon2(f *testing.F) {
	f.Add("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g")
	f.Add("$2b$10$abcdefghijklmnopqrstuv")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		p, salt, key, err := parseArgon2(s)
		if err == nil && (p.memory == 0 || p.memory > 1<<21 || len(key) < 16 || len(key) > 64 || salt == nil) {
			t.Errorf("accepted invalid parameters %+v for %q", p, s)
		}
	})
}
