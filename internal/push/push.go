// Package push sends Web Push messages (RFC 8030) to the clients of accounts, encrypted with
// RFC 8291 and signed with VAPID (RFC 8292). UnifiedPush distributors such as ntfy speak the
// same protocol. See docs/push.md.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// KeyFileName is the name of the VAPID key in the state directory
	KeyFileName = "push-vapid.key"
	// TTL is how long a push service keeps an undelivered message
	TTL = 4 * time.Hour
	// maxEndpointLength limits the endpoints clients may register
	maxEndpointLength = 2048
	sendTimeout       = 30 * time.Second
)

// ErrEndpointNotAllowed is returned for an endpoint that is not https on an allowed host
var ErrEndpointNotAllowed = errors.New("the endpoint must be an https URL on an allowed host")

// Options configure the sender
type Options struct {
	// StateDir holds the VAPID key
	StateDir string
	// AllowedHosts are the hosts, with an optional port, the server sends to
	AllowedHosts []string
	// Subject is the contact of the VAPID tokens, a mailto: or https: URL
	Subject string
}

// Sender sends push messages
type Sender struct {
	key          *ecdsa.PrivateKey
	allowedHosts map[string]bool
	subject      string
	// Client sends the requests. It does not follow redirects.
	Client *http.Client
	now    func() time.Time
}

// NewSender loads the VAPID key, or creates it on the first start
func NewSender(opts Options) (*Sender, error) {
	key, err := loadOrCreateKey(filepath.Join(opts.StateDir, KeyFileName))
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	for _, h := range opts.AllowedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts[h] = true
		}
	}
	return &Sender{
		key:          key,
		allowedHosts: hosts,
		subject:      opts.Subject,
		Client: &http.Client{
			Timeout: sendTimeout,
			// a redirect could lead to a host that is not allowed
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now,
	}, nil
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "EC PRIVATE KEY" {
			return nil, fmt.Errorf("%s holds no EC private key", path)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%s: the VAPID key must be P-256", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err = pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return key, f.Close()
}

// PublicKey is the VAPID public key the clients subscribe with: the uncompressed P-256 point,
// base64url without padding
func (s *Sender) PublicKey() string {
	return publicKeyString(s.key)
}

// AllowedHosts lists the hosts the server sends to
func (s *Sender) AllowedHosts() []string {
	out := make([]string, 0, len(s.allowedHosts))
	for h := range s.allowedHosts {
		out = append(out, h)
	}
	return out
}

// CheckEndpoint returns ErrEndpointNotAllowed unless the endpoint is an https URL on an
// allowed host. A host without a port in the list allows the default port only.
func (s *Sender) CheckEndpoint(endpoint string) error {
	if len(endpoint) > maxEndpointLength {
		return ErrEndpointNotAllowed
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Fragment != "" {
		return ErrEndpointNotAllowed
	}
	host := strings.ToLower(u.Host)
	if port := u.Port(); port == "" || port == "443" {
		host = strings.ToLower(u.Hostname())
	}
	if !s.allowedHosts[host] {
		return ErrEndpointNotAllowed
	}
	return nil
}

// Result is the outcome of a push
type Result int

const (
	// Delivered means the push service accepted the message
	Delivered Result = iota
	// Gone means the subscription no longer exists (404 or 410)
	Gone
	// Failed is any other answer or no answer
	Failed
)

// Message is one push message
type Message struct {
	Payload []byte
	// Topic lets the push service replace an undelivered message with the same topic
	Topic string
}

// Send sends a message to a subscription
func (s *Sender) Send(ctx context.Context, endpoint string, p256dh []byte, authSecret []byte, msg Message) (Result, error) {
	if err := s.CheckEndpoint(endpoint); err != nil {
		return Failed, err
	}
	body, err := encrypt(msg.Payload, p256dh, authSecret)
	if err != nil {
		return Failed, err
	}
	u, _ := url.Parse(endpoint)
	authorization, err := vapidAuthorization(s.key, u.Scheme+"://"+u.Host, s.subject, s.now())
	if err != nil {
		return Failed, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Failed, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(TTL.Seconds())))
	req.Header.Set("Urgency", "normal")
	if msg.Topic != "" {
		req.Header.Set("Topic", msg.Topic)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return Failed, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Delivered, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return Gone, nil
	}
	return Failed, fmt.Errorf("the push service answered %s", resp.Status)
}
