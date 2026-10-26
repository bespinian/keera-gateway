package authn

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/auth"
)

// The CLI and the gateway compute the challenge separately, so they must agree
// on real S256.
func TestTheProofKeyIsTheSameOnBothSides(t *testing.T) {
	verifier := NewCLIVerifier()
	challenge := CLIChallenge(verifier)
	if CLIChallenge(verifier) != challenge {
		t.Error("the same verifier produced two challenges")
	}
	// 32 bytes, unpadded base64url, as the control plane checks.
	if len(challenge) != 43 {
		t.Errorf("challenge = %q (%d characters), want 43", challenge, len(challenge))
	}
	if _, err := base64.RawURLEncoding.DecodeString(challenge); err != nil {
		t.Errorf("challenge = %q, want raw base64url: %v", challenge, err)
	}

	other := NewCLIVerifier()
	if CLIChallenge(other) == challenge {
		t.Error("two verifiers produced one challenge")
	}
}

// A leaked token should be recognisable at a glance, and not as an API key.
func TestACommandLineTokenSaysWhatItIs(t *testing.T) {
	token, hash := NewCLIToken()
	if !strings.HasPrefix(token, CLITokenPrefix) {
		t.Errorf("token = %q, want it to carry %q", token, CLITokenPrefix)
	}
	if len(hash) != 32 {
		t.Errorf("hash = %d bytes, want the 32 of a SHA-256", len(hash))
	}
	if string(auth.Hash(token)) != string(hash) {
		t.Error("the stored hash is not the one a presented token resolves to")
	}

	second, _ := NewCLIToken()
	if second == token {
		t.Error("two sign-ins minted one token")
	}
}

// The code crosses a URL, so it is stored as a hash like every credential.
func TestAOneTimeCodeIsStoredAsAHash(t *testing.T) {
	code, hash := NewCLICode()
	if string(auth.Hash(code)) != string(hash) {
		t.Error("the stored hash is not the one a presented code resolves to")
	}
	if strings.Contains(string(hash), code) {
		t.Error("the code itself is in what is stored for it")
	}
}
