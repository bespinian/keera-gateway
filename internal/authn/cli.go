package authn

import (
	"encoding/base64"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
)

// The command line signs in through the browser, like the panel, and keeps the
// result in a file instead of a cookie. This file holds what the command line
// and the gateway must agree on: the token format, the lifetimes and the proof
// key.

// CLITokenPrefix starts every command-line token, so a leaked one is easy to
// spot by eye or by a secret scanner. It differs from auth.Prefix because this
// token signs a person in to the control plane, not to the inference API.
const CLITokenPrefix = "keera_cli_"

const (
	// CLITokenTTL is how long a terminal stays signed in. Signing in again
	// costs a browser window, so it is a month, not a panel session's twelve
	// hours. The directory is still asked again meanwhile, so leaving it ends
	// the token sooner.
	CLITokenTTL = 30 * 24 * time.Hour
	// CLICodeTTL bounds the hop from the browser to the loopback listener. It
	// is minutes because people often leave a consent screen open.
	CLICodeTTL = 5 * time.Minute
)

// newSecret mints a random token that starts with prefix, and the hash stored
// for it. Only the hash is stored, so a database dump holds no working
// credentials.
func newSecret(prefix string) (token string, hash []byte) {
	token = prefix + auth.RandomToken()
	return token, auth.Hash(token)
}

func NewCLIToken() (token string, hash []byte) { return newSecret(CLITokenPrefix) }

// NewCLICode mints the one-time code the browser carries back to the loopback
// listener, and the hash stored for it.
func NewCLICode() (code string, hash []byte) { return newSecret("") }

// NewSession mints a panel session's cookie value, the hash stored for it,
// and its CSRF token.
func NewSession() (token string, hash []byte, csrf string) {
	token, hash = newSecret("")
	return token, hash, auth.RandomToken()
}

// CLIChallenge derives the S256 challenge for a verifier.
//
// The command line publishes it when it starts a sign-in, and the gateway
// checks the verifier against it when the code is redeemed. That is what makes
// a code caught by the wrong local listener worthless.
func CLIChallenge(verifier string) string { return s256(verifier) }

// NewCLIVerifier mints the verifier a command line keeps to itself.
func NewCLIVerifier() string { return auth.RandomToken() }

// s256 is the PKCE S256 challenge for a verifier.
func s256(verifier string) string {
	return base64.RawURLEncoding.EncodeToString(auth.Hash(verifier))
}
