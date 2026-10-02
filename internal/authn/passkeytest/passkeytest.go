// Package passkeytest is a software authenticator, for tests of the passkey
// ceremonies. It signs what a browser and a real authenticator would.
package passkeytest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/bespinian/keera-gateway/internal/authn"
)

// Authenticator holds one passkey.
type Authenticator struct {
	// Origin is the page the ceremony claims to run on, and RPID the site the
	// authenticator data is for.
	Origin string
	RPID   string
	// Flags are the authenticator data flags. New sets user present and user
	// verified.
	Flags byte
	// Count is the signature counter. Zero stays zero, like a synced passkey;
	// anything else goes up by one per signature.
	Count uint32
	// UserHandle is what a sign-in reports as the passkey's owner.
	UserHandle []byte

	ID  []byte
	Alg int
	key crypto.Signer
}

// New makes an ES256 passkey.
func New(origin, rpID string) *Authenticator {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return WithKey(origin, rpID, key, authn.AlgES256)
}

// NewEd25519 and NewRSA make passkeys with the other two algorithms.
func NewEd25519(origin, rpID string) *Authenticator {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return WithKey(origin, rpID, key, authn.AlgEdDSA)
}

func NewRSA(origin, rpID string) *Authenticator {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return WithKey(origin, rpID, key, authn.AlgRS256)
}

// WithKey makes a passkey from a given key.
func WithKey(origin, rpID string, key crypto.Signer, alg int) *Authenticator {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &Authenticator{
		Origin: origin, RPID: rpID, Flags: 0x01 | 0x04, ID: id, Alg: alg, key: key,
	}
}

// Register answers a navigator.credentials.create with this challenge.
func (a *Authenticator) Register(challenge string) authn.Registration {
	spki, err := x509.MarshalPKIXPublicKey(a.key.Public())
	if err != nil {
		panic(err)
	}
	// The COSE key that would follow is left out: the gateway reads the key
	// from public_key instead.
	data := a.header(a.Flags | 0x40)
	data = append(data, make([]byte, 16)...)
	data = binary.BigEndian.AppendUint16(data, uint16(len(a.ID)))
	data = append(data, a.ID...)
	return authn.Registration{
		ID:                enc(a.ID),
		ClientDataJSON:    enc(a.clientData("webauthn.create", challenge)),
		AuthenticatorData: enc(data),
		PublicKey:         enc(spki),
		Algorithm:         a.Alg,
	}
}

// Sign answers a navigator.credentials.get with this challenge.
func (a *Authenticator) Sign(challenge string) authn.Assertion {
	if a.Count != 0 {
		a.Count++
	}
	data := a.header(a.Flags)
	clientData := a.clientData("webauthn.get", challenge)
	hash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, data...), hash[:]...)

	var sig []byte
	var err error
	switch a.Alg {
	case authn.AlgEdDSA:
		sig, err = a.key.Sign(rand.Reader, signed, crypto.Hash(0))
	default:
		sum := sha256.Sum256(signed)
		sig, err = a.key.Sign(rand.Reader, sum[:], crypto.SHA256)
	}
	if err != nil {
		panic(err)
	}
	return authn.Assertion{
		ID:                enc(a.ID),
		ClientDataJSON:    enc(clientData),
		AuthenticatorData: enc(data),
		Signature:         enc(sig),
		UserHandle:        enc(a.UserHandle),
	}
}

func (a *Authenticator) header(flags byte) []byte {
	rp := sha256.Sum256([]byte(a.RPID))
	data := append(rp[:], flags)
	return binary.BigEndian.AppendUint32(data, a.Count)
}

func (a *Authenticator) clientData(kind, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": kind, "challenge": challenge, "origin": a.Origin, "crossOrigin": false,
	})
	return b
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
