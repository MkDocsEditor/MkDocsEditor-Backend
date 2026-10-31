package auth

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ErrInvalidUsername is returned for usernames that break the rules
var ErrInvalidUsername = errors.New("usernames have 1 to 32 characters (letters, digits, '.', '_', '-') and start with a letter or digit")

// ErrInvalidDisplayName is returned for display names that break the rules
var ErrInvalidDisplayName = errors.New("display names have 1 to 64 characters and no control characters")

// ErrInvalidColor is returned for colours that are not #rrggbb
var ErrInvalidColor = errors.New("colours look like #1971c2")

// ErrInvalidEmail is returned for emails that cannot be used as a git author email
var ErrInvalidEmail = errors.New("the email must look like name@example.com")

// ValidateUsername checks the rules for new usernames
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return ErrInvalidUsername
	}
	return nil
}

// cleanDisplayName trims a display name and checks it. An empty name becomes fallback.
func cleanDisplayName(name, fallback string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fallback
	}
	if utf8.RuneCountInString(name) > 64 || !utf8.ValidString(name) {
		return "", ErrInvalidDisplayName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidDisplayName
		}
	}
	return name, nil
}

// passwordHash returns the stored hash of an account ("" without a password)
func (s *Service) passwordHash(db querier, accountID string) (string, error) {
	var h string
	err := db.QueryRow("SELECT password_hash FROM accounts WHERE id = ?", accountID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// usernameTaken reports whether a username exists, also as a former username, except for
// the given account
func usernameTaken(db querier, username, exceptAccountID string) (bool, error) {
	key := usernameKey(username)
	var n int
	err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM accounts WHERE username_key = ? AND id != ?) +
		(SELECT COUNT(*) FROM username_aliases WHERE username_key = ? AND account_id != ?)`, key, exceptAccountID, key, exceptAccountID).Scan(&n)
	return n > 0, err
}

// UsernameAvailable checks a new username: format and uniqueness
func (s *Service) UsernameAvailable(username string) error {
	if err := ValidateUsername(username); err != nil {
		return err
	}
	taken, err := usernameTaken(s.db, username, "")
	if err != nil {
		return err
	}
	if taken {
		return ErrUsernameTaken
	}
	return nil
}
