package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// pure Go SQLite driver, registers "sqlite"
	_ "modernc.org/sqlite"
)

// DatabaseFileName is the name of the account database in the state directory
const DatabaseFileName = "auth.db"

// migrations are applied in order; the schema version is PRAGMA user_version.
// Never change a released migration, only append new ones.
var migrations = []string{
	// 1: accounts, former usernames, actor bindings, settings, audit log
	`
CREATE TABLE accounts (
	id                  TEXT PRIMARY KEY,
	username            TEXT NOT NULL,
	username_key        TEXT NOT NULL UNIQUE,
	display_name        TEXT NOT NULL,
	email               TEXT NOT NULL DEFAULT '',
	color               TEXT NOT NULL,
	role                TEXT NOT NULL CHECK (role IN ('admin', 'editor', 'reader')),
	status              TEXT NOT NULL CHECK (status IN ('active', 'disabled', 'deleted')),
	password_hash       TEXT NOT NULL DEFAULT '',
	password_changed_at INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL,
	last_login_at       INTEGER NOT NULL DEFAULT 0,
	deleted_at          INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE username_aliases (
	username_key TEXT PRIMARY KEY,
	account_id   TEXT NOT NULL REFERENCES accounts(id),
	created_at   INTEGER NOT NULL
);
CREATE INDEX username_aliases_account ON username_aliases(account_id);
CREATE TABLE actor_bindings (
	actor_id TEXT PRIMARY KEY,
	subject  TEXT NOT NULL,
	bound_at INTEGER NOT NULL
);
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE audit_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	time       INTEGER NOT NULL,
	type       TEXT NOT NULL,
	actor_id   TEXT NOT NULL DEFAULT '',
	target_id  TEXT NOT NULL DEFAULT '',
	ip         TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT '',
	details    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_events_time ON audit_events(time);
`,
	// 2: sessions and links (setup, invite, reset)
	`
CREATE TABLE sessions (
	id           TEXT PRIMARY KEY,
	account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	token_hash   BLOB NOT NULL UNIQUE,
	type         TEXT NOT NULL CHECK (type IN ('cookie', 'bearer')),
	device_name  TEXT NOT NULL DEFAULT '',
	user_agent   TEXT NOT NULL DEFAULT '',
	ip           TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	reauth_at    INTEGER NOT NULL
);
CREATE INDEX sessions_account ON sessions(account_id);
CREATE TABLE links (
	id                   TEXT PRIMARY KEY,
	kind                 TEXT NOT NULL CHECK (kind IN ('setup', 'invite', 'reset')),
	token_hash           BLOB NOT NULL UNIQUE,
	role                 TEXT NOT NULL DEFAULT '',
	account_id           TEXT NOT NULL DEFAULT '',
	note                 TEXT NOT NULL DEFAULT '',
	reset_second_factors INTEGER NOT NULL DEFAULT 0,
	created_by           TEXT NOT NULL DEFAULT '',
	created_at           INTEGER NOT NULL,
	expires_at           INTEGER NOT NULL,
	used_at              INTEGER NOT NULL DEFAULT 0,
	used_by              TEXT NOT NULL DEFAULT '',
	revoked_at           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX links_kind ON links(kind, created_at);
`,
}

// openDatabase opens (and creates) the account database and applies the migrations
func openDatabase(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// create the file with restrictive permissions before SQLite does
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if err = migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("the database has schema version %d, this backend knows only %d: it was used by a newer version", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err = tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if version == len(migrations) {
		return nil
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

// unix milliseconds, 0 for the zero time
func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// setting reads an instance setting, "" if it is not set
func (s *Service) setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func setSetting(db execer, key, value string) error {
	_, err := db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	return err
}
