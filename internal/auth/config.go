package auth

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

// defaults of docs/auth.md section 13
const (
	DefaultSessionIdleTimeout = 30 * 24 * time.Hour
	DefaultSessionMaxLifetime = 90 * 24 * time.Hour
	DefaultAuditRetention     = 365 * 24 * time.Hour
	// ReauthWindow is how long a login or step-up counts as recent
	ReauthWindow = 10 * time.Minute
	// how often lastUsedAt of a session is written at most
	sessionTouchInterval = 5 * time.Minute

	setupLinkLifetime     = 24 * time.Hour
	DefaultInviteLifetime = 7 * 24 * time.Hour
	MaxInviteLifetime     = 30 * 24 * time.Hour
	DefaultResetLifetime  = time.Hour
	MaxResetLifetime      = 24 * time.Hour
)

// Cookie secure modes
const (
	CookieSecureAuto   = "auto"
	CookieSecureAlways = "always"
	CookieSecureNever  = "never"
)

// settings are the normalized authentication settings
type settings struct {
	disabled        bool
	clientURL       string
	legacyBasicAuth bool
	idleTimeout     time.Duration
	maxLifetime     time.Duration
	cookieSecure    string
	auditRetention  time.Duration
	rateLimits      configuration.RateLimitConfiguration
}

func newSettings(conf configuration.AuthConfiguration) (settings, error) {
	s := settings{
		disabled:        conf.Disabled,
		clientURL:       strings.TrimRight(conf.ClientURL, "/"),
		legacyBasicAuth: true,
		idleTimeout:     conf.SessionIdleTimeout,
		maxLifetime:     conf.SessionMaxLifetime,
		cookieSecure:    conf.CookieSecure,
		auditRetention:  conf.AuditRetention,
		rateLimits:      conf.RateLimits,
	}
	if conf.LegacyBasicAuth != nil {
		s.legacyBasicAuth = *conf.LegacyBasicAuth
	}
	if s.clientURL != "" {
		u, err := url.Parse(s.clientURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return s, fmt.Errorf("server.auth.clientUrl %q is not an http(s) URL", conf.ClientURL)
		}
	}
	if s.idleTimeout <= 0 {
		s.idleTimeout = DefaultSessionIdleTimeout
	}
	if s.maxLifetime <= 0 {
		s.maxLifetime = DefaultSessionMaxLifetime
	}
	if s.idleTimeout > s.maxLifetime {
		s.idleTimeout = s.maxLifetime
	}
	switch s.cookieSecure {
	case "":
		s.cookieSecure = CookieSecureAuto
	case CookieSecureAuto, CookieSecureAlways, CookieSecureNever:
	default:
		return s, fmt.Errorf("server.auth.cookieSecure must be auto, always or never, not %q", conf.CookieSecure)
	}
	if s.auditRetention <= 0 {
		s.auditRetention = DefaultAuditRetention
	}
	if s.rateLimits.PerIPBurst <= 0 {
		s.rateLimits.PerIPBurst = 20
	}
	if s.rateLimits.PerIPRefill <= 0 {
		s.rateLimits.PerIPRefill = 6 * time.Second
	}
	if s.rateLimits.AccountFailuresBeforeBackoff <= 0 {
		s.rateLimits.AccountFailuresBeforeBackoff = 5
	}
	if s.rateLimits.MaxBackoff <= 0 {
		s.rateLimits.MaxBackoff = 15 * time.Minute
	}
	return s, nil
}

// Disabled reports whether authentication is turned off (server.auth.disabled)
func (s *Service) Disabled() bool {
	return s.cfg.disabled
}

// LegacyBasicAuth reports whether HTTP Basic auth is accepted
func (s *Service) LegacyBasicAuth() bool {
	return s.cfg.legacyBasicAuth
}

// CookieSecure returns the cookie secure mode
func (s *Service) CookieSecure() string {
	return s.cfg.cookieSecure
}

// ClientURL returns the configured web client URL without a trailing slash, or ""
func (s *Service) ClientURL() string {
	return s.cfg.clientURL
}

// LinkURL returns the URL of a link token, or nil without a configured client URL
func (s *Service) LinkURL(kind, token string) *string {
	if s.cfg.clientURL == "" {
		return nil
	}
	u := s.cfg.clientURL + "/" + kind + "#" + token
	return &u
}
