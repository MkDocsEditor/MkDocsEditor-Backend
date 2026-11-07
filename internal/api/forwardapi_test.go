package api

import (
	"net/http"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

func TestForwardAuth(t *testing.T) {
	e := newAccountEnv(t, func(c *configuration.Configuration) {
		c.Server.TrustedProxies = []string{"127.0.0.1"}
	})
	admin := e.setupAdmin()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	type answer struct {
		StatusCode int
		Header     http.Header
	}
	forward := func(headers map[string]string, cookie *http.Cookie) answer {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+APIPrefix+EndpointPathForward, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return answer{resp.StatusCode, resp.Header}
	}
	html := "text/html,application/xhtml+xml,*/*;q=0.8"

	// a session: 200 with the user
	resp := forward(map[string]string{"Accept": html, headerForwardedURI: "/view/"}, admin.cookie)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(headerAuthUser) != "markus" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("session: %d %v", resp.StatusCode, resp.Header)
	}
	// an app token works too
	r := admin.do(http.MethodPost, "/account/tokens/", map[string]any{"name": "site", "scope": "read"})
	expect(t, r, http.StatusOK, "")
	var created struct {
		Token string `json:"token"`
	}
	r.json(t, &created)
	token := created.Token
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+APIPrefix+EndpointPathForward, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	tokenResp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		t.Errorf("app token: %d", tokenResp.StatusCode)
	}

	for _, tc := range []struct {
		name     string
		headers  map[string]string
		status   int
		location string
	}{
		{"page", map[string]string{"Accept": html, headerForwardedURI: "/view/haus/heizung/?x=1"}, http.StatusFound, "/login?next=%2Fview%2Fhaus%2Fheizung%2F%3Fx%3D1"},
		{"no uri", map[string]string{"Accept": html}, http.StatusFound, "/login"},
		{"other host", map[string]string{"Accept": html, headerForwardedURI: "//evil.example.com/x"}, http.StatusFound, "/login"},
		{"scheme", map[string]string{"Accept": html, headerForwardedURI: "https://evil.example.com/"}, http.StatusFound, "/login"},
		{"backslash", map[string]string{"Accept": html, headerForwardedURI: "/\\evil.example.com"}, http.StatusFound, "/login"},
		{"not html", map[string]string{"Accept": "text/css,*/*", headerForwardedURI: "/view/style.css"}, http.StatusUnauthorized, ""},
		{"post", map[string]string{"Accept": html, headerForwardedMethod: http.MethodPost, headerForwardedURI: "/view/"}, http.StatusUnauthorized, ""},
	} {
		resp := forward(tc.headers, nil)
		if resp.StatusCode != tc.status || resp.Header.Get("Location") != tc.location {
			t.Errorf("%s: %d %q", tc.name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// an expired session: the cookie is cleared, and the login follows
	stale := *admin.cookie
	expect(t, admin.do(http.MethodPost, "/auth/logout/", nil), http.StatusNoContent, "")
	if resp := forward(map[string]string{"Accept": html, headerForwardedURI: "/view/"}, &stale); resp.StatusCode != http.StatusFound {
		t.Errorf("logged out: %d", resp.StatusCode)
	}
	// many checks do not hit the login rate limit
	for range 50 {
		if resp := forward(map[string]string{"Accept": html}, nil); resp.StatusCode != http.StatusFound {
			t.Fatalf("check: %d", resp.StatusCode)
		}
	}
}

// X-Forwarded-* of a client that is no trusted proxy are ignored
func TestForwardAuthUntrustedHeaders(t *testing.T) {
	e := newAccountEnv(t)
	e.setupAdmin()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+APIPrefix+EndpointPathForward, nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set(headerForwardedURI, "/view/")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Errorf("%d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
