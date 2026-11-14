package activity

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// What each account last saw of a document, for "changed since you were here". The text is
// stored itself: it outlives Automerge epochs and git commits, and the changes are a diff of
// it against the live text. See docs/sync-protocol.md.

// Seen is what an account last saw of a document
type Seen struct {
	Time time.Time
	// Token is the revision (Automerge heads) of Text
	Token string
	Text  string
}

// SetSeen stores what an account saw of a document
func (s *Store) SetSeen(accountID string, path string, token string, text string) (time.Time, error) {
	at := s.now().UTC()
	_, err := s.db.Exec(`INSERT INTO seen (account_id, path, seen_at, seen_token, text) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (account_id, path) DO UPDATE SET seen_at = excluded.seen_at, seen_token = excluded.seen_token, text = excluded.text`,
		accountID, path, at.UnixMilli(), token, text)
	return at, err
}

// LastSeen returns what an account last saw of a document, nil if it never did
func (s *Store) LastSeen(accountID string, path string) (*Seen, error) {
	var seen Seen
	var at int64
	err := s.db.QueryRow(`SELECT seen_at, seen_token, text FROM seen WHERE account_id = ? AND path = ?`, accountID, path).
		Scan(&at, &seen.Token, &seen.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen.Time = time.UnixMilli(at).UTC()
	return &seen, nil
}

// MoveSeen changes the paths of a moved item and, for a section, everything below it
func (s *Store) MoveSeen(oldPath string, newPath string) error {
	rows, err := s.db.Query(`SELECT DISTINCT path FROM seen WHERE path = ? OR path LIKE ? ESCAPE '\'`, oldPath, likePrefix(oldPath))
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			_ = rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	_ = rows.Close()
	for _, p := range paths {
		moved, ok := wiki.MovePath(p, oldPath, newPath)
		if !ok {
			continue
		}
		// what was seen at the new path before is older news: the moved item wins
		if _, err = s.db.Exec(`DELETE FROM seen WHERE path = ? AND account_id IN (SELECT account_id FROM seen WHERE path = ?)`, moved, p); err != nil {
			return err
		}
		if _, err = s.db.Exec(`UPDATE seen SET path = ? WHERE path = ?`, moved, p); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSeen forgets a deleted item and everything below it
func (s *Store) DeleteSeen(path string) error {
	_, err := s.db.Exec(`DELETE FROM seen WHERE path = ? OR path LIKE ? ESCAPE '\'`, path, likePrefix(path))
	return err
}

// likePrefix is a LIKE pattern for the paths below a section
func likePrefix(path string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(strings.TrimSuffix(path, "/")) + "/%"
}

// EditAuthorsSince returns the authors of the edits of a document after a time, in the order
// they first edited, following the document back through moves
func (s *Store) EditAuthorsSince(path string, since time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT kind, path, old_path, authors FROM feed WHERE time > ? ORDER BY time DESC, id DESC`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	paths := map[string]bool{path: true}
	var newestFirst []string
	for rows.Next() {
		var kind, p, oldPath, authorsJSON string
		if err = rows.Scan(&kind, &p, &oldPath, &authorsJSON); err != nil {
			return nil, err
		}
		if !paths[p] {
			continue
		}
		switch kind {
		case KindMoved:
			paths[oldPath] = true
		case KindEdited:
			var authors []string
			if json.Unmarshal([]byte(authorsJSON), &authors) == nil {
				// reversed below: within an entry the authors are in order already
				for i := len(authors) - 1; i >= 0; i-- {
					newestFirst = append(newestFirst, authors[i])
				}
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(newestFirst)
	out := []string{}
	for _, a := range newestFirst {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}
