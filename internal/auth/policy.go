package auth

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Password rules of NIST SP 800-63B, see docs/auth.md section 3
const (
	PasswordMinLength = 10
	PasswordMaxLength = 256
)

// Reasons of a WeakPasswordError
const (
	WeakTooShort         = "too-short"
	WeakTooLong          = "too-long"
	WeakCommon           = "common"
	WeakRepetitive       = "repetitive"
	WeakContainsUsername = "contains-username"
)

// WeakPasswordError is returned for passwords that break the rules
type WeakPasswordError struct {
	Reason string
}

func (e *WeakPasswordError) Error() string {
	switch e.Reason {
	case WeakTooShort:
		return "the password must have at least 10 characters"
	case WeakTooLong:
		return "the password must have at most 256 characters"
	case WeakCommon:
		return "this password is too common, choose another one"
	case WeakRepetitive:
		return "the password is a repeated character or a simple sequence"
	case WeakContainsUsername:
		return "the password must not contain the username or the name of the application"
	}
	return "the password is too weak"
}

// common passwords with 10 or more characters (NFKC, lower case), one per line, sorted.
// Built from SecLists (MIT, see common-passwords.LICENSE): xato-net-10-million-passwords-1000000,
// Pwdb_top-1000000 and 100k-most-used-passwords-NCSC.
//
//go:embed common-passwords.txt.gz
var commonPasswordsGz []byte

var commonPasswords = sync.OnceValue(func() []string {
	r, err := gzip.NewReader(bytes.NewReader(commonPasswordsGz))
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	list := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !sort.StringsAreSorted(list) {
		sort.Strings(list)
	}
	return list
})

func isCommonPassword(normalized string) bool {
	list := commonPasswords()
	p := strings.ToLower(normalized)
	i := sort.SearchStrings(list, p)
	return i < len(list) && list[i] == p
}

var appWords = regexp.MustCompile(`(?i)mkdocs`)

// CheckPassword applies the password rules. username may be empty.
func CheckPassword(password, username string) error {
	p := normalizePassword(password)
	n := utf8.RuneCountInString(p)
	if n < PasswordMinLength {
		return &WeakPasswordError{Reason: WeakTooShort}
	}
	if n > PasswordMaxLength {
		return &WeakPasswordError{Reason: WeakTooLong}
	}
	lower := strings.ToLower(p)
	if (username != "" && strings.Contains(lower, strings.ToLower(normalizePassword(username)))) || appWords.MatchString(p) {
		return &WeakPasswordError{Reason: WeakContainsUsername}
	}
	if isRepetitive(lower) {
		return &WeakPasswordError{Reason: WeakRepetitive}
	}
	if isCommonPassword(p) {
		return &WeakPasswordError{Reason: WeakCommon}
	}
	return nil
}

// isRepetitive reports whether a password is one repeated character, a repeated short
// pattern, or a run of consecutive characters (ascending or descending), ignoring spaces
func isRepetitive(p string) bool {
	runes := []rune(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, p))
	if len(runes) < 2 {
		return true
	}
	// a pattern of up to 3 characters repeated, like "abcabcabca" or "ababababab"
	for period := 1; period <= 3 && period < len(runes); period++ {
		repeated := true
		for i := period; i < len(runes); i++ {
			if runes[i] != runes[i-period] {
				repeated = false
				break
			}
		}
		if repeated {
			return true
		}
	}
	for _, step := range []rune{1, -1} {
		sequence := true
		for i := 1; i < len(runes); i++ {
			if runes[i]-runes[i-1] != step && !(step == 1 && runes[i-1] == '9' && runes[i] == '0') {
				sequence = false
				break
			}
		}
		if sequence {
			return true
		}
	}
	return false
}
