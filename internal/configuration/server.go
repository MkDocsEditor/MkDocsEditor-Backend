package configuration

import "time"

type (
	ServerConfiguration struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		// TrustedProxies are the addresses (CIDRs or IPs) of reverse proxies whose
		// X-Forwarded-For and X-Forwarded-Proto headers are trusted
		TrustedProxies []string `yaml:"trustedProxies"`
		// Auth configures accounts and sessions, see docs/auth.md
		Auth AuthConfiguration `yaml:"auth"`
		// BasicAuth is the old single-user configuration. It keeps working; the user is an admin.
		BasicAuth AuthenticationConfiguration `yaml:"basicAuth"`
		CORS      CorsConfiguration           `yaml:"cors"`
	}

	AuthConfiguration struct {
		// Disabled turns authentication off: every request is the anonymous admin (development only)
		Disabled bool `yaml:"disabled"`
		// ClientURL is the web client, used for setup, invite and reset links
		ClientURL string `yaml:"clientUrl"`
		// LegacyBasicAuth accepts HTTP Basic auth (transition from server.auth.users).
		// Unset means true until the clients use sessions.
		LegacyBasicAuth *bool `yaml:"legacyBasicAuth"`
		// SessionIdleTimeout ends sessions that were not used for this long (default 720h)
		SessionIdleTimeout time.Duration `yaml:"sessionIdleTimeout"`
		// SessionMaxLifetime ends sessions this long after the login (default 2160h)
		SessionMaxLifetime time.Duration `yaml:"sessionMaxLifetime"`
		// CookieSecure is "auto" (default), "always" or "never"
		CookieSecure string `yaml:"cookieSecure"`
		// SecretKeyFile holds the server key (default <stateDir>/auth.key)
		SecretKeyFile string `yaml:"secretKeyFile"`
		// AuditRetention is how long audit events are kept (default 8760h)
		AuditRetention time.Duration `yaml:"auditRetention"`
		// RateLimits of the login endpoints
		RateLimits RateLimitConfiguration `yaml:"rateLimits"`
		// WebAuthn configures passkeys
		WebAuthn WebAuthnConfiguration `yaml:"webauthn"`
		// Users are imported once into the account database, then ignored
		Users []UserConfiguration `yaml:"users"`
	}

	RateLimitConfiguration struct {
		// Disabled turns all rate limits off (automated tests only)
		Disabled bool `yaml:"disabled"`
		// PerIPBurst is the burst of the per-IP token bucket (default 20)
		PerIPBurst int `yaml:"perIpBurst"`
		// PerIPRefill adds one token to the per-IP bucket every this long (default 6s)
		PerIPRefill time.Duration `yaml:"perIpRefill"`
		// AccountFailuresBeforeBackoff is the number of failures per account before the
		// exponential backoff starts (default 5)
		AccountFailuresBeforeBackoff int `yaml:"accountFailuresBeforeBackoff"`
		// MaxBackoff caps the backoff (default 15m)
		MaxBackoff time.Duration `yaml:"maxBackoff"`
	}

	WebAuthnConfiguration struct {
		// RPID is the relying party id (default: host of clientUrl)
		RPID string `yaml:"rpId"`
		// RPName is shown by authenticators (default MkDocsEditor)
		RPName string `yaml:"rpName"`
		// Origins are the allowed web origins (default: origin of clientUrl and the CORS origins)
		Origins []string `yaml:"origins"`
		// Android apps that may use the passkeys
		Android []AndroidAppConfiguration `yaml:"android"`
		// ServeAssetLinks serves /.well-known/assetlinks.json from the backend
		ServeAssetLinks bool `yaml:"serveAssetLinks"`
	}

	AndroidAppConfiguration struct {
		PackageName            string   `yaml:"packageName"`
		SHA256CertFingerprints []string `yaml:"sha256CertFingerprints"`
	}

	UserConfiguration struct {
		Username string `yaml:"username"`
		// PasswordHash is a bcrypt hash, see "mkdocsrest hash-password"
		PasswordHash string `yaml:"passwordHash"`
		// DisplayName is shown to other users and used as the git author name (optional)
		DisplayName string `yaml:"displayName"`
		// Email is used as the git author email (optional)
		Email string `yaml:"email"`
		// Role is one of admin, editor, reader (default: editor)
		Role string `yaml:"role"`
		// Color overrides the colour derived from the username, e.g. "#1971c2" (optional)
		Color string `yaml:"color"`
	}

	AuthenticationConfiguration struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	}

	CorsConfiguration struct {
		AllowedOrigins []string `yaml:"allowedOrigins"`
		AllowedMethods []string `yaml:"allowedMethods"`
	}
)
