// Package auth manages the accounts of the backend: storage in an embedded SQLite database,
// passwords, sessions, websocket tickets and the bindings of Automerge actor ids to accounts.
// See docs/auth.md.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"golang.org/x/text/unicode/norm"
)

// Roles of a user
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleReader = "reader"
)

// Account states
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusDeleted  = "deleted"
)

const (
	// AnonymousID is the account id and username of all requests when authentication is off
	AnonymousID = "anonymous"
	// AnonymousUsername is the username of the anonymous user
	AnonymousUsername = "anonymous"
)

// palette of user colours, picked by a hash of the username
var palette = []string{
	"#e03131", "#c2255c", "#9c36b5", "#6741d9", "#3b5bdb", "#1971c2",
	"#0c8599", "#099268", "#2f9e44", "#66a80f", "#f08c00", "#e8590c",
}

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// User is the public view of an account. Instances are snapshots and never change.
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Email       string `json:"-"`
	Role        string `json:"role"`
	Color       string `json:"color"`
	Status      string `json:"-"`
}

// CanWrite reports whether the user may change documents and the tree
func (u *User) CanWrite() bool {
	return u.Role == RoleAdmin || u.Role == RoleEditor
}

// IsAdmin reports whether the user is an admin
func (u *User) IsAdmin() bool {
	return u.Role == RoleAdmin
}

// Anonymous is the user of all requests when authentication is off
func Anonymous() *User {
	return &User{ID: AnonymousID, Username: AnonymousUsername, DisplayName: "Anonymous", Role: RoleAdmin,
		Color: ColorForUsername(AnonymousUsername), Status: StatusActive}
}

// ColorForUsername returns the stable default colour of a user
func ColorForUsername(username string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(username))
	return palette[int(h.Sum32()%12)%len(palette)]
}

// usernameKey is the case-insensitive form of a username used for uniqueness
func usernameKey(username string) string {
	return strings.ToLower(norm.NFKC.String(username))
}

// Options configure the Service
type Options struct {
	// StateDir holds the database (auth.db) and, before the migration, actors.json
	StateDir string
	// Server is the server configuration (auth settings and the users to import)
	Server configuration.ServerConfiguration
}

// Service is the account system
type Service struct {
	db   *sql.DB
	opts Options
	cfg  settings
	keys *serverKeys
	now  func() time.Time

	limiter *limiter

	listenersMu sync.Mutex
	listeners   []func(RevokeEvent)

	challengesMu sync.Mutex
	challenges   map[string]*challenge

	stop     chan struct{}
	stopOnce sync.Once

	// in-memory view of all accounts, rebuilt after every change
	cacheMu    sync.Mutex
	cacheValid bool
	byID       map[string]*User
	byUsername map[string]string // username key (current and former) -> account id
	active     int

	ticketsMu sync.Mutex
	tickets   map[string]ticket

	basicMu    sync.Mutex
	basicCache map[string]cachedVerification
	// basicKey keys the digests in basicCache; it exists only in memory and changes on
	// every start, so a memory dump of the cache cannot be brute-forced offline without it
	basicKey []byte
}

// Open opens the account database in the state directory and imports the configured users
// and the actor bindings of actors.json on the first start.
func Open(opts Options) (*Service, error) {
	cfg, err := newSettings(opts.Server.Auth)
	if err != nil {
		return nil, err
	}
	db, err := openDatabase(filepath.Join(opts.StateDir, DatabaseFileName))
	if err != nil {
		return nil, err
	}
	keys, err := loadServerKeys(opts.StateDir, cfg.secretKeyFile)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Service{
		db:         db,
		opts:       opts,
		cfg:        cfg,
		keys:       keys,
		now:        time.Now,
		limiter:    newLimiter(),
		challenges: map[string]*challenge{},
		stop:       make(chan struct{}),
		tickets:    map[string]ticket{},
		basicCache: map[string]cachedVerification{},
		basicKey:   make([]byte, 32),
	}
	if _, err = rand.Read(s.basicKey); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.importConfiguredUsers(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.importActorsFile(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.pruneExpired()
	go s.maintenance()
	return s, nil
}

func (s *Service) maintenance() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.pruneExpired()
		}
	}
}

// Close closes the database
func (s *Service) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	return s.db.Close()
}

// Enabled reports whether authentication is on (not server.auth.disabled). While no
// account exists, the server waits for the setup (SetupRequired).
func (s *Service) Enabled() bool {
	return !s.cfg.disabled
}

// SetClock replaces the time source. Only for tests.
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

func (s *Service) invalidateCache() {
	s.cacheMu.Lock()
	s.cacheValid = false
	s.cacheMu.Unlock()
}

func (s *Service) loadCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheValid {
		return
	}
	byID := map[string]*User{}
	byUsername := map[string]string{}
	active := 0
	rows, err := s.db.Query("SELECT id, username, username_key, display_name, email, color, role, status FROM accounts")
	if err != nil {
		log.Printf("Unable to read the accounts: %v", err)
		return
	}
	for rows.Next() {
		var u User
		var key string
		if err = rows.Scan(&u.ID, &u.Username, &key, &u.DisplayName, &u.Email, &u.Color, &u.Role, &u.Status); err != nil {
			log.Printf("Unable to read the accounts: %v", err)
			_ = rows.Close()
			return
		}
		byID[u.ID] = &u
		byUsername[key] = u.ID
		if u.Status != StatusDeleted {
			active++
		}
	}
	_ = rows.Close()
	rows, err = s.db.Query("SELECT username_key, account_id FROM username_aliases")
	if err != nil {
		log.Printf("Unable to read the former usernames: %v", err)
		return
	}
	for rows.Next() {
		var key, id string
		if err = rows.Scan(&key, &id); err == nil {
			if _, taken := byUsername[key]; !taken {
				byUsername[key] = id
			}
		}
	}
	_ = rows.Close()
	s.byID, s.byUsername, s.active, s.cacheValid = byID, byUsername, active, true
}

// UserByID returns the account with the given id in any state (deleted accounts are
// tombstones), the anonymous user for AnonymousID, or nil.
func (s *Service) UserByID(id string) *User {
	if id == AnonymousID {
		return Anonymous()
	}
	s.loadCache()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.byID[id]
}

// UserByUsername returns the account that has or had the username (ignoring case), or nil
func (s *Service) UserByUsername(username string) *User {
	s.loadCache()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if id, ok := s.byUsername[usernameKey(username)]; ok {
		return s.byID[id]
	}
	return nil
}

// Users returns all accounts, including deleted ones
func (s *Service) Users() []*User {
	s.loadCache()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	users := make([]*User, 0, len(s.byID))
	for _, u := range s.byID {
		users = append(users, u)
	}
	return users
}

// newID returns a random id of 26 lower-case base32 characters (128 bits)
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// ErrNotFound is returned for unknown objects
var ErrNotFound = errors.New("not found")

// ValidateRole checks that a role is admin, editor or reader
func ValidateRole(role string) error {
	return validateRole(role)
}

func validateRole(role string) error {
	switch role {
	case RoleAdmin, RoleEditor, RoleReader:
		return nil
	}
	return fmt.Errorf("unknown role %q", role)
}
