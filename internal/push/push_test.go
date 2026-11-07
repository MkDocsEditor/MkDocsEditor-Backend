package push

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/push/pushtest"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The example of RFC 8291 appendix A
func TestEncryptMatchesRFC8291(t *testing.T) {
	asPrivate, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPublic := b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	got, err := encryptWith([]byte("When I grow up, I want to be a watermelon"), uaPublic,
		b64(t, "BTBZMqHH6r4Tts7J_aSIgg"), asPrivate, b64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if s := base64.RawURLEncoding.EncodeToString(got); s != want {
		t.Errorf("got  %s\nwant %s", s, want)
	}
}

// verifyVAPID checks an Authorization header and returns the claims
func verifyVAPID(t *testing.T, header string, pub string) map[string]any {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	token, k, ok2 := strings.Cut(rest, ", k=")
	if !ok || !ok2 || k != pub {
		t.Fatalf("authorization = %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, k))
	if err != nil {
		t.Fatal(err)
	}
	sig := b64(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("bad signature")
	}
	if h := string(b64(t, parts[0])); h != `{"typ":"JWT","alg":"ES256"}` {
		t.Errorf("header = %s", h)
	}
	var claims map[string]any
	if err = json.Unmarshal(b64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestEncryptRoundTripAndVAPID(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{7}, 16)
	body, err := encrypt([]byte(`{"type":"feed-changed"}`), ua.PublicKey().Bytes(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint32(body[16:20]) != recordSize {
		t.Errorf("record size %x", body[16:20])
	}
	if got, err := pushtest.Decrypt(body, ua, secret); err != nil || string(got) != `{"type":"feed-changed"}` {
		t.Errorf("decrypted %q, %v", got, err)
	}

	s, err := NewSender(Options{StateDir: t.TempDir(), Subject: "mailto:admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	header, err := vapidAuthorization(s.key, "https://push.example.com", s.subject, now)
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyVAPID(t, header, s.PublicKey())
	if claims["aud"] != "https://push.example.com" || claims["sub"] != "mailto:admin@example.com" ||
		claims["exp"] != float64(now.Add(vapidLifetime).Unix()) {
		t.Errorf("claims = %v", claims)
	}
}

func TestKeyIsKept(t *testing.T) {
	dir := t.TempDir()
	a, err := NewSender(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSender(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if a.PublicKey() != b.PublicKey() || len(b64(t, a.PublicKey())) != 65 {
		t.Errorf("keys %s %s", a.PublicKey(), b.PublicKey())
	}
}

func TestCheckEndpointAndKeys(t *testing.T) {
	s, err := NewSender(Options{StateDir: t.TempDir(), AllowedHosts: []string{"ntfy.example.com", "127.0.0.1:8443"}})
	if err != nil {
		t.Fatal(err)
	}
	for endpoint, ok := range map[string]bool{
		"https://ntfy.example.com/upABC":       true,
		"https://NTFY.example.com:443/upABC":   true,
		"https://127.0.0.1:8443/up":            true,
		"https://127.0.0.1/up":                 false,
		"http://ntfy.example.com/upABC":        false,
		"https://ntfy.example.com:8080/upABC":  false,
		"https://user@ntfy.example.com/upABC":  false,
		"https://evil.example.com/upABC":       false,
		"https://ntfy.example.com.evil.com/up": false,
		"ntfy.example.com/up":                  false,
	} {
		if err := s.CheckEndpoint(endpoint); (err == nil) != ok {
			t.Errorf("%s: %v", endpoint, err)
		}
	}

	ua, _ := ecdh.P256().GenerateKey(nil)
	pub := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	if _, _, err := DecodeKeys(pub, "BTBZMqHH6r4Tts7J_aSIgg=="); err != nil {
		t.Errorf("padded auth: %v", err)
	}
	for _, keys := range [][2]string{{pub, "short"}, {"AAAA", "BTBZMqHH6r4Tts7J_aSIgg"}, {pub, "BTBZMqHH6r4Tts7J_aSIg+"}} {
		if _, _, err := DecodeKeys(keys[0], keys[1]); err == nil {
			t.Errorf("%v accepted", keys)
		}
	}
}
