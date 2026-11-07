package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// recordSize is the record size of the aes128gcm content coding. The payloads are tiny,
	// so one record always holds them.
	recordSize = 4096
	// p256PointSize is the size of an uncompressed P-256 public key, the key id of the header
	p256PointSize = 65
	// authSecretSize is the size of the authentication secret of a subscription
	authSecretSize = 16
	// vapidLifetime is how long a VAPID token is valid, at most 24 hours (RFC 8292)
	vapidLifetime = 12 * time.Hour
)

// ErrInvalidKeys is returned for subscription keys that are not a P-256 point and a 16 byte secret
var ErrInvalidKeys = errors.New("p256dh must be an uncompressed P-256 point and auth 16 bytes, both base64url")

// DecodeKeys decodes and checks the keys of a subscription, base64url with or without padding
func DecodeKeys(p256dh string, auth string) ([]byte, []byte, error) {
	pub, err1 := decodeBase64URL(p256dh)
	secret, err2 := decodeBase64URL(auth)
	if err1 != nil || err2 != nil || len(secret) != authSecretSize {
		return nil, nil, ErrInvalidKeys
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return nil, nil, ErrInvalidKeys
	}
	return pub, secret, nil
}

func decodeBase64URL(s string) ([]byte, error) {
	for len(s) > 0 && s[len(s)-1] == '=' {
		s = s[:len(s)-1]
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// encrypt encrypts a payload for a subscription with the aes128gcm content coding of RFC 8188
// and the key derivation of RFC 8291
func encrypt(payload []byte, uaPublic []byte, authSecret []byte) ([]byte, error) {
	asPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptWith(payload, uaPublic, authSecret, asPrivate, salt)
}

func encryptWith(payload []byte, uaPublic []byte, authSecret []byte, asPrivate *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, ErrInvalidKeys
	}
	secret, err := asPrivate.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := asPrivate.PublicKey().Bytes()

	// RFC 8291 section 3.4
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	// one record: the payload and the delimiter of the last record, no padding
	if len(payload)+1+16 > recordSize {
		return nil, fmt.Errorf("the payload has %d bytes, more than one record", len(payload))
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain := append(append([]byte{}, payload...), 0x02)

	// RFC 8188 section 2.1: salt, record size, key id length, key id
	header := make([]byte, 0, 16+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, p256PointSize)
	header = append(header, asPublic...)
	return gcm.Seal(header, nonce, plain, nil), nil
}

// vapidAuthorization returns the Authorization header of RFC 8292 for a push service origin
func vapidAuthorization(key *ecdsa.PrivateKey, audience string, subject string, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": audience, "exp": now.Add(vapidLifetime).Unix(), "sub": subject})
	if err != nil {
		return "", err
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	token := signed + "." + base64.RawURLEncoding.EncodeToString(sig)
	return "vapid t=" + token + ", k=" + publicKeyString(key), nil
}

// publicKeyString returns the uncompressed public key, base64url without padding
func publicKeyString(key *ecdsa.PrivateKey) string {
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		// a P-256 key always converts
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(pub.Bytes())
}
