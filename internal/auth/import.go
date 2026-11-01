package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	settingConfigImported = "config_users_imported"
	settingActorsImported = "actors_file_imported"

	// ActorsFileName is the file that held the actor bindings before the database
	ActorsFileName = "actors.json"
)

type importedUser struct {
	username, displayName, email, role, color, passwordHash string
}

// validateConfiguredUsers checks server.auth.users and server.basicAuth like the old
// file-based authentication did
func (s *Service) validateConfiguredUsers() ([]importedUser, error) {
	conf := s.opts.Server
	var users []importedUser
	seen := map[string]bool{}
	for i, uc := range conf.Auth.Users {
		if uc.Username == "" {
			return nil, fmt.Errorf("server.auth.users[%d]: username is empty", i)
		}
		if strings.Contains(uc.Username, ":") {
			return nil, fmt.Errorf("server.auth.users[%d]: username must not contain ':'", i)
		}
		if seen[usernameKey(uc.Username)] {
			return nil, fmt.Errorf("server.auth.users[%d]: duplicate username %q (usernames are unique ignoring case)", i, uc.Username)
		}
		seen[usernameKey(uc.Username)] = true
		if !isBcrypt(uc.PasswordHash) {
			return nil, fmt.Errorf("server.auth.users[%d] (%s): passwordHash is not a bcrypt hash", i, uc.Username)
		}
		role := uc.Role
		if role == "" {
			role = RoleEditor
		}
		if err := ValidateRole(role); err != nil {
			return nil, fmt.Errorf("server.auth.users[%d] (%s): %w", i, uc.Username, err)
		}
		if uc.Color != "" && !colorPattern.MatchString(uc.Color) {
			return nil, fmt.Errorf("server.auth.users[%d] (%s): color must look like #1971c2", i, uc.Username)
		}
		users = append(users, importedUser{username: uc.Username, displayName: uc.DisplayName, email: uc.Email,
			role: role, color: uc.Color, passwordHash: normalizeBcrypt(uc.PasswordHash)})
	}

	legacy := conf.BasicAuth
	if legacy.User != "" && legacy.Password != "" {
		if strings.Contains(legacy.User, ":") {
			return nil, errors.New("server.basicAuth.user must not contain ':'")
		}
		if seen[usernameKey(legacy.User)] {
			return nil, fmt.Errorf("server.basicAuth.user %q is also in server.auth.users", legacy.User)
		}
		h, err := HashPassword(legacy.Password)
		if err != nil {
			return nil, err
		}
		users = append(users, importedUser{username: legacy.User, role: RoleAdmin, passwordHash: h})
	}
	return users, nil
}

// importConfiguredUsers imports server.auth.users and server.basicAuth into an empty
// database, once
func (s *Service) importConfiguredUsers() error {
	done, err := s.setting(settingConfigImported)
	if err != nil {
		return err
	}
	configured := len(s.opts.Server.Auth.Users) > 0 || (s.opts.Server.BasicAuth.User != "" && s.opts.Server.BasicAuth.Password != "")
	if done != "" {
		if configured {
			log.Printf("WARNING: server.auth.users and server.basicAuth are ignored: the accounts were imported into %s on %s. Remove them from the configuration.", DatabaseFileName, done)
		}
		return nil
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM accounts WHERE status != 'deleted'").Scan(&count); err != nil {
		return err
	}
	if count > 0 || !configured {
		return nil
	}
	users, err := s.validateConfiguredUsers()
	if err != nil {
		return fmt.Errorf("invalid user configuration: %w", err)
	}

	now := s.now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var ids []string
	for _, u := range users {
		id, err := insertAccount(tx, accountRecord{
			username: u.username, displayName: u.displayName, email: u.email, color: u.color,
			role: u.role, status: StatusActive, passwordHash: u.passwordHash,
		}, now)
		if err != nil {
			return fmt.Errorf("importing %s: %w", u.username, err)
		}
		ids = append(ids, id)
	}
	if err = setSetting(tx, settingConfigImported, now.Format(time.RFC3339)); err != nil {
		return err
	}
	for i, u := range users {
		if err = insertAudit(tx, now, AuditEvent{Type: AuditAccountImported, TargetID: ids[i],
			Details: map[string]any{"username": u.username, "role": u.role}}); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidateCache()
	for _, u := range users {
		log.Printf("Imported the configured user %q (%s) into %s", u.username, u.role, DatabaseFileName)
	}
	log.Printf("From now on server.auth.users and server.basicAuth are ignored; manage the accounts through the API and remove them from the configuration.")
	return nil
}

// accountRecord holds the fields of a new account
type accountRecord struct {
	username, displayName, email, color, role, status, passwordHash string
}

// insertAccount creates an account. The username must be free (also as a former username).
func insertAccount(tx *sql.Tx, a accountRecord, now time.Time) (string, error) {
	key := usernameKey(a.username)
	var taken int
	if err := tx.QueryRow("SELECT (SELECT COUNT(*) FROM accounts WHERE username_key = ?) + (SELECT COUNT(*) FROM username_aliases WHERE username_key = ?)", key, key).Scan(&taken); err != nil {
		return "", err
	}
	if taken > 0 {
		return "", ErrUsernameTaken
	}
	if a.displayName == "" {
		a.displayName = a.username
	}
	if a.color == "" {
		a.color = ColorForUsername(a.username)
	}
	id := newID()
	ms := toMillis(now)
	var deletedAt int64
	if a.status == StatusDeleted {
		deletedAt = ms
	}
	var changedAt int64
	if a.passwordHash != "" {
		changedAt = ms
	}
	_, err := tx.Exec(`INSERT INTO accounts (id, username, username_key, display_name, email, color, role, status,
		password_hash, password_changed_at, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, a.username, key, a.displayName, a.email, a.color, a.role, a.status, a.passwordHash, changedAt, ms, ms, deletedAt)
	return id, err
}

// ErrUsernameTaken is returned when a username (or a former username) exists
var ErrUsernameTaken = errors.New("the username is taken")

type legacyActorBinding struct {
	Username string    `json:"username"`
	Bound    time.Time `json:"bound"`
}

// importActorsFile moves the bindings of actors.json into the database. Usernames become
// account ids; usernames without an account become deleted accounts (tombstones), so their
// authorship still resolves. The file is renamed to actors.json.imported afterwards.
func (s *Service) importActorsFile() error {
	done, err := s.setting(settingActorsImported)
	if err != nil || done != "" {
		return err
	}
	path := filepath.Join(s.opts.StateDir, ActorsFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return setSetting(s.db, settingActorsImported, s.now().UTC().Format(time.RFC3339))
	}
	if err != nil {
		return err
	}
	var bindings map[string]legacyActorBinding
	if err = json.Unmarshal(data, &bindings); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	now := s.now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	subjects := map[string]string{}
	resolve := func(username string) (string, error) {
		if strings.HasPrefix(username, ":") {
			return username, nil
		}
		if id, ok := subjects[username]; ok {
			return id, nil
		}
		key := usernameKey(username)
		var id string
		err := tx.QueryRow("SELECT id FROM accounts WHERE username_key = ? UNION SELECT account_id FROM username_aliases WHERE username_key = ?", key, key).Scan(&id)
		switch {
		case err == nil:
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		case username == AnonymousUsername:
			id = AnonymousID
		default:
			id, err = insertAccount(tx, accountRecord{username: username, role: RoleReader, status: StatusDeleted}, now)
			if err != nil {
				return "", err
			}
			log.Printf("The former user %q has actor ids but no account; created a deleted account for the authorship", username)
		}
		subjects[username] = id
		return id, nil
	}

	for actorID, b := range bindings {
		subject, err := resolve(b.Username)
		if err != nil {
			return fmt.Errorf("importing the actor bindings: %w", err)
		}
		bound := b.Bound
		if bound.IsZero() {
			bound = now
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO actor_bindings (actor_id, subject, bound_at) VALUES (?, ?, ?)", actorID, subject, toMillis(bound)); err != nil {
			return err
		}
	}
	if err = setSetting(tx, settingActorsImported, now.Format(time.RFC3339)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidateCache()
	log.Printf("Imported %d actor bindings from %s into %s", len(bindings), path, DatabaseFileName)
	if err = os.Rename(path, path+".imported"); err != nil {
		log.Printf("Unable to rename %s (it is not read again): %v", path, err)
	}
	return nil
}
