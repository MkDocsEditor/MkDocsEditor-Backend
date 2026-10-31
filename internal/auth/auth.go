// Package auth authenticates users against the configured user list and issues
// short-lived, single-use tickets for websocket connections.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"golang.org/x/crypto/bcrypt"
)

// Roles of a user
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleReader = "reader"
)

const (
	// TicketLifetime is how long a websocket ticket stays valid
	TicketLifetime = 60 * time.Second
	// how long a successful password check is remembered, so not every request pays for bcrypt
	verificationCacheLifetime = 10 * time.Minute
	// AnonymousUsername is the user of all requests when no authentication is configured
	AnonymousUsername = "anonymous"
)

// palette of user colours, picked by a hash of the username
var palette = []string{
	"#e03131", "#c2255c", "#9c36b5", "#6741d9", "#3b5bdb", "#1971c2",
	"#0c8599", "#099268", "#2f9e44", "#66a80f", "#f08c00", "#e8590c",
}

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// dummyHash is compared against for unknown users, so response times do not reveal usernames
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy password"), 12)
	if err != nil {
		panic(err)
	}
	return h
})

// User is an authenticated user
type User struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Email       string `json:"-"`
	Role        string `json:"role"`
	Color       string `json:"color"`
}

// CanWrite reports whether the user may change documents and the tree
func (u *User) CanWrite() bool {
	return u.Role == RoleAdmin || u.Role == RoleEditor
}

type account struct {
	user *User
	// bcrypt hash, or empty when plainPassword is used (legacy basicAuth)
	passwordHash  []byte
	plainPassword string
}

type cachedVerification struct {
	digest  [32]byte
	expires time.Time
}

type ticket struct {
	user    *User
	expires time.Time
}

// Authenticator checks credentials and manages websocket tickets
type Authenticator struct {
	accounts map[string]*account
	lock     sync.Mutex
	cache    map[string]cachedVerification
	tickets  map[string]ticket
	now      func() time.Time
}

// NewAuthenticator creates an Authenticator from the server configuration.
// It returns an error if the configuration is invalid.
func NewAuthenticator(config configuration.ServerConfiguration) (*Authenticator, error) {
	a := &Authenticator{
		accounts: map[string]*account{},
		cache:    map[string]cachedVerification{},
		tickets:  map[string]ticket{},
		now:      time.Now,
	}
	for i, uc := range config.Auth.Users {
		if uc.Username == "" {
			return nil, fmt.Errorf("server.auth.users[%d]: username is empty", i)
		}
		if strings.ContainsAny(uc.Username, ":") {
			return nil, fmt.Errorf("server.auth.users[%d]: username must not contain ':'", i)
		}
		if _, exists := a.accounts[uc.Username]; exists {
			return nil, fmt.Errorf("server.auth.users[%d]: duplicate username %q", i, uc.Username)
		}
		passwordHash := normalizeHash(uc.PasswordHash)
		if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
			return nil, fmt.Errorf("server.auth.users[%d] (%s): passwordHash is not a bcrypt hash: %w", i, uc.Username, err)
		}
		role := uc.Role
		if role == "" {
			role = RoleEditor
		}
		if role != RoleAdmin && role != RoleEditor && role != RoleReader {
			return nil, fmt.Errorf("server.auth.users[%d] (%s): unknown role %q", i, uc.Username, uc.Role)
		}
		if uc.Color != "" && !colorPattern.MatchString(uc.Color) {
			return nil, fmt.Errorf("server.auth.users[%d] (%s): color must look like #1971c2", i, uc.Username)
		}
		a.accounts[uc.Username] = &account{
			user:         newUser(uc.Username, uc.DisplayName, uc.Email, role, uc.Color),
			passwordHash: []byte(passwordHash),
		}
	}

	legacy := config.BasicAuth
	if legacy.User != "" && legacy.Password != "" {
		if _, exists := a.accounts[legacy.User]; exists {
			return nil, fmt.Errorf("server.basicAuth.user %q is also in server.auth.users", legacy.User)
		}
		a.accounts[legacy.User] = &account{
			user:          newUser(legacy.User, "", "", RoleAdmin, ""),
			plainPassword: legacy.Password,
		}
	}
	return a, nil
}

// normalizeHash accepts the $2y$ prefix of htpasswd and PHP, which is the same algorithm as $2b$
func normalizeHash(h string) string {
	if strings.HasPrefix(h, "$2y$") {
		return "$2b$" + h[4:]
	}
	return h
}

func newUser(username, displayName, email, role, color string) *User {
	if displayName == "" {
		displayName = username
	}
	if color == "" {
		color = ColorForUsername(username)
	}
	return &User{Username: username, DisplayName: displayName, Email: email, Role: role, Color: color}
}

// ColorForUsername returns the stable default colour of a user
func ColorForUsername(username string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(username))
	return palette[int(h.Sum32()%12)%len(palette)]
}

// Enabled reports whether any user is configured. Without users, authentication is off.
func (a *Authenticator) Enabled() bool {
	return len(a.accounts) > 0
}

// Anonymous is the user of all requests when authentication is off
func Anonymous() *User {
	return newUser(AnonymousUsername, "Anonymous", "", RoleAdmin, "")
}

// User returns the configured user with the given name, or nil
func (a *Authenticator) User(username string) *User {
	if acc, ok := a.accounts[username]; ok {
		return acc.user
	}
	return nil
}

// Users returns all configured users
func (a *Authenticator) Users() []*User {
	users := make([]*User, 0, len(a.accounts))
	for _, acc := range a.accounts {
		users = append(users, acc.user)
	}
	return users
}

// Authenticate checks a username and password. It returns the user, or nil if the credentials are wrong.
func (a *Authenticator) Authenticate(username, password string) *User {
	acc, ok := a.accounts[username]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		return nil
	}

	if acc.passwordHash == nil {
		if subtle.ConstantTimeCompare([]byte(password), []byte(acc.plainPassword)) == 1 {
			return acc.user
		}
		return nil
	}

	digest := sha256.Sum256([]byte(username + "\x00" + password))
	a.lock.Lock()
	cached, hit := a.cache[username]
	a.lock.Unlock()
	if hit && a.now().Before(cached.expires) && subtle.ConstantTimeCompare(cached.digest[:], digest[:]) == 1 {
		return acc.user
	}

	if bcrypt.CompareHashAndPassword(acc.passwordHash, []byte(password)) != nil {
		return nil
	}
	a.lock.Lock()
	a.cache[username] = cachedVerification{digest: digest, expires: a.now().Add(verificationCacheLifetime)}
	a.lock.Unlock()
	return acc.user
}

// IssueTicket creates a single-use websocket ticket for the given user
func (a *Authenticator) IssueTicket(user *User) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	value := base64.RawURLEncoding.EncodeToString(b)
	expires := a.now().Add(TicketLifetime)

	a.lock.Lock()
	defer a.lock.Unlock()
	// drop expired tickets
	for k, t := range a.tickets {
		if !a.now().Before(t.expires) {
			delete(a.tickets, k)
		}
	}
	a.tickets[value] = ticket{user: user, expires: expires}
	return value, expires, nil
}

// ErrInvalidTicket is returned for unknown, used or expired tickets
var ErrInvalidTicket = errors.New("invalid or expired ticket")

// RedeemTicket returns the user of a ticket and invalidates it
func (a *Authenticator) RedeemTicket(value string) (*User, error) {
	a.lock.Lock()
	defer a.lock.Unlock()
	t, ok := a.tickets[value]
	if !ok {
		return nil, ErrInvalidTicket
	}
	delete(a.tickets, value)
	if !a.now().Before(t.expires) {
		return nil, ErrInvalidTicket
	}
	return t.user, nil
}

// HashPassword creates a bcrypt hash for the configuration
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(h), err
}
