package collab

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebsocketOriginCheck(t *testing.T) {
	check := checkOrigin([]string{"https://editor.example.com"})
	for origin, want := range map[string]bool{
		"":                           true,
		"https://editor.example.com": true,
		"https://evil.example.com":   false,
	} {
		req := httptest.NewRequest(http.MethodGet, "/document/x/ws/", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if got := check(req); got != want {
			t.Errorf("origin %q: %v, want %v", origin, got, want)
		}
	}
	if !checkOrigin(nil)(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("no configured origins must allow everything")
	}
}
