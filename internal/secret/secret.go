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
)

type Box struct {
	aead cipher.AEAD
}

// minKeyLen is a floor: hashing does not make a short key harder to guess.
const minKeyLen = 16

func New(key string) (*Box, error) {
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
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts a credential for the row name names. The name is
// authenticated, so a ciphertext copied to another row fails to open.
func (b *Box) Seal(name, plaintext string) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce) // never fails; see crypto/rand.Read
	return b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(name))
}

// Open decrypts what Seal produced for the same name.
func (b *Box) Open(name string, sealed []byte) (string, error) {
	if len(sealed) < b.aead.NonceSize() {
		return "", errors.New("secret: ciphertext is truncated")
	}
	nonce, body := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	out, err := b.aead.Open(nil, nonce, body, []byte(name))
	if err != nil {
		// The key or the row changed. Either way the fix is to set the
		// credential again.
		return "", errors.New("secret: cannot decrypt with the configured key")
	}
	return string(out), nil
}
