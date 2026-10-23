// Package auth mints and verifies the API keys clients present to the gateway.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// Prefix starts every key Keera Gateway issues, so a leaked key is easy to spot
// in a log or a secret scanner.
const Prefix = "keera_sk_"

// prefixLen is how much of a key is stored in the clear, so the panel can show
// it and a log line can be matched to a key without the secret.
const prefixLen = len(Prefix) + 6

const scheme = "bearer"

var errMalformed = errors.New("auth: malformed key")

// Generate returns a new key with the values stored for it: its SHA-256 and its
// display prefix. The key itself is never stored.
func Generate() (key string, hash []byte, prefix string) {
	key = Prefix + RandomToken()
	return key, Hash(key), key[:prefixLen]
}

// RandomToken returns 32 random bytes, URL-safe base64 encoded. rand.Read
// never fails: it crashes the program instead of returning weak bytes.
func RandomToken() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// WellFormed reports whether key has the shape Generate gives every key. A
// token that does not cannot be a key, so it is refused without a query.
func WellFormed(key string) bool {
	rest, ok := strings.CutPrefix(key, Prefix)
	return ok && Encoded32(rest)
}

var encoded32Len = base64.RawURLEncoding.EncodedLen(32)

// Encoded32 reports whether s has the shape RandomToken gives: 32 bytes in
// unpadded base64url.
func Encoded32(s string) bool {
	if len(s) != encoded32Len {
		return false
	}
	for _, c := range s {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// Hash returns the value stored for a secret token: an API key, a session, a
// command-line token or code, or a passkey link.
//
// SHA-256 rather than a password hash: each token is random, so there is no
// dictionary to defend against, and a slow hash on every request would invite
// denial of service.
func Hash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// FromHeader extracts a bearer token from an Authorization header value. It
// also accepts a bare token, because some editor plugins send one.
func FromHeader(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", errMalformed
	}
	// Strip the scheme only when something follows it, so a header of
	// "Bearer " is refused rather than read as the token "Bearer".
	if len(h) >= len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
		rest := h[len(scheme):]
		if rest == "" {
			return "", errMalformed
		}
		if rest[0] == ' ' || rest[0] == '\t' {
			h = strings.TrimSpace(rest)
		}
	}
	return h, nil
}
