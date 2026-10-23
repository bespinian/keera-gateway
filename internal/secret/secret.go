// Package secret encrypts the credentials Keera Gateway has to read back:
// hosted provider API keys, MCP server credentials and sign-in refresh
// tokens. Keys Keera issues are only hashed. The encryption key lives only in
// the process environment, so a database dump holds ciphertext, not a working
// credential.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

type Box struct {
	aead cipher.AEAD
	// previous are the keys before this one, which still open what they
	// sealed while it is re-sealed with this one. See Reseal.
	previous []cipher.AEAD
}

// minKeyLen is a floor: hashing does not make a short key harder to guess.
const minKeyLen = 16

// New is a box that seals with key, and opens what key or any of previous
// sealed.
func New(key string, previous ...string) (*Box, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	b := &Box{aead: aead}
	for _, k := range previous {
		old, err := newAEAD(k)
		if err != nil {
			return nil, fmt.Errorf("the previous key: %w", err)
		}
		b.previous = append(b.previous, old)
	}
	return b, nil
}

func newAEAD(key string) (cipher.AEAD, error) {
	if len(key) < minKeyLen {
		return nil, errors.New("the encryption key is too short to be one; generate one with: openssl rand -hex 32")
	}
	// Domain separation: the same value used elsewhere yields a different key.
	// The label predates MCP credentials and refresh tokens; changing it would
	// make every stored credential unreadable.
	sum := sha256.Sum256([]byte("keera-model-credential-v1|" + key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Rotating reports whether the box has previous keys, so stored credentials
// may still need re-sealing.
func (b *Box) Rotating() bool { return len(b.previous) > 0 }

// Seal encrypts a credential for the row name names. The name is
// authenticated, so a ciphertext copied to another row fails to open.
func (b *Box) Seal(name, plaintext string) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce) // never fails; see crypto/rand.Read
	return b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(name))
}

// Open decrypts what Seal produced for the same name, with this key or a
// previous one.
func (b *Box) Open(name string, sealed []byte) (string, error) {
	out, _, err := b.open(name, sealed)
	return out, err
}

// Reseal seals again with the current key what a previous key sealed. It
// returns nil when sealed is already under the current key.
func (b *Box) Reseal(name string, sealed []byte) ([]byte, error) {
	out, current, err := b.open(name, sealed)
	if err != nil || current {
		return nil, err
	}
	return b.Seal(name, out), nil
}

// open is Open, and whether the current key was the one that opened it.
func (b *Box) open(name string, sealed []byte) (string, bool, error) {
	// Every key has the same nonce size: they are all AES-GCM.
	if len(sealed) < b.aead.NonceSize() {
		return "", false, errors.New("secret: ciphertext is truncated")
	}
	nonce, body := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	for i, aead := range append([]cipher.AEAD{b.aead}, b.previous...) {
		if out, err := aead.Open(nil, nonce, body, []byte(name)); err == nil {
			return string(out), i == 0, nil
		}
	}
	// The key or the row changed. Either way the fix is to set the
	// credential again.
	return "", false, errors.New("secret: cannot decrypt with the configured key")
}
