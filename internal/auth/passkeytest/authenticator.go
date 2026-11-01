// Package passkeytest is a software authenticator for tests: it answers WebAuthn creation
// and request options like a passkey provider (attestation "none", discoverable, user
// verification) and produces the JSON a browser or Android Credential Manager sends.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

var b64 = base64.RawURLEncoding

// Authenticator holds passkeys for one relying party
type Authenticator struct {
	RPID   string
	Origin string
	AAGUID [16]byte
	// BackedUp marks the credentials as synced (BE and BS flags)
	BackedUp bool
	// SkipUV clears the user verification flag (a broken or non-verifying authenticator)
	SkipUV bool
	Keys   []*Key
}

// Key is one passkey
type Key struct {
	ID         []byte
	UserHandle []byte
	Private    *ecdsa.PrivateKey
	Counter    uint32
}

// Bitwarden is the AAGUID of the Bitwarden passkey provider
var Bitwarden = [16]byte{0xd5, 0x48, 0x82, 0x6e, 0x79, 0xb4, 0xdb, 0x40, 0xa3, 0xd8, 0x11, 0x11, 0x6f, 0x7e, 0x83, 0x49}

func (a *Authenticator) authData(withCredential *Key, counter uint32) []byte {
	rp := sha256.Sum256([]byte(a.RPID))
	flags := byte(0x01) // UP
	if !a.SkipUV {
		flags |= 0x04
	}
	if a.BackedUp {
		flags |= 0x08 | 0x10
	}
	if withCredential != nil {
		flags |= 0x40
	}
	out := append([]byte{}, rp[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, counter)
	if withCredential != nil {
		out = append(out, a.AAGUID[:]...)
		out = binary.BigEndian.AppendUint16(out, uint16(len(withCredential.ID))) //nolint:gosec // short ids
		out = append(out, withCredential.ID...)
		// the uncompressed form is 0x04, X and Y; COSE needs the raw coordinates
		pub, _ := withCredential.Private.PublicKey.Bytes()
		cose, _ := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: pub[1:33], -3: pub[33:]})
		out = append(out, cose...)
	}
	return out
}

type creationOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	ExcludeCredentials []struct {
		ID string `json:"id"`
	} `json:"excludeCredentials"`
}

func decode(options any, v any) error {
	data, err := json.Marshal(options)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Create answers PublicKeyCredentialCreationOptions with a new passkey and returns the
// RegistrationResponseJSON
func (a *Authenticator) Create(options any) (json.RawMessage, error) {
	var o creationOptions
	if err := decode(options, &o); err != nil {
		return nil, err
	}
	if o.RP.ID != a.RPID {
		return nil, fmt.Errorf("rp id %q, authenticator is for %q", o.RP.ID, a.RPID)
	}
	userHandle, err := b64.DecodeString(o.User.ID)
	if err != nil {
		return nil, err
	}
	for _, ex := range o.ExcludeCredentials {
		for _, k := range a.Keys {
			if ex.ID == b64.EncodeToString(k.ID) {
				return nil, errors.New("InvalidStateError: a credential of this authenticator is excluded")
			}
		}
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	k := &Key{ID: make([]byte, 16), UserHandle: userHandle, Private: priv}
	_, _ = rand.Read(k.ID)
	a.Keys = append(a.Keys, k)
	clientData, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": o.Challenge, "origin": a.Origin, "crossOrigin": false})
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(k, 0)})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.ID), "rawId": b64.EncodeToString(k.ID), "type": "public-key",
		"authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(clientData), "attestationObject": b64.EncodeToString(attestation),
			"transports": []string{"internal", "hybrid"},
		},
		"clientExtensionResults": map[string]any{},
	})
}

type requestOptions struct {
	Challenge        string `json:"challenge"`
	RPID             string `json:"rpId"`
	AllowCredentials []struct {
		ID string `json:"id"`
	} `json:"allowCredentials"`
}

// Get answers PublicKeyCredentialRequestOptions with the first matching passkey (the
// newest one for a discoverable request) and returns the AuthenticationResponseJSON
func (a *Authenticator) Get(options any) (json.RawMessage, error) {
	var o requestOptions
	if err := decode(options, &o); err != nil {
		return nil, err
	}
	if o.RPID != a.RPID {
		return nil, fmt.Errorf("rp id %q, authenticator is for %q", o.RPID, a.RPID)
	}
	var key *Key
	for i := len(a.Keys) - 1; i >= 0 && key == nil; i-- {
		k := a.Keys[i]
		if len(o.AllowCredentials) == 0 {
			key = k
		}
		for _, c := range o.AllowCredentials {
			if c.ID == b64.EncodeToString(k.ID) {
				key = k
			}
		}
	}
	if key == nil {
		return nil, errors.New("NotAllowedError: no matching passkey")
	}
	return a.Sign(key, o.Challenge)
}

// Sign creates an assertion of a key for a challenge (base64url)
func (a *Authenticator) Sign(key *Key, challenge string) (json.RawMessage, error) {
	key.Counter++
	clientData, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": a.Origin, "crossOrigin": false})
	authData := a.authData(nil, key.Counter)
	hash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), hash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, key.Private, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(key.ID), "rawId": b64.EncodeToString(key.ID), "type": "public-key",
		"authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(clientData), "authenticatorData": b64.EncodeToString(authData),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(key.UserHandle),
		},
		"clientExtensionResults": map[string]any{},
	})
}

// Challenge returns the challenge (base64url) of creation or request options
func Challenge(options any) string {
	var o struct {
		Challenge string `json:"challenge"`
	}
	_ = decode(options, &o)
	return o.Challenge
}
