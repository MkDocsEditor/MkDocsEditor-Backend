package activity

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kinds of feed entries
const (
	KindEdited  = "edited"
	KindCreated = "created"
	KindMoved   = "moved"
	KindDeleted = "deleted"
)

// ErrInvalidCursor is returned for a cursor that this store did not hand out
var ErrInvalidCursor = errors.New("invalid cursor")

// Entry is one entry of the feed
type Entry struct {
	ID       int64
	Time     time.Time
	Kind     string
	ItemType string
	Path     string
	// OldPath is set for moves
	OldPath string
	// Authors are account ids or the pseudo users ":disk" and ":server", in the order they
	// first changed something
	Authors []string
	// Summary is set for edits
	Summary *Summary
}

// RecordEdit records that the server wrote a document: before is the text the file had,
// after the new one. Edits of the same document within EditSession extend the last entry,
// whose summary then compares with the text before the first of them. It never blocks.
func (s *Store) RecordEdit(path string, before string, after string, authors []string) {
	if before == after || len(authors) == 0 {
		return
	}
	at := s.now()
	authors = slices.Clone(authors)
	s.enqueue(func() {
		if err := s.recordEdit(path, before, after, authors, at); err != nil {
			log.Printf("Unable to record the edit of %s in the feed: %v", path, err)
			return
		}
		s.feedChanged()
	})
}

func (s *Store) recordEdit(path string, before string, after string, authors []string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id         int64
		base       string
		authorJSON string
	)
	err = tx.QueryRow(`SELECT id, base_text, authors FROM feed WHERE path = ? AND kind = ? AND base_text IS NOT NULL AND time >= ?
		ORDER BY time DESC, id DESC LIMIT 1`, path, KindEdited, at.Add(-EditSession).UnixMilli()).Scan(&id, &base, &authorJSON)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// a new session, older ones of this document end
		if _, err = tx.Exec(`UPDATE feed SET base_text = NULL WHERE path = ? AND base_text IS NOT NULL`, path); err != nil {
			return err
		}
		sum := Summarize(before, after)
		if _, err = tx.Exec(`INSERT INTO feed (time, kind, item_type, path, authors, lines_added, lines_removed, headings, base_text)
			VALUES (?, ?, 'document', ?, ?, ?, ?, ?, ?)`,
			at.UnixMilli(), KindEdited, path, mustJSON(authors), sum.LinesAdded, sum.LinesRemoved, mustJSON(sum.Headings), before); err != nil {
			return err
		}
	case err != nil:
		return err
	case base == after:
		// back to where the session started
		if _, err = tx.Exec(`DELETE FROM feed WHERE id = ?`, id); err != nil {
			return err
		}
	default:
		var known []string
		if err = json.Unmarshal([]byte(authorJSON), &known); err != nil {
			return err
		}
		for _, a := range authors {
			if !slices.Contains(known, a) {
				known = append(known, a)
			}
		}
		sum := Summarize(base, after)
		if _, err = tx.Exec(`UPDATE feed SET time = ?, authors = ?, lines_added = ?, lines_removed = ?, headings = ? WHERE id = ?`,
			at.UnixMilli(), mustJSON(known), sum.LinesAdded, sum.LinesRemoved, mustJSON(sum.Headings), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordItem records that an item was created, moved or deleted. oldPath is the path before a
// move, author the account that did it. Edit sessions of the items end. It never blocks.
func (s *Store) RecordItem(kind string, itemType string, path string, oldPath string, author string) {
	at := s.now()
	s.enqueue(func() {
		if err := s.recordItem(kind, itemType, path, oldPath, author, at); err != nil {
			log.Printf("Unable to record that %s was %s in the feed: %v", path, kind, err)
			return
		}
		s.feedChanged()
	})
}

func (s *Store) recordItem(kind string, itemType string, path string, oldPath string, author string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// the paths below p sort between p+"/" and p+"0", '0' follows '/'
	for _, p := range []string{path, oldPath} {
		if p == "" {
			continue
		}
		if _, err = tx.Exec(`UPDATE feed SET base_text = NULL WHERE base_text IS NOT NULL AND (path = ? OR (path >= ? AND path < ?))`,
			p, p+"/", p+"0"); err != nil {
			return err
		}
	}
	authors := []string{}
	if author != "" {
		authors = append(authors, author)
	}
	if _, err = tx.Exec(`INSERT INTO feed (time, kind, item_type, path, old_path, authors) VALUES (?, ?, ?, ?, ?, ?)`,
		at.UnixMilli(), kind, itemType, path, oldPath, mustJSON(authors)); err != nil {
		return err
	}
	return tx.Commit()
}

// Feed returns up to limit entries older than the cursor, newest first, and the cursor of the
// next page ("" if there is none). An empty cursor starts with the newest entry.
func (s *Store) Feed(cursor string, limit int) ([]Entry, string, error) {
	beforeTime, beforeID := int64(1<<62), int64(1<<62)
	if cursor != "" {
		var err error
		if beforeTime, beforeID, err = parseCursor(cursor); err != nil {
			return nil, "", err
		}
	}
	rows, err := s.db.Query(`SELECT id, time, kind, item_type, path, old_path, authors, lines_added, lines_removed, headings
		FROM feed WHERE time >= ? AND (time < ? OR (time = ? AND id < ?)) ORDER BY time DESC, id DESC LIMIT ?`,
		s.now().Add(-Retention).UnixMilli(), beforeTime, beforeTime, beforeID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	entries := []Entry{}
	for rows.Next() {
		var (
			e                     Entry
			at                    int64
			authors, headings     string
			linesAdded, linesRemd int
		)
		if err = rows.Scan(&e.ID, &at, &e.Kind, &e.ItemType, &e.Path, &e.OldPath, &authors, &linesAdded, &linesRemd, &headings); err != nil {
			return nil, "", err
		}
		e.Time = time.UnixMilli(at).UTC()
		if err = json.Unmarshal([]byte(authors), &e.Authors); err != nil {
			return nil, "", err
		}
		if e.Kind == KindEdited {
			e.Summary = &Summary{LinesAdded: linesAdded, LinesRemoved: linesRemd}
			if err = json.Unmarshal([]byte(headings), &e.Summary.Headings); err != nil {
				return nil, "", err
			}
		}
		entries = append(entries, e)
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[limit-1]
		next = formatCursor(last.Time.UnixMilli(), last.ID)
	}
	return entries, next, nil
}

func formatCursor(at int64, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d.%d", at, id)))
}

func parseCursor(cursor string) (int64, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, ErrInvalidCursor
	}
	at, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return 0, 0, ErrInvalidCursor
	}
	t, err1 := strconv.ParseInt(at, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, ErrInvalidCursor
	}
	return t, i, nil
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
