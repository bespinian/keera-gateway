package authn

import (
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
)

// The command line signs in through the browser, as the panel does, and keeps
// the result in a file instead of a cookie. This file holds what the command
// line and the gateway must agree on: the token format, the handshake's
// lifetimes and the proof key.

// CLITokenPrefix starts every command-line token, so a leaked one is easy to
// spot by eye or by a secret scanner. It differs from auth.Prefix because this
// token signs a person in to the control plane, not to the inference API.
const CLITokenPrefix = "keera_cli_"

const (
	// CLITokenTTL is how long a signed-in terminal stays signed in. Signing in
	// again costs a browser window, so it is a month rather than a browser
	// session's twelve hours; a lost laptop still stops working by itself. The
	// directory is asked again meanwhile, so leaving it ends the token sooner.
	CLITokenTTL = 30 * 24 * time.Hour
	// CLICodeTTL bounds the hop from the browser to the loopback listener. It
	// is minutes because people often leave a consent screen open for a while.
	CLICodeTTL = 5 * time.Minute
)

// NewCLIToken mints a token and the hash stored for it. Only the hash is
// stored, so a database dump holds no working credentials.
func NewCLIToken() (token string, hash []byte) {
	token = CLITokenPrefix + auth.RandomToken()
	return token, HashCLIToken(token)
}

// HashCLIToken returns the value stored in cli_tokens.id for token.
//
// SHA-256 rather than a password hash: the input is 256 random bits, so there
// is no dictionary to defend against, and this runs on every command.
func HashCLIToken(token string) []byte { return sum256(token) }

// NewCLICode mints the one-time code the browser carries back to the loopback
// listener, and the hash stored for it.
func NewCLICode() (code string, hash []byte) {
	code = auth.RandomToken()
	return code, HashCLICode(code)
}

// HashCLICode returns the value stored in cli_codes.id for code.
func HashCLICode(code string) []byte { return sum256(code) }

// NewSession mints a panel session's cookie value, the hash stored for it,
// and its CSRF token.
func NewSession() (token string, hash []byte, csrf string) {
	token = auth.RandomToken()
	return token, HashSession(token), auth.RandomToken()
}

// HashSession returns the value stored for a session's cookie value.
func HashSession(token string) []byte { return sum256(token) }

// CLIChallenge derives the S256 challenge for a verifier.
//
// The command line publishes it when it starts a sign-in, and the gateway
// checks the verifier against it when the code is redeemed. That is what makes
// a code caught by the wrong local listener worthless.
func CLIChallenge(verifier string) string { return s256(verifier) }

// NewCLIVerifier mints the verifier a command line keeps to itself.
func NewCLIVerifier() string { return auth.RandomToken() }

func sum256(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// s256 is the PKCE S256 challenge for a verifier.
func s256(verifier string) string {
	return base64.RawURLEncoding.EncodeToString(sum256(verifier))
}
