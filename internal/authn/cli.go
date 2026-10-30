package authn

import (
	"encoding/base64"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
)

// The command line signs in through the browser and keeps the result in a
// file. This file holds what the CLI and the gateway must agree on.

// CLITokenPrefix makes a leaked token easy to spot. It differs from
// auth.Prefix because this token is for the control plane, not inference.
const CLITokenPrefix = "keera_cli_"

// CLICodeTTL bounds the hop from the browser to the loopback listener.
// People often leave a consent screen open.
const CLICodeTTL = 5 * time.Minute

// newSecret mints a token and its hash. Only the hash is stored, so a database
// dump holds no working credentials.
func newSecret(prefix string) (token string, hash []byte) {
	token = prefix + auth.RandomToken()
	return token, auth.Hash(token)
}

func NewCLIToken() (token string, hash []byte) { return newSecret(CLITokenPrefix) }

// NewCLICode mints the one-time code the browser carries to the loopback
// listener.
func NewCLICode() (code string, hash []byte) { return newSecret("") }

func NewSession() (token string, hash []byte, csrf string) {
	token, hash = newSecret("")
	return token, hash, auth.RandomToken()
}

// CLIChallenge derives the S256 challenge for a verifier. It makes a code
// caught by the wrong local listener worthless.
func CLIChallenge(verifier string) string { return s256(verifier) }

func NewCLIVerifier() string { return auth.RandomToken() }

func s256(verifier string) string {
	return base64.RawURLEncoding.EncodeToString(auth.Hash(verifier))
}
