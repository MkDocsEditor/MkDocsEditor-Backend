package auth

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// Passkey errors
var (
	ErrPasskeysDisabled          = errors.New("passkeys are not configured on this server")
	ErrPasskeyVerificationFailed = errors.New("the passkey could not be verified")
	ErrPasskeyRegistrationFailed = errors.New("the passkey could not be registered")
	ErrInvalidPasskeyName        = errors.New("passkey names have 1 to 64 characters")
)

const ceremonyLifetime = 5 * time.Minute

// knownAuthenticators maps the AAGUIDs of common passkey providers to a name, used as the
// default passkey name
var knownAuthenticators = map[string]string{
	"ea9b8d66-4d01-1d21-3ce4-b6b48cb575d4": "Google Password Manager",
	"fbfc3007-154e-4ecc-8c0b-6e020557d7bd": "iCloud Keychain",
	"d548826e-79b4-db40-a3d8-11116f7e8349": "Bitwarden",
	"bada5566-a7aa-401f-bd96-45619a55120d": "1Password",
	"531126d6-e717-415c-9320-3d9aa6981239": "Dashlane",
	"50726f74-6f6e-5061-7373-50726f746f6e": "Proton Pass",
	"fdb141b2-5d84-443e-8a35-4698c205a502": "KeePassXC",
	"08987058-cadc-4b81-b6e1-30de50dcbe96": "Windows Hello",
	"adce0002-35bc-c60a-648b-0b25f1f05503": "Chrome on Mac",
}

// passkeyConfig is the WebAuthn setup of the server
type passkeyConfig struct {
	wa         *webauthn.WebAuthn // nil when passkeys are off
	rpID       string
	assetLinks []byte // nil when the backend does not serve assetlinks.json
}

// AndroidOrigin returns the WebAuthn origin of an Android app from the SHA-256 fingerprint
// of its signing certificate (hex, colons optional)
func AndroidOrigin(fingerprint string) (string, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", ""))
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("%q is not a SHA-256 certificate fingerprint (32 hex bytes)", fingerprint)
	}
	return "android:apk-key-hash:" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func newPasskeyConfig(server configuration.ServerConfiguration, cfg settings) (*passkeyConfig, error) {
	conf := server.Auth.WebAuthn
	pc := &passkeyConfig{}
	var androidOrigins []string
	type assetLink struct {
		Relation []string `json:"relation"`
		Target   struct {
			Namespace    string   `json:"namespace"`
			PackageName  string   `json:"package_name"`
			Fingerprints []string `json:"sha256_cert_fingerprints"`
		} `json:"target"`
	}
	var links []assetLink
	for _, app := range conf.Android {
		if strings.TrimSpace(app.PackageName) == "" || len(app.SHA256CertFingerprints) == 0 {
			return nil, errors.New("server.auth.webauthn.android entries need packageName and sha256CertFingerprints")
		}
		var l assetLink
		l.Relation = []string{"delegate_permission/common.handle_all_urls", "delegate_permission/common.get_login_creds"}
		l.Target.Namespace, l.Target.PackageName = "android_app", strings.TrimSpace(app.PackageName)
		for _, fp := range app.SHA256CertFingerprints {
			origin, err := AndroidOrigin(fp)
			if err != nil {
				return nil, fmt.Errorf("server.auth.webauthn.android: %w", err)
			}
			androidOrigins = append(androidOrigins, origin)
			l.Target.Fingerprints = append(l.Target.Fingerprints, strings.ToUpper(strings.TrimSpace(fp)))
		}
		links = append(links, l)
	}
	if conf.ServeAssetLinks {
		if len(links) == 0 {
			return nil, errors.New("server.auth.webauthn.serveAssetLinks needs server.auth.webauthn.android")
		}
		data, err := json.MarshalIndent(links, "", "  ")
		if err != nil {
			return nil, err
		}
		pc.assetLinks = data
	}

	pc.rpID = strings.TrimSpace(conf.RPID)
	if pc.rpID == "" && cfg.clientURL != "" {
		if u, err := url.Parse(cfg.clientURL); err == nil {
			pc.rpID = u.Hostname()
		}
	}
	if pc.rpID == "" {
		return pc, nil
	}
	var origins []string
	if len(conf.Origins) > 0 {
		for _, o := range conf.Origins {
			origins = append(origins, strings.TrimRight(strings.TrimSpace(o), "/"))
		}
	} else {
		if o := originOf(cfg.clientURL); o != "" {
			origins = append(origins, o)
		}
		for _, o := range server.CORS.AllowedOrigins {
			if o = strings.TrimRight(o, "/"); o != "*" && o != "" {
				origins = append(origins, o)
			}
		}
	}
	if len(origins) == 0 {
		return nil, errors.New("passkeys need allowed origins: set server.auth.webauthn.origins or server.auth.clientUrl")
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  pc.rpID,
		RPDisplayName:         cfg.issuer,
		RPOrigins:             origins,
		RPOpaqueOrigins:       androidOrigins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyLifetime, TimeoutUVD: ceremonyLifetime},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyLifetime, TimeoutUVD: ceremonyLifetime},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("passkey configuration (server.auth.webauthn): %w", err)
	}
	pc.wa = wa
	return pc, nil
}

// PasskeysEnabled reports whether passkeys are configured
func (s *Service) PasskeysEnabled() bool {
	return s.pk.wa != nil
}

// RPID returns the WebAuthn relying party id, "" when passkeys are off
func (s *Service) RPID() string {
	if s.pk.wa == nil {
		return ""
	}
	return s.pk.rpID
}

// AssetLinks returns the Digital Asset Links document, nil when it is not served
func (s *Service) AssetLinks() []byte {
	return s.pk.assetLinks
}

// waUser is an account as go-webauthn sees it
type waUser struct {
	accountID   string
	handle      []byte
	name        string
	displayName string
	creds       []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.handle }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// Passkey is the public view of a stored passkey
type Passkey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	BackedUp   bool       `json:"backedUp"`
	Transports []string   `json:"transports"`
	Provider   string     `json:"provider,omitempty"`
}

type storedPasskey struct {
	Passkey
	cred webauthn.Credential
}

// ceremony kinds
const (
	ceremonyLogin = iota + 1
	ceremonySecondFactor
	ceremonyLinkRegistration
	ceremonyAccountRegistration
	ceremonyReauth
)

type ceremony struct {
	kind      int
	data      webauthn.SessionData
	expires   time.Time
	accountID string
	sessionID string
	challenge string
	linkToken string
	account   NewAccount
	handle    []byte
}

type ceremonies struct {
	mu sync.Mutex
	m  map[string]*ceremony
}

func (s *Service) startCeremony(c *ceremony) string {
	id := newToken("")
	now := s.now()
	c.expires = now.Add(ceremonyLifetime)
	s.cer.mu.Lock()
	defer s.cer.mu.Unlock()
	for k, old := range s.cer.m {
		if !now.Before(old.expires) {
			delete(s.cer.m, k)
		}
	}
	s.cer.m[id] = c
	return id
}

// takeCeremony removes and returns a ceremony of a kind; ceremonies are single use
func (s *Service) takeCeremony(id string, kinds ...int) (*ceremony, error) {
	s.cer.mu.Lock()
	c := s.cer.m[id]
	delete(s.cer.m, id)
	s.cer.mu.Unlock()
	if c == nil || !s.now().Before(c.expires) {
		return nil, ErrChallengeExpired
	}
	if slices.Contains(kinds, c.kind) {
		return c, nil
	}
	return nil, ErrChallengeExpired
}

func newHandle() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// webauthnHandle returns the user handle of an account, creating it when asked
func webauthnHandle(db interface {
	querier
	execer
}, accountID string, create bool) ([]byte, error) {
	var h []byte
	err := db.QueryRow("SELECT handle FROM webauthn_handles WHERE account_id = ?", accountID).Scan(&h)
	if err == nil || !errors.Is(err, sql.ErrNoRows) || !create {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return h, err
	}
	h = newHandle()
	_, err = db.Exec("INSERT INTO webauthn_handles (account_id, handle) VALUES (?, ?)", accountID, h)
	return h, err
}

func (s *Service) passkeyCount(db querier, accountID string) int {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM passkeys WHERE account_id = ?", accountID).Scan(&n); err != nil {
		log.Printf("Unable to count the passkeys of %s: %v", accountID, err)
	}
	return n
}

const passkeyColumns = "id, name, credential, created_at, last_used_at" //nolint:gosec // column names

func scanPasskey(row interface{ Scan(...any) error }) (*storedPasskey, error) {
	var p storedPasskey
	var credJSON string
	var created, used int64
	if err := row.Scan(&p.ID, &p.Name, &credJSON, &created, &used); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(credJSON), &p.cred); err != nil {
		return nil, err
	}
	p.CreatedAt = fromMillis(created)
	if used != 0 {
		t := fromMillis(used)
		p.LastUsedAt = &t
	}
	p.BackedUp = p.cred.Flags.BackupState
	p.Transports = []string{}
	for _, t := range p.cred.Transport {
		p.Transports = append(p.Transports, string(t))
	}
	p.Provider = providerName(p.cred.Authenticator.AAGUID)
	return &p, nil
}

func providerName(aaguid []byte) string {
	id, err := uuid.FromBytes(aaguid)
	if err != nil {
		return ""
	}
	return knownAuthenticators[id.String()]
}

func (s *Service) storedPasskeys(accountID string) ([]*storedPasskey, error) {
	rows, err := s.db.Query("SELECT "+passkeyColumns+" FROM passkeys WHERE account_id = ? ORDER BY created_at", accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*storedPasskey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Passkeys lists the passkeys of an account, oldest first
func (s *Service) Passkeys(accountID string) ([]*Passkey, error) {
	stored, err := s.storedPasskeys(accountID)
	if err != nil {
		return nil, err
	}
	out := make([]*Passkey, 0, len(stored))
	for _, p := range stored {
		pk := p.Passkey
		out = append(out, &pk)
	}
	return out, nil
}

// waUserOf loads an account with its passkeys; the handle is created when missing
func (s *Service) waUserOf(u *User) (*waUser, error) {
	h, err := webauthnHandle(s.db, u.ID, true)
	if err != nil {
		return nil, err
	}
	stored, err := s.storedPasskeys(u.ID)
	if err != nil {
		return nil, err
	}
	wu := &waUser{accountID: u.ID, handle: h, name: u.Username, displayName: u.DisplayName}
	for _, p := range stored {
		wu.creds = append(wu.creds, p.cred)
	}
	return wu, nil
}

func cleanPasskeyName(name string, aaguid []byte) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if name = providerName(aaguid); name == "" {
			name = "Passkey"
		}
	}
	if len([]rune(name)) > 64 {
		return "", ErrInvalidPasskeyName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidPasskeyName
		}
	}
	return name, nil
}

func insertPasskey(tx execer, accountID string, cred *webauthn.Credential, name string, now time.Time) (string, error) {
	data, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	id := newID()
	_, err = tx.Exec("INSERT INTO passkeys (id, account_id, credential_id, name, credential, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, accountID, cred.ID, name, string(data), toMillis(now))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return "", ErrPasskeyRegistrationFailed
	}
	return id, err
}

func registrationOptions(existing []webauthn.Credential) []webauthn.RegistrationOption {
	exclude := make([]protocol.CredentialDescriptor, 0, len(existing))
	for i := range existing {
		exclude = append(exclude, existing[i].Descriptor())
	}
	return []webauthn.RegistrationOption{
		webauthn.WithExclusions(exclude),
		webauthn.WithCredentialParameters([]protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgEdDSA},
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgRS256},
		}),
	}
}

func (s *Service) verifyRegistration(c *ceremony, wu *waUser, credential []byte) (*webauthn.Credential, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credential)
	if err != nil {
		return nil, ErrPasskeyRegistrationFailed
	}
	cred, err := s.pk.wa.CreateCredential(wu, c.data, parsed)
	if err != nil {
		log.Printf("Passkey registration failed: %v", err)
		return nil, ErrPasskeyRegistrationFailed
	}
	return cred, nil
}

// BeginPasskeyLogin starts a passkey login: discoverable without a challenge, or as the
// second factor of a password login challenge
func (s *Service) BeginPasskeyLogin(challengeToken string) (string, any, error) {
	if s.pk.wa == nil {
		return "", nil, ErrPasskeysDisabled
	}
	if challengeToken == "" {
		assertion, data, err := s.pk.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return "", nil, err
		}
		return s.startCeremony(&ceremony{kind: ceremonyLogin, data: *data}), assertion.Response, nil
	}
	accountID, err := s.challengeAccount(challengeToken)
	if err != nil {
		return "", nil, err
	}
	u := s.UserByID(accountID)
	if u == nil {
		return "", nil, ErrChallengeExpired
	}
	wu, err := s.waUserOf(u)
	if err != nil {
		return "", nil, err
	}
	if len(wu.creds) == 0 {
		return "", nil, ErrPasskeyVerificationFailed
	}
	assertion, data, err := s.pk.wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return "", nil, err
	}
	return s.startCeremony(&ceremony{kind: ceremonySecondFactor, data: *data, accountID: accountID, challenge: challengeToken}), assertion.Response, nil
}

// verifyAssertion checks a passkey assertion and stores the updated credential. With
// accountID empty (discoverable), the account comes from the user handle.
func (s *Service) verifyAssertion(c *ceremony, credential []byte) (*User, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		return nil, ErrPasskeyVerificationFailed
	}
	var wu *waUser
	var cred *webauthn.Credential
	if c.accountID == "" {
		var found webauthn.User
		found, cred, err = s.pk.wa.ValidatePasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			var accountID string
			if err := s.db.QueryRow("SELECT account_id FROM webauthn_handles WHERE handle = ?", userHandle).Scan(&accountID); err != nil {
				return nil, ErrPasskeyVerificationFailed
			}
			u := s.UserByID(accountID)
			if u == nil {
				return nil, ErrPasskeyVerificationFailed
			}
			return s.waUserOf(u)
		}, c.data, parsed)
		if err == nil {
			wu = found.(*waUser)
		}
	} else {
		u := s.UserByID(c.accountID)
		if u == nil {
			return nil, ErrPasskeyVerificationFailed
		}
		if wu, err = s.waUserOf(u); err == nil {
			cred, err = s.pk.wa.ValidateLogin(wu, c.data, parsed)
		}
	}
	if err != nil {
		log.Printf("Passkey verification failed: %v", err)
		return nil, ErrPasskeyVerificationFailed
	}
	if cred.Authenticator.CloneWarning {
		log.Printf("Passkey of %s reported a lower signature counter (cloned authenticator?)", wu.name)
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	if _, err = s.db.Exec("UPDATE passkeys SET credential = ?, last_used_at = ? WHERE account_id = ? AND credential_id = ?",
		string(data), toMillis(s.now()), wu.accountID, cred.ID); err != nil {
		return nil, err
	}
	return s.UserByID(wu.accountID), nil
}

// FinishPasskeyLogin completes a passkey login and creates the session
func (s *Service) FinishPasskeyLogin(ceremonyID string, credential []byte, p SessionParams) (*LoginResult, error) {
	if s.pk.wa == nil {
		return nil, ErrPasskeysDisabled
	}
	c, err := s.takeCeremony(ceremonyID, ceremonyLogin, ceremonySecondFactor)
	if err != nil {
		return nil, err
	}
	if c.kind == ceremonySecondFactor {
		// the password challenge must still be valid
		if _, err = s.challengeAccount(c.challenge); err != nil {
			return nil, err
		}
	}
	u, err := s.verifyAssertion(c, credential)
	if err != nil {
		s.Audit(AuditEvent{Type: AuditLoginFailed, TargetID: c.accountID, IP: p.IP, UserAgent: p.UserAgent, Details: map[string]any{"method": "passkey"}})
		return nil, err
	}
	if u.Status != StatusActive {
		s.Audit(AuditEvent{Type: AuditLoginFailed, TargetID: u.ID, IP: p.IP, UserAgent: p.UserAgent,
			Details: map[string]any{"method": "passkey", "reason": u.Status}})
		return nil, ErrAccountDisabled
	}
	method := "passkey"
	if c.kind == ceremonySecondFactor {
		s.endChallenge(c.challenge)
		method = "password+passkey"
	}
	s.succeed(usernameKey(u.Username))
	return s.completeLogin(u, p, method)
}

// BeginLinkPasskey starts the passkey registration of a setup, invite or reset link
func (s *Service) BeginLinkPasskey(token string, acc NewAccount) (string, any, error) {
	if s.pk.wa == nil {
		return "", nil, ErrPasskeysDisabled
	}
	l, target, err := s.InspectLink(token)
	if err != nil {
		return "", nil, err
	}
	var wu *waUser
	if l.Kind == LinkReset {
		if wu, err = s.waUserOf(target); err != nil {
			return "", nil, err
		}
		if l.ResetSecondFactors {
			// the old passkeys are removed by the reset
			wu.creds = nil
		}
	} else {
		if err = s.UsernameAvailable(acc.Username); err != nil {
			return "", nil, err
		}
		displayName, err := cleanDisplayName(acc.DisplayName, acc.Username)
		if err != nil {
			return "", nil, err
		}
		acc.DisplayName = displayName
		wu = &waUser{handle: newHandle(), name: acc.Username, displayName: displayName}
	}
	creation, data, err := s.pk.wa.BeginRegistration(wu, registrationOptions(wu.creds)...)
	if err != nil {
		return "", nil, err
	}
	return s.startCeremony(&ceremony{kind: ceremonyLinkRegistration, data: *data, linkToken: token, account: acc, handle: wu.handle,
		accountID: wu.accountID}), creation.Response, nil
}

// FinishLinkPasskey completes the passkey registration of a link and redeems the link
func (s *Service) FinishLinkPasskey(ceremonyID string, credential []byte, passkeyName string, p SessionParams) (*LoginResult, error) {
	if s.pk.wa == nil {
		return nil, ErrPasskeysDisabled
	}
	c, err := s.takeCeremony(ceremonyID, ceremonyLinkRegistration)
	if err != nil {
		return nil, err
	}
	wu := &waUser{accountID: c.accountID, handle: c.handle, name: c.account.Username, displayName: c.account.DisplayName}
	cred, err := s.verifyRegistration(c, wu, credential)
	if err != nil {
		return nil, err
	}
	name, err := cleanPasskeyName(passkeyName, cred.Authenticator.AAGUID)
	if err != nil {
		return nil, err
	}
	return s.redeemLink(c.linkToken, c.account, p, func(tx *sql.Tx, l *Link, accountID string) error {
		if l.Kind == LinkReset {
			h, err := webauthnHandle(tx, accountID, true)
			if err != nil {
				return err
			}
			if !bytes.Equal(h, c.handle) {
				return ErrChallengeExpired
			}
		} else if _, err := tx.Exec("INSERT INTO webauthn_handles (account_id, handle) VALUES (?, ?)", accountID, c.handle); err != nil {
			return err
		}
		now := s.now().UTC()
		if _, err := insertPasskey(tx, accountID, cred, name, now); err != nil {
			return err
		}
		return insertAudit(tx, now, AuditEvent{Type: AuditPasskeyAdded, ActorID: accountID, TargetID: accountID, IP: p.IP, UserAgent: p.UserAgent,
			Details: map[string]any{"name": name, "linkId": l.ID}})
	})
}

// BeginAddPasskey starts the registration of another passkey of a signed-in user
func (s *Service) BeginAddPasskey(u *User) (string, any, error) {
	if s.pk.wa == nil {
		return "", nil, ErrPasskeysDisabled
	}
	wu, err := s.waUserOf(u)
	if err != nil {
		return "", nil, err
	}
	creation, data, err := s.pk.wa.BeginRegistration(wu, registrationOptions(wu.creds)...)
	if err != nil {
		return "", nil, err
	}
	return s.startCeremony(&ceremony{kind: ceremonyAccountRegistration, data: *data, accountID: u.ID}), creation.Response, nil
}

// FinishAddPasskey completes the registration of another passkey
func (s *Service) FinishAddPasskey(u *User, ceremonyID string, credential []byte, name string, ri RequestInfo) (*Passkey, error) {
	if s.pk.wa == nil {
		return nil, ErrPasskeysDisabled
	}
	c, err := s.takeCeremony(ceremonyID, ceremonyAccountRegistration)
	if err != nil {
		return nil, err
	}
	if c.accountID != u.ID {
		return nil, ErrChallengeExpired
	}
	wu, err := s.waUserOf(u)
	if err != nil {
		return nil, err
	}
	cred, err := s.verifyRegistration(c, wu, credential)
	if err != nil {
		return nil, err
	}
	if name, err = cleanPasskeyName(name, cred.Authenticator.AAGUID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	id, err := insertPasskey(s.db, u.ID, cred, name, now)
	if err != nil {
		return nil, err
	}
	s.invalidateBasicCache(u.ID)
	s.Audit(AuditEvent{Type: AuditPasskeyAdded, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent, Details: map[string]any{"name": name, "passkeyId": id}})
	return s.passkey(u.ID, id)
}

func (s *Service) passkey(accountID, id string) (*Passkey, error) {
	p, err := scanPasskey(s.db.QueryRow("SELECT "+passkeyColumns+" FROM passkeys WHERE id = ? AND account_id = ?", id, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p.Passkey, nil
}

// RenamePasskey changes the name of a passkey
func (s *Service) RenamePasskey(u *User, id, name string, ri RequestInfo) (*Passkey, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrInvalidPasskeyName
	}
	name, err := cleanPasskeyName(name, nil)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec("UPDATE passkeys SET name = ? WHERE id = ? AND account_id = ?", name, id, u.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	s.Audit(AuditEvent{Type: AuditPasskeyRenamed, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent, Details: map[string]any{"name": name, "passkeyId": id}})
	return s.passkey(u.ID, id)
}

// DeletePasskey removes a passkey, unless it is the last way to log in or the last second
// factor while the instance requires one
func (s *Service) DeletePasskey(u *User, id string, ri RequestInfo) error {
	if _, err := s.passkey(u.ID, id); err != nil {
		return err
	}
	if s.passkeyCount(s.db, u.ID) == 1 {
		hash, err := s.passwordHash(s.db, u.ID)
		if err != nil {
			return err
		}
		if hash == "" {
			return ErrLastLoginMethod
		}
		totp, err := totpEnabled(s.db, u.ID)
		if err != nil {
			return err
		}
		if !totp && s.Require2FA() {
			return ErrSecondFactorRequired
		}
	}
	if _, err := s.db.Exec("DELETE FROM passkeys WHERE id = ? AND account_id = ?", id, u.ID); err != nil {
		return err
	}
	s.Audit(AuditEvent{Type: AuditPasskeyRemoved, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent, Details: map[string]any{"passkeyId": id}})
	return nil
}

// BeginReauthPasskey starts a step-up with a passkey of the signed-in user
func (s *Service) BeginReauthPasskey(u *User, sess *Session) (string, any, error) {
	if s.pk.wa == nil {
		return "", nil, ErrPasskeysDisabled
	}
	wu, err := s.waUserOf(u)
	if err != nil {
		return "", nil, err
	}
	if len(wu.creds) == 0 {
		return "", nil, ErrPasskeyVerificationFailed
	}
	assertion, data, err := s.pk.wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return "", nil, err
	}
	return s.startCeremony(&ceremony{kind: ceremonyReauth, data: *data, accountID: u.ID, sessionID: sess.ID}), assertion.Response, nil
}

// FinishReauthPasskey completes a step-up with a passkey
func (s *Service) FinishReauthPasskey(u *User, sess *Session, ceremonyID string, credential []byte, ri RequestInfo) (time.Time, error) {
	if s.pk.wa == nil {
		return time.Time{}, ErrPasskeysDisabled
	}
	c, err := s.takeCeremony(ceremonyID, ceremonyReauth)
	if err != nil {
		return time.Time{}, err
	}
	if c.accountID != u.ID || c.sessionID != sess.ID {
		return time.Time{}, ErrChallengeExpired
	}
	if _, err = s.verifyAssertion(c, credential); err != nil {
		s.Audit(AuditEvent{Type: AuditReauthFailed, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent, Details: map[string]any{"method": "passkey"}})
		return time.Time{}, err
	}
	if err = s.markReauthenticated(sess); err != nil {
		return time.Time{}, err
	}
	s.Audit(AuditEvent{Type: AuditReauthSucceeded, ActorID: u.ID, TargetID: u.ID, IP: ri.IP, UserAgent: ri.UserAgent,
		Details: map[string]any{"method": "passkey", "sessionId": sess.ID}})
	return ReauthenticatedUntil(sess), nil
}
