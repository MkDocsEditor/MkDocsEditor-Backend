package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

// grantFor runs the OAuth flow inside the service and returns the tokens
func grantFor(t *testing.T, s *Service, u *User, name string, end *time.Time) *OAuthTokens {
	t.Helper()
	client, err := s.RegisterOAuthClient(OAuthClientMetadata{ClientName: "Agent", RedirectURIs: []string{"http://127.0.0.1/cb"}}, RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	r, err := s.StartOAuthRequest(client, "http://127.0.0.1/cb", "", "", base64.RawURLEncoding.EncodeToString(sum[:]), "https://wiki/api/mcp")
	if err != nil {
		t.Fatal(err)
	}
	location, err := s.ApproveOAuthRequest(u, r.ID, "", name, end, RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(location)
	tokens, err := s.ExchangeOAuthCode(client.ID, loc.Query().Get("code"), "http://127.0.0.1/cb", verifier, "", RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func grantOf(t *testing.T, s *Service, token string) *AppToken {
	t.Helper()
	_, g, err := s.AuthenticateAppToken(token)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A grant with a fixed end: tokens never outlive it, refreshes do not move it
func TestOAuthFixedEnd(t *testing.T) {
	s, c, u := setupService(t, configuration.AuthConfiguration{})
	end := c.now().Add(90 * time.Minute)
	tokens := grantFor(t, s, u, "Laptop agent", &end)
	g := grantOf(t, s, tokens.AccessToken)
	if g.Name != "Laptop agent" || g.SlidingExpiry || g.ExpiresAt == nil || !g.ExpiresAt.Equal(end) {
		t.Fatalf("grant = %+v", g)
	}
	c.advance(time.Hour)
	tokens, err := s.RefreshOAuthToken(g.OAuthClientID, tokens.RefreshToken, "", RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.ExpiresIn != 30*60 {
		t.Errorf("access token lives %ds, want the 30 min to the end", tokens.ExpiresIn)
	}
	if g := grantOf(t, s, tokens.AccessToken); !g.ExpiresAt.Equal(end) {
		t.Errorf("refresh moved the end to %v", g.ExpiresAt)
	}
	c.advance(30 * time.Minute)
	if _, _, err := s.AuthenticateAppToken(tokens.AccessToken); err == nil {
		t.Error("access token works after the end")
	}
	if _, err := s.RefreshOAuthToken(g.OAuthClientID, tokens.RefreshToken, "", RequestInfo{}); err == nil {
		t.Error("refresh works after the end")
	}

	// past ends and bad names are refused
	past := c.now().Add(-time.Minute)
	client, _ := s.RegisterOAuthClient(OAuthClientMetadata{RedirectURIs: []string{"https://x.example/cb"}}, RequestInfo{})
	r, _ := s.StartOAuthRequest(client, "https://x.example/cb", "", "", strings.Repeat("a", 43), "r")
	if _, err := s.ApproveOAuthRequest(u, r.ID, "", "", &past, RequestInfo{}); err != ErrInvalidTokenExpiresAt {
		t.Errorf("past end: %v", err)
	}
	if _, err := s.ApproveOAuthRequest(u, r.ID, "", strings.Repeat("n", 65), nil, RequestInfo{}); err != ErrInvalidTokenName {
		t.Errorf("long name: %v", err)
	}
	// a refused approval leaves the request pending
	if _, err := s.PendingOAuthRequest(r.ID); err != nil {
		t.Errorf("request gone after a refused approval: %v", err)
	}
}

// Without a fixed end the grant slides: each refresh moves the end 30 days ahead
func TestOAuthSlidingEnd(t *testing.T) {
	s, c, u := setupService(t, configuration.AuthConfiguration{})
	tokens := grantFor(t, s, u, "", nil)
	g := grantOf(t, s, tokens.AccessToken)
	if g.Name != "Agent" || !g.SlidingExpiry || !g.ExpiresAt.Equal(c.now().Add(OAuthRefreshTokenLifetime)) {
		t.Fatalf("grant = %+v", g)
	}
	c.advance(20 * 24 * time.Hour)
	tokens, err := s.RefreshOAuthToken(g.OAuthClientID, tokens.RefreshToken, "", RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if g := grantOf(t, s, tokens.AccessToken); !g.ExpiresAt.Equal(c.now().Add(OAuthRefreshTokenLifetime)) {
		t.Errorf("end after refresh = %v", g.ExpiresAt)
	}
	// renaming keeps the end
	renamed, err := s.RenameAppToken(u, g.ID, " Work laptop ", RequestInfo{})
	if err != nil || renamed.Name != "Work laptop" || !renamed.SlidingExpiry {
		t.Fatalf("rename = %+v %v", renamed, err)
	}
	if n := countAudit(t, s, AuditAppTokenUpdated); n != 1 {
		t.Errorf("%d app-token.updated events", n)
	}
}
