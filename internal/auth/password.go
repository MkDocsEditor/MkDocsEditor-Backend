package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/text/unicode/norm"
)

// argon2Params are the Argon2id parameters, OWASP Password Storage Cheat Sheet:
// 19 MiB memory, 2 iterations, parallelism 1
type argon2Params struct {
	memory  uint32 // KiB
	time    uint32
	threads uint8
}

var defaultArgon2 = argon2Params{memory: 19 * 1024, time: 2, threads: 1}

// currentArgon2 is the parameter set new hashes use; tests lower it
var currentArgon2 = defaultArgon2

const (
	argon2SaltLen = 16
	argon2KeyLen  = 32
	// at most this many hashes are computed at the same time (19 MiB each)
	maxConcurrentHashes = 4
)

var hashSlots = make(chan struct{}, maxConcurrentHashes)

// dummyHash is verified for unknown users, so the response time does not reveal accounts
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("dummy password for unknown users")
	if err != nil {
		panic(err)
	}
	return h
})

// normalizePassword applies the NFKC normalization of NIST SP 800-63B
func normalizePassword(password string) string {
	return norm.NFKC.String(password)
}

// HashPassword returns the Argon2id hash of a password in PHC format
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := currentArgon2
	key := computeArgon2([]byte(normalizePassword(password)), salt, p, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func computeArgon2(password, salt []byte, p argon2Params, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(password, salt, p.time, p.memory, p.threads, keyLen)
}

var errUnknownHash = errors.New("unknown password hash format")

// parseArgon2 parses a PHC string
func parseArgon2(encoded string) (p argon2Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errUnknownHash
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errUnknownHash
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, errUnknownHash
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 || p.memory > 1<<21 || p.time > 100 {
		return p, nil, nil, errUnknownHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, errUnknownHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, errUnknownHash
	}
	return p, salt, key, nil
}

// normalizeBcrypt accepts the $2y$ prefix of htpasswd and PHP, which is the same algorithm as $2b$
func normalizeBcrypt(h string) string {
	if strings.HasPrefix(h, "$2y$") {
		return "$2b$" + h[4:]
	}
	return h
}

func isBcrypt(h string) bool {
	return strings.HasPrefix(h, "$2a$") || strings.HasPrefix(h, "$2b$") || strings.HasPrefix(h, "$2y$")
}

// VerifyPassword checks a password against an Argon2id or bcrypt hash. needsRehash is true
// when the hash should be replaced by a hash with the current parameters.
func VerifyPassword(encoded, password string) (ok bool, needsRehash bool) {
	if isBcrypt(encoded) {
		// bcrypt hashes were made from the password as typed
		hashSlots <- struct{}{}
		err := bcrypt.CompareHashAndPassword([]byte(normalizeBcrypt(encoded)), []byte(password))
		<-hashSlots
		return err == nil, true
	}
	p, salt, key, err := parseArgon2(encoded)
	if err != nil {
		return false, false
	}
	got := computeArgon2([]byte(normalizePassword(password)), salt, p, uint32(len(key))) // #nosec G115 -- len(key) <= 64
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false
	}
	return true, p != currentArgon2 || len(salt) != argon2SaltLen || len(key) != argon2KeyLen
}

// verifyDummy spends the time of a password check without an account
func verifyDummy(password string) {
	VerifyPassword(dummyHash(), password)
}
