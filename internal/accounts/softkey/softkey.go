// Package softkey is a software WebAuthn authenticator for tests: it makes
// an ES256 passkey and answers the panel's ceremonies the way a browser and
// a platform authenticator would (attestation "none"), so the unit tests and
// the e2e suite can sign in with a passkey without a browser. It is test
// tooling; the panel itself never imports it.
package softkey

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

// The authenticator data flags (WebAuthn §6.1).
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagBE = 0x08 // backup eligible
	flagBS = 0x10 // backed up
	flagAT = 0x40 // attested credential data follows
)

var b64 = base64.RawURLEncoding

// Key is one passkey: its key pair, its credential id, the account's user
// handle and the relying party it was made for, once Create has run.
type Key struct {
	Private      *ecdsa.PrivateKey
	CredentialID []byte
	UserHandle   []byte
	RPID         string
	// Counter is the signature counter; Get increments it unless
	// NoCounter (a synced passkey keeps 0).
	Counter   uint32
	NoCounter bool
	// Verify sets the user-verified flag (a PIN or biometrics).
	Verify bool
	// Synced sets the backup flags, as a synced passkey has them.
	Synced bool
}

// New returns a fresh key that verifies its user.
func New() *Key {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		panic(err)
	}
	return &Key{Private: priv, CredentialID: id, Verify: true}
}

// creationOptions is the part of the panel's creation options a key reads.
type creationOptions struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"publicKey"`
}

// requestOptions is the part of the panel's request options a key reads.
type requestOptions struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RPID      string `json:"rpId"`
	} `json:"publicKey"`
}

func (k *Key) flags(attested bool) byte {
	f := byte(flagUP)
	if k.Verify {
		f |= flagUV
	}
	if k.Synced {
		f |= flagBE | flagBS
	}
	if attested {
		f |= flagAT
	}
	return f
}

func clientData(kind, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

func (k *Key) authData(rpID string, attested bool) ([]byte, error) {
	rpHash := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, rpHash[:]...)
	out = append(out, k.flags(attested))
	out = binary.BigEndian.AppendUint32(out, k.Counter)
	if !attested {
		return out, nil
	}
	out = append(out, make([]byte, 16)...) // AAGUID: none
	out = binary.BigEndian.AppendUint16(out, uint16(len(k.CredentialID)))
	out = append(out, k.CredentialID...)
	pub, err := k.Private.PublicKey.Bytes() // 0x04 || x || y
	if err != nil {
		return nil, err
	}
	cose, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: pub[1:33], -3: pub[33:65]})
	if err != nil {
		return nil, err
	}
	return append(out, cose...), nil
}

// Create answers creation options (the JSON the panel's begin route
// returns) from origin, and returns the PublicKeyCredential as JSON.
func (k *Key) Create(options []byte, origin string) ([]byte, error) {
	var o creationOptions
	if err := json.Unmarshal(options, &o); err != nil {
		return nil, err
	}
	if o.PublicKey.Challenge == "" || o.PublicKey.RP.ID == "" {
		return nil, errors.New("softkey: not creation options")
	}
	handle, err := b64.DecodeString(o.PublicKey.User.ID)
	if err != nil {
		return nil, fmt.Errorf("softkey: user id: %w", err)
	}
	k.UserHandle, k.RPID = handle, o.PublicKey.RP.ID
	ad, err := k.authData(k.RPID, true)
	if err != nil {
		return nil, err
	}
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.CredentialID), "rawId": b64.EncodeToString(k.CredentialID), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData("webauthn.create", o.PublicKey.Challenge, origin)),
			"attestationObject": b64.EncodeToString(att),
			"transports":        []string{"internal", "hybrid"},
		},
	})
}

// Get answers request options from origin with an assertion, as JSON. It
// signs for the relying party the options name, as a browser would; Sign
// lets a test sign for another one.
func (k *Key) Get(options []byte, origin string) ([]byte, error) {
	var o requestOptions
	if err := json.Unmarshal(options, &o); err != nil {
		return nil, err
	}
	if o.PublicKey.Challenge == "" {
		return nil, errors.New("softkey: not request options")
	}
	return k.Sign(o.PublicKey.Challenge, o.PublicKey.RPID, origin)
}

// Sign makes an assertion of challenge for rpID from origin.
func (k *Key) Sign(challenge, rpID, origin string) ([]byte, error) {
	if !k.NoCounter {
		k.Counter++
	}
	ad, err := k.authData(rpID, false)
	if err != nil {
		return nil, err
	}
	cd := clientData("webauthn.get", challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.Private, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.CredentialID), "rawId": b64.EncodeToString(k.CredentialID), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(cd),
			"authenticatorData": b64.EncodeToString(ad),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(k.UserHandle),
		},
	})
}

// Challenge reads the challenge out of options, for a test that answers
// them with another key or twice.
func Challenge(options []byte) string {
	var o requestOptions
	_ = json.Unmarshal(options, &o)
	return o.PublicKey.Challenge
}
