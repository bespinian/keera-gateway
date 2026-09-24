// Package secret encrypts the credentials Keera Gateway has to read back.
//
// Keys Keera issues are stored only as hashes. A hosted provider's API key has
// to be sent on every request, so it is the one reversible secret in the
// database, sealed with a key that lives only in the process environment. A
// database dump then holds ciphertext, not a working credential.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Box seals and opens credentials. A nil Box means no encryption key is
// configured: it reports Enabled false and refuses to seal.
type Box struct {
	aead cipher.AEAD
}

// minKeyLen is a floor: hashing does not make a short key harder to guess.
const minKeyLen = 16

// New builds a Box from the configured key. An empty key is not an error: the
// feature is off, and New returns a nil Box.
func New(key string) (*Box, error) {
	if key == "" {
		return nil, nil
	}
	if len(key) < minKeyLen {
		return nil, errors.New("the encryption key is too short to be one; generate one with: openssl rand -hex 32")
	}
	// Domain separation: the same value used elsewhere yields a different key.
	sum := sha256.Sum256([]byte("keera-model-credential-v1|" + key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Enabled reports whether credentials can be stored. It is safe on a nil Box.
func (b *Box) Enabled() bool { return b != nil }

// ErrDisabled is returned when a deployment has no encryption key configured.
var ErrDisabled = errors.New("secret: no encryption key is configured")

// Seal encrypts a credential for one model. The alias is authenticated, so a
// ciphertext copied to another model's row fails to open.
func (b *Box) Seal(alias, plaintext string) ([]byte, error) {
	if !b.Enabled() {
		return nil, ErrDisabled
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(alias)), nil
}

// Open decrypts what Seal produced for the same model.
func (b *Box) Open(alias string, sealed []byte) (string, error) {
	if !b.Enabled() {
		return "", ErrDisabled
	}
	if len(sealed) < b.aead.NonceSize() {
		return "", errors.New("secret: ciphertext is truncated")
	}
	nonce, body := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	out, err := b.aead.Open(nil, nonce, body, []byte(alias))
	if err != nil {
		// The key or the row changed. Either way the fix is to set the
		// credential again.
		return "", errors.New("secret: cannot decrypt with the configured key")
	}
	return string(out), nil
}
