package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	// SecretKeyFileName is the default file of the server key in the state directory
	SecretKeyFileName = "auth.key"
	// SecretKeyEnv holds the server key (base64) and takes precedence over the file
	SecretKeyEnv = "MKDOCSREST_AUTH_SECRET_KEY" //nolint:gosec // the name of a variable, not a secret
)

// serverKeys are derived from the 32-byte server key, one key per purpose
type serverKeys struct {
	totp          []byte // AES-256-GCM key of the TOTP secrets
	recoveryCodes []byte // HMAC-SHA256 key of the recovery code hashes
}

func deriveKey(master []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("mkdocseditor/" + purpose))
	return mac.Sum(nil)
}

// loadServerKeys reads the server key from the environment or the key file, and creates
// the file (mode 0600) on the first start
func loadServerKeys(stateDir, file string) (*serverKeys, error) {
	master, err := readServerKey(stateDir, file)
	if err != nil {
		return nil, err
	}
	return &serverKeys{totp: deriveKey(master, "totp-secret"), recoveryCodes: deriveKey(master, "recovery-codes")}, nil
}

func decodeServerKey(s, source string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("the server key in %s must be 32 bytes, base64 encoded", source)
	}
	return key, nil
}

func readServerKey(stateDir, file string) ([]byte, error) {
	if v := os.Getenv(SecretKeyEnv); v != "" {
		return decodeServerKey(v, "$"+SecretKeyEnv)
	}
	if file == "" {
		file = filepath.Join(stateDir, SecretKeyFileName)
	}
	data, err := os.ReadFile(file) //nolint:gosec // path from the configuration
	if err == nil {
		return decodeServerKey(string(data), file)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("unable to read the server key: %w", err)
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path from the configuration
	if err != nil {
		return nil, fmt.Errorf("unable to create the server key: %w", err)
	}
	if _, err = f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	log.Printf("Created the server key %s; back it up together with the account database", file)
	return key, nil
}

// seal encrypts data with AES-256-GCM; the account id is authenticated, so a secret cannot
// be moved to another account
func seal(key []byte, plaintext []byte, accountID string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, []byte(accountID)), nil
}

// errSecretUnreadable means the server key changed since the secret was stored
var errSecretUnreadable = errors.New("a stored secret cannot be decrypted: the server key (auth.key) changed")

func unseal(key []byte, sealed []byte, accountID string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errSecretUnreadable
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte(accountID))
	if err != nil {
		return nil, errSecretUnreadable
	}
	return plain, nil
}
