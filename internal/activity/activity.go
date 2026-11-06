// Package activity keeps what the dashboard shows: the pinned documents and the change feed.
// Both live in their own SQLite database in the state directory, see docs/dashboard.md.
//
// The package knows paths relative to the docs folder and account ids, nothing about HTTP, the
// tree or Automerge. Writes of the feed go through a queue, so recording never blocks the
// caller, which may hold a document lock.
package activity

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	// pure Go SQLite driver, registers "sqlite"
	_ "modernc.org/sqlite"
)

// DatabaseFileName is the name of the activity database in the state directory
const DatabaseFileName = "activity.db"

const (
	// EditSession is how long after the last edit of a document a new edit extends its entry
	EditSession = 15 * time.Minute
	// Retention is how long feed entries are kept
	Retention = 365 * 24 * time.Hour
	// queueSize is the number of feed writes that may wait. Beyond it they are dropped.
	queueSize = 1024
	// cleanupInterval is how often old entries are removed
	cleanupInterval = time.Hour
)

// migrations are applied in order; the schema version is PRAGMA user_version.
// Never change a released migration, only append new ones.
var migrations = []string{
	// 1: pins and the feed
	`
CREATE TABLE pins (
	account_id TEXT NOT NULL,
	path       TEXT NOT NULL,
	pinned_by  TEXT NOT NULL,
	pinned_at  INTEGER NOT NULL,
	PRIMARY KEY (account_id, path)
);
CREATE TABLE feed (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	time          INTEGER NOT NULL,
	kind          TEXT NOT NULL,
	item_type     TEXT NOT NULL,
	path          TEXT NOT NULL,
	old_path      TEXT NOT NULL DEFAULT '',
	authors       TEXT NOT NULL DEFAULT '[]',
	lines_added   INTEGER NOT NULL DEFAULT 0,
	lines_removed INTEGER NOT NULL DEFAULT 0,
	headings      TEXT NOT NULL DEFAULT '[]',
	base_text     TEXT
);
CREATE INDEX feed_time ON feed(time, id);
CREATE INDEX feed_open ON feed(path) WHERE base_text IS NOT NULL;
`,
}

// Store is the activity database
type Store struct {
	db *sql.DB
	// now is the clock, replaced in tests
	now func() time.Time

	queue chan func()
	done  chan struct{}

	closeOnce sync.Once
	// queueMu guards closed and sending to queue. The worker never takes it.
	queueMu sync.Mutex
	closed  bool
	// mu guards onFeedChanged
	mu            sync.Mutex
	onFeedChanged func()
}

// Open opens (and creates) the activity database in the state directory
func Open(stateDir string) (*Store, error) {
	return open(stateDir, time.Now)
}

func open(stateDir string, now func() time.Time) (*Store, error) {
	path := filepath.Join(stateDir, DatabaseFileName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)"} {
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
	s := &Store{db: db, now: now, queue: make(chan func(), queueSize), done: make(chan struct{})}
	go s.worker()
	return s, nil
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
	if version == len(migrations) {
		return nil
	}
	for i := version; i < len(migrations); i++ {
		if _, err = tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

// OnFeedChanged sets the function that is called after the feed changed. It runs on the worker,
// so it must not block for long.
func (s *Store) OnFeedChanged(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onFeedChanged = f
}

func (s *Store) feedChanged() {
	s.mu.Lock()
	f := s.onFeedChanged
	s.mu.Unlock()
	if f != nil {
		f()
	}
}

// enqueue runs a write on the worker. It never blocks: when the queue is full, the write is lost.
func (s *Store) enqueue(job func()) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- job:
	default:
		log.Printf("The activity queue is full, a feed entry is lost")
	}
}

// Wait returns when the writes queued before are done
func (s *Store) Wait() {
	done := make(chan struct{})
	s.queueMu.Lock()
	if s.closed {
		s.queueMu.Unlock()
		return
	}
	s.queue <- func() { close(done) }
	s.queueMu.Unlock()
	<-done
}

func (s *Store) worker() {
	defer close(s.done)
	s.cleanup()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case job, ok := <-s.queue:
			if !ok {
				return
			}
			job()
		case <-ticker.C:
			s.cleanup()
		}
	}
}

// cleanup removes the entries older than the retention and forgets the start texts of edit
// sessions that ended
func (s *Store) cleanup() {
	now := s.now()
	if _, err := s.db.Exec(`DELETE FROM feed WHERE time < ?`, now.Add(-Retention).UnixMilli()); err != nil {
		log.Printf("Unable to remove old feed entries: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE feed SET base_text = NULL WHERE base_text IS NOT NULL AND time < ?`, now.Add(-EditSession).UnixMilli()); err != nil {
		log.Printf("Unable to end old edit sessions: %v", err)
	}
}

// Close finishes the queued writes and closes the database
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.queueMu.Lock()
		s.closed = true
		close(s.queue)
		s.queueMu.Unlock()
		<-s.done
		err = s.db.Close()
	})
	return err
}
