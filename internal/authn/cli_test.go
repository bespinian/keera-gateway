package authn

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The two sides of the handshake compute the challenge independently - the
// command line to publish it, the gateway to check the verifier that redeems the
// code - so they have to agree, and it has to be the S256 the rest of the flow
// uses rather than anything resembling it.
func TestTheProofKeyIsTheSameOnBothSides(t *testing.T) {
	verifier, err := NewCLIVerifier()
	if err != nil {
		t.Fatalf("minting a verifier: %v", err)
	}
	challenge := CLIChallenge(verifier)
	if CLIChallenge(verifier) != challenge {
		t.Error("the same verifier produced two challenges")
	}
	// 32 bytes, base64url, unpadded - the shape the control plane checks for
	// before it sends anybody to a browser.
	if len(challenge) != 43 {
		t.Errorf("challenge = %q (%d characters), want 43", challenge, len(challenge))
	}
	if _, err := base64.RawURLEncoding.DecodeString(challenge); err != nil {
		t.Errorf("challenge = %q, want raw base64url: %v", challenge, err)
	}

	other, err := NewCLIVerifier()
	if err != nil {
		t.Fatalf("minting a verifier: %v", err)
	}
	if CLIChallenge(other) == challenge {
		t.Error("two verifiers produced one challenge")
	}
}

// A credential that leaks into a log, a bug report or a repository should be
// recognisable as one at a glance - and as this one rather than as an API key,
// which admits its holder to something else entirely.
func TestACommandLineTokenSaysWhatItIs(t *testing.T) {
	token, hash, err := NewCLIToken()
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}
	if !strings.HasPrefix(token, CLITokenPrefix) {
		t.Errorf("token = %q, want it to carry %q", token, CLITokenPrefix)
	}
	if len(hash) != 32 {
		t.Errorf("hash = %d bytes, want the 32 of a SHA-256", len(hash))
	}
	if string(HashCLIToken(token)) != string(hash) {
		t.Error("the stored hash is not the one a presented token resolves to")
	}

	second, _, err := NewCLIToken()
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}
	if second == token {
		t.Error("two sign-ins minted one token")
	}
}

// The code is short-lived and single-use, but it still crosses a URL, so it is
// stored the way every other credential here is: as a hash, never as itself.
func TestAOneTimeCodeIsStoredAsAHash(t *testing.T) {
	code, hash, err := NewCLICode()
	if err != nil {
		t.Fatalf("minting a code: %v", err)
	}
	if string(HashCLICode(code)) != string(hash) {
		t.Error("the stored hash is not the one a presented code resolves to")
	}
	if strings.Contains(string(hash), code) {
		t.Error("the code itself is in what is stored for it")
	}
}
