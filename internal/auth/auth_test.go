package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"golang.org/x/crypto/bcrypt"
)

func hash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func TestNewAuthenticatorValidation(t *testing.T) {
	good := hash(t, "pw")
	cases := map[string]configuration.ServerConfiguration{
		"empty username": {Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{PasswordHash: good}}}},
		"plain password": {Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{Username: "a", PasswordHash: "pw"}}}},
		"unknown role":   {Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{Username: "a", PasswordHash: good, Role: "root"}}}},
		"bad color":      {Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{Username: "a", PasswordHash: good, Color: "red"}}}},
		"duplicate": {Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{
			{Username: "a", PasswordHash: good}, {Username: "a", PasswordHash: good}}}},
		"legacy duplicate": {
			Auth:      configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{Username: "a", PasswordHash: good}}},
			BasicAuth: configuration.AuthenticationConfiguration{User: "a", Password: "x"},
		},
	}
	for name, c := range cases {
		if _, err := NewAuthenticator(c); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	a, err := NewAuthenticator(configuration.ServerConfiguration{
		Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{
			{Username: "markus", PasswordHash: hash(t, "secret"), Role: "admin", DisplayName: "Markus", Email: "m@example.com"},
			{Username: "kid", PasswordHash: hash(t, "kidpw"), Role: "reader", Color: "#123456"},
		}},
		BasicAuth: configuration.AuthenticationConfiguration{User: "legacy", Password: "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Enabled() || len(a.Users()) != 3 {
		t.Fatal("users missing")
	}

	u := a.Authenticate("markus", "secret")
	if u == nil || u.DisplayName != "Markus" || u.Email != "m@example.com" || !u.CanWrite() {
		t.Fatalf("markus = %+v", u)
	}
	// second call is served from the cache
	if a.Authenticate("markus", "secret") != u {
		t.Error("cached authentication failed")
	}
	if a.Authenticate("markus", "wrong") != nil {
		t.Error("wrong password accepted after a cached success")
	}
	if a.Authenticate("nobody", "secret") != nil {
		t.Error("unknown user accepted")
	}
	kid := a.Authenticate("kid", "kidpw")
	if kid == nil || kid.CanWrite() || kid.Color != "#123456" || kid.DisplayName != "kid" {
		t.Errorf("kid = %+v", kid)
	}
	legacy := a.Authenticate("legacy", "old")
	if legacy == nil || legacy.Role != RoleAdmin {
		t.Errorf("legacy = %+v", legacy)
	}
	if a.Authenticate("legacy", "olD") != nil {
		t.Error("wrong legacy password accepted")
	}
}

func TestHtpasswdHashPrefix(t *testing.T) {
	h := hash(t, "pw")
	y := "$2y$" + h[4:]
	a, err := NewAuthenticator(configuration.ServerConfiguration{
		Auth: configuration.AuthConfiguration{Users: []configuration.UserConfiguration{{Username: "u", PasswordHash: y}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Authenticate("u", "pw") == nil {
		t.Error("$2y$ hash not accepted")
	}
}

func TestNoUsers(t *testing.T) {
	a, err := NewAuthenticator(configuration.ServerConfiguration{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Enabled() {
		t.Error("authentication enabled without users")
	}
	if Anonymous().Role != RoleAdmin {
		t.Error("anonymous user must be able to write when authentication is off")
	}
}

func TestTickets(t *testing.T) {
	a, _ := NewAuthenticator(configuration.ServerConfiguration{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	user := &User{Username: "markus"}

	ticket, expires, err := a.IssueTicket(user)
	if err != nil || !expires.Equal(now.Add(TicketLifetime)) {
		t.Fatal(err, expires)
	}
	if u, err := a.RedeemTicket(ticket); err != nil || u != user {
		t.Fatal("redeem failed", err)
	}
	if _, err := a.RedeemTicket(ticket); err == nil {
		t.Error("ticket redeemed twice")
	}

	ticket, _, _ = a.IssueTicket(user)
	now = now.Add(TicketLifetime)
	if _, err := a.RedeemTicket(ticket); err == nil {
		t.Error("expired ticket accepted")
	}
	if _, err := a.RedeemTicket("made-up"); err == nil {
		t.Error("unknown ticket accepted")
	}
}

func TestColorForUsername(t *testing.T) {
	c := ColorForUsername("markus")
	if c != ColorForUsername("markus") || !strings.HasPrefix(c, "#") {
		t.Error("colour is not stable")
	}
}

func TestHashPassword(t *testing.T) {
	h, err := HashPassword("pw")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(h), []byte("pw")) != nil {
		t.Fatal("hash does not verify", err)
	}
}
