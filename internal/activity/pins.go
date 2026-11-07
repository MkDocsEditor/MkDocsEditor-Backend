package activity

import (
	"database/sql"
	"slices"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
)

// Family is the account id of the pins everyone sees
const Family = ""

// Pin is a pinned document
type Pin struct {
	// AccountID is the account whose personal pin this is, Family for a family pin
	AccountID string
	Path      string
	PinnedBy  string
	PinnedAt  time.Time
}

// PinChanges tells whose pins a move or a delete changed
type PinChanges struct {
	Family   bool
	Accounts []string
}

// Empty reports whether no pin changed
func (c PinChanges) Empty() bool {
	return !c.Family && len(c.Accounts) == 0
}

func (c *PinChanges) add(accountID string) {
	if accountID == Family {
		c.Family = true
		return
	}
	for _, a := range c.Accounts {
		if a == accountID {
			return
		}
	}
	c.Accounts = append(c.Accounts, accountID)
}

// Pins returns the family pins and the personal pins of an account, in their order
func (s *Store) Pins(accountID string) (family []Pin, personal []Pin, err error) {
	rows, err := s.db.Query(`SELECT account_id, path, pinned_by, pinned_at FROM pins WHERE account_id IN (?, ?)
		ORDER BY position, pinned_at DESC, path`, Family, accountID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	family, personal = []Pin{}, []Pin{}
	for rows.Next() {
		var (
			p  Pin
			at int64
		)
		if err = rows.Scan(&p.AccountID, &p.Path, &p.PinnedBy, &at); err != nil {
			return nil, nil, err
		}
		p.PinnedAt = time.UnixMilli(at).UTC()
		if p.AccountID == Family {
			family = append(family, p)
		} else {
			personal = append(personal, p)
		}
	}
	return family, personal, rows.Err()
}

// AddPin pins a document for an account, or for the family with Family, at the end of the
// list. It returns false if the document was pinned already.
func (s *Store) AddPin(accountID string, path string, by string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO pins (account_id, path, pinned_by, pinned_at, position)
		VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM pins WHERE account_id = ?))
		ON CONFLICT DO NOTHING`, accountID, path, by, s.now().UnixMilli(), accountID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemovePin unpins a document. It returns false if it was not pinned.
func (s *Store) RemovePin(accountID string, path string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM pins WHERE account_id = ? AND path = ?`, accountID, path)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReorderPins puts the pins of an account (or Family) at the paths first, in that order. Paths
// that are not pinned are ignored, the other pins follow in their order. It returns false if
// the order stayed the same.
func (s *Store) ReorderPins(accountID string, paths []string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT path FROM pins WHERE account_id = ? ORDER BY position, pinned_at DESC, path`, accountID)
	if err != nil {
		return false, err
	}
	var current []string
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			_ = rows.Close()
			return false, err
		}
		current = append(current, p)
	}
	_ = rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	pinned := map[string]bool{}
	for _, p := range current {
		pinned[p] = true
	}
	order := make([]string, 0, len(current))
	placed := map[string]bool{}
	for _, p := range paths {
		if pinned[p] && !placed[p] {
			placed[p] = true
			order = append(order, p)
		}
	}
	for _, p := range current {
		if !placed[p] {
			order = append(order, p)
		}
	}
	if slices.Equal(order, current) {
		return false, nil
	}
	for i, p := range order {
		if _, err = tx.Exec(`UPDATE pins SET position = ? WHERE account_id = ? AND path = ?`, i, accountID, p); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// MovePins changes the paths of the pins of a moved item: the item itself and, for a section,
// everything below it. A pin that already exists at the new path stays.
func (s *Store) MovePins(oldPath string, newPath string) (PinChanges, error) {
	return s.changePins(oldPath, func(tx *sql.Tx, p Pin) error {
		moved, _ := wiki.MovePath(p.Path, oldPath, newPath)
		_, err := tx.Exec(`UPDATE OR IGNORE pins SET path = ? WHERE account_id = ? AND path = ?`, moved, p.AccountID, p.Path)
		if err == nil {
			// the pin at the new path won
			_, err = tx.Exec(`DELETE FROM pins WHERE account_id = ? AND path = ?`, p.AccountID, p.Path)
		}
		return err
	})
}

// DeletePins removes the pins of a deleted item and everything below it
func (s *Store) DeletePins(path string) (PinChanges, error) {
	return s.changePins(path, func(tx *sql.Tx, p Pin) error {
		_, err := tx.Exec(`DELETE FROM pins WHERE account_id = ? AND path = ?`, p.AccountID, p.Path)
		return err
	})
}

// changePins runs change for each pin at path or below it, in one transaction
func (s *Store) changePins(path string, change func(tx *sql.Tx, p Pin) error) (PinChanges, error) {
	var changes PinChanges
	tx, err := s.db.Begin()
	if err != nil {
		return changes, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT account_id, path FROM pins WHERE path = ? OR (path >= ? AND path < ?)`, path, path+"/", path+"0")
	if err != nil {
		return changes, err
	}
	var pins []Pin
	for rows.Next() {
		var p Pin
		if err = rows.Scan(&p.AccountID, &p.Path); err != nil {
			_ = rows.Close()
			return changes, err
		}
		if wiki.IsInside(p.Path, path) {
			pins = append(pins, p)
		}
	}
	_ = rows.Close()
	if err = rows.Err(); err != nil {
		return changes, err
	}
	for _, p := range pins {
		if err = change(tx, p); err != nil {
			return PinChanges{}, err
		}
		changes.add(p.AccountID)
	}
	return changes, tx.Commit()
}
