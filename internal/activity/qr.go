package activity

import (
	"database/sql"
	"log"
	"time"
)

// QRCode is what the store remembers of a QR code key, see docs/qr.md
type QRCode struct {
	Key string
	// LastPath is the document that had the key last
	LastPath string
	Label    *string
	// LastScannedAt is the zero time before the first scan
	LastScannedAt time.Time
}

// SaveQRCodes remembers the current page and label of keys. It runs in the background, in
// order with the other writes; Wait waits for it.
func (s *Store) SaveQRCodes(codes []QRCode) {
	if len(codes) == 0 {
		return
	}
	s.enqueue(func() {
		if err := s.saveQRCodes(codes); err != nil {
			log.Printf("Unable to save the QR codes: %v", err)
		}
	})
}

func (s *Store) saveQRCodes(codes []QRCode) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range codes {
		if _, err = tx.Exec(`INSERT INTO qr_codes (key, last_path, label) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET last_path = excluded.last_path, label = excluded.label`,
			c.Key, c.LastPath, c.Label); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QRCodes returns the remembered keys by key
func (s *Store) QRCodes() (map[string]QRCode, error) {
	rows, err := s.db.Query(`SELECT key, last_path, label, last_scanned_at FROM qr_codes`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]QRCode{}
	for rows.Next() {
		c, err := scanQRCode(rows)
		if err != nil {
			return nil, err
		}
		out[c.Key] = c
	}
	return out, rows.Err()
}

// QRCode returns a remembered key, nil if the store does not know it
func (s *Store) QRCode(key string) (*QRCode, error) {
	c, err := scanQRCode(s.db.QueryRow(`SELECT key, last_path, label, last_scanned_at FROM qr_codes WHERE key = ?`, key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// RecordQRScan sets the scan time of a known key to now and returns it. It returns the zero
// time for an unknown key.
func (s *Store) RecordQRScan(key string) (time.Time, error) {
	now := s.now()
	res, err := s.db.Exec(`UPDATE qr_codes SET last_scanned_at = ? WHERE key = ?`, now.UnixMilli(), key)
	if err != nil {
		return time.Time{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return time.Time{}, err
	}
	return time.UnixMilli(now.UnixMilli()).UTC(), nil
}

func scanQRCode(row interface{ Scan(...any) error }) (QRCode, error) {
	var c QRCode
	var label sql.NullString
	var scanned int64
	if err := row.Scan(&c.Key, &c.LastPath, &label, &scanned); err != nil {
		return c, err
	}
	if label.Valid {
		c.Label = &label.String
	}
	if scanned != 0 {
		c.LastScannedAt = time.UnixMilli(scanned).UTC()
	}
	return c, nil
}
