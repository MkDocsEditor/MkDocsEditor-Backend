package activity

import (
	"database/sql"
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

// Pins returns the family pins and the personal pins of an account, newest first
func (s *Store) Pins(accountID string) (family []Pin, personal []Pin, err error) {
	rows, err := s.db.Query(`SELECT account_id, path, pinned_by, pinned_at FROM pins WHERE account_id IN (?, ?)
		ORDER BY pinned_at DESC, path`, Family, accountID)
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

// AddPin pins a document for an account, or for the family with Family. It returns false if
// the document was pinned already.
func (s *Store) AddPin(accountID string, path string, by string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO pins (account_id, path, pinned_by, pinned_at) VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING`, accountID, path, by, s.now().UnixMilli())
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
