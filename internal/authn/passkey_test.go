package authn_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/passkeytest"
)

const origin = "https://keera.example.ch"

func relyingParty(t *testing.T) *authn.RelyingParty {
	t.Helper()
	rp, err := authn.NewRelyingParty(origin + "/keera")
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

func TestTheRelyingPartyComesFromThePublicURL(t *testing.T) {
	rp := relyingParty(t)
	if rp.ID() != "keera.example.ch" {
		t.Errorf("got %s, want keera.example.ch", rp.ID())
	}
	local, err := authn.NewRelyingParty("http://localhost:8080")
	if err != nil {
		t.Fatalf("http on localhost is a secure context: %v", err)
	}
	if local.ID() != "localhost" {
		t.Errorf("got %s, want localhost", local.ID())
	}
	// Browsers refuse each of these, so the gateway refuses to start with them.
	for _, bad := range []string{"", "http://keera.example.ch", "https://127.0.0.1",
		"http://127.0.0.1:8080", "https://[::1]:8443"} {
		if _, err := authn.NewRelyingParty(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestEachAlgorithmRegistersAndSignsIn(t *testing.T) {
	rp := relyingParty(t)
	for name, a := range map[string]*passkeytest.Authenticator{
		"ES256": passkeytest.New(origin, rp.ID()),
		"EdDSA": passkeytest.NewEd25519(origin, rp.ID()),
		"RS256": passkeytest.NewRSA(origin, rp.ID()),
	} {
		t.Run(name, func(t *testing.T) {
			cred, err := rp.VerifyRegistration("reg-challenge", a.Register("reg-challenge"))
			if err != nil {
				t.Fatalf("registration: %v", err)
			}
			if _, err := rp.VerifyAssertion("sign-in", a.Sign("sign-in"), cred); err != nil {
				t.Fatalf("sign-in: %v", err)
			}
		})
	}
}

func TestARegistrationIsRefused(t *testing.T) {
	rp := relyingParty(t)
	tests := []struct {
		name string
		mut  func(a *passkeytest.Authenticator, r *authn.Registration)
		want string
	}{
		{"for another challenge", func(a *passkeytest.Authenticator, r *authn.Registration) {
			*r = a.Register("another")
		}, "challenge"},
		// A phishing site cannot relay a ceremony: the browser writes its own
		// origin into the client data.
		{"from another origin", func(a *passkeytest.Authenticator, r *authn.Registration) {
			a.Origin = "https://keera-example.ch"
			*r = a.Register("c")
		}, "ran on"},
		{"for another site", func(a *passkeytest.Authenticator, r *authn.Registration) {
			a.RPID = "example.ch"
			*r = a.Register("c")
		}, "another site"},
		{"without user verification", func(a *passkeytest.Authenticator, r *authn.Registration) {
			a.Flags = 0x01
			*r = a.Register("c")
		}, "PIN"},
		{"with an id the authenticator did not attest", func(_ *passkeytest.Authenticator, r *authn.Registration) {
			r.ID = "AAAA"
		}, "does not match"},
		{"with a key that is not the algorithm's", func(_ *passkeytest.Authenticator, r *authn.Registration) {
			r.Algorithm = authn.AlgRS256
		}, "does not go with"},
		{"without a public key", func(_ *passkeytest.Authenticator, r *authn.Registration) {
			r.PublicKey = ""
		}, "public key"},
		{"as a sign-in", func(a *passkeytest.Authenticator, r *authn.Registration) {
			r.ClientDataJSON = a.Sign("c").ClientDataJSON
		}, "webauthn.get"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := passkeytest.New(origin, rp.ID())
			r := a.Register("c")
			tc.mut(a, &r)
			_, err := rp.VerifyRegistration("c", r)
			if !errors.Is(err, authn.ErrPasskey) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one about %q", err, tc.want)
			}
		})
	}
}

func TestASignInIsRefused(t *testing.T) {
	rp := relyingParty(t)
	tests := []struct {
		name string
		mut  func(a *passkeytest.Authenticator, cred *authn.Credential) authn.Assertion
		want string
	}{
		{"for another challenge", func(a *passkeytest.Authenticator, _ *authn.Credential) authn.Assertion {
			return a.Sign("another")
		}, "challenge"},
		{"from another origin", func(a *passkeytest.Authenticator, _ *authn.Credential) authn.Assertion {
			a.Origin = "https://evil.example"
			return a.Sign("c")
		}, "ran on"},
		{"without user verification", func(a *passkeytest.Authenticator, _ *authn.Credential) authn.Assertion {
			a.Flags = 0x01
			return a.Sign("c")
		}, "PIN"},
		{"signed by another key", func(_ *passkeytest.Authenticator, _ *authn.Credential) authn.Assertion {
			other := passkeytest.New(origin, rp.ID())
			return other.Sign("c")
		}, "signature does not match"},
		{"with altered authenticator data", func(a *passkeytest.Authenticator, _ *authn.Credential) authn.Assertion {
			as := a.Sign("c")
			b, _ := base64.RawURLEncoding.DecodeString(as.AuthenticatorData)
			b[len(b)-1] ^= 1
			as.AuthenticatorData = base64.RawURLEncoding.EncodeToString(b)
			return as
		}, "signature does not match"},
		// A counter that does not move means two copies of one key.
		{"with a counter that went back", func(a *passkeytest.Authenticator, cred *authn.Credential) authn.Assertion {
			a.Count = 5
			cred.SignCount = 9
			return a.Sign("c")
		}, "counter"},
		{"as a registration", func(a *passkeytest.Authenticator, _ *authn.Credential) authn.Assertion {
			as := a.Sign("c")
			as.ClientDataJSON = a.Register("c").ClientDataJSON
			return as
		}, "webauthn.create"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := passkeytest.New(origin, rp.ID())
			cred, err := rp.VerifyRegistration("r", a.Register("r"))
			if err != nil {
				t.Fatal(err)
			}
			as := tc.mut(a, &cred)
			_, err = rp.VerifyAssertion("c", as, cred)
			if !errors.Is(err, authn.ErrPasskey) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one about %q", err, tc.want)
			}
		})
	}
}

func TestTheCounterMovesForward(t *testing.T) {
	rp := relyingParty(t)
	a := passkeytest.New(origin, rp.ID())
	a.Count = 1
	cred, err := rp.VerifyRegistration("r", a.Register("r"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rp.VerifyAssertion("c", a.Sign("c"), cred)
	if err != nil || got != 2 {
		t.Fatalf("counter = %d, %v; want 2", got, err)
	}
}

func TestPasskeyAccountsAreMarked(t *testing.T) {
	ext := authn.PasskeyExternalID("user_1")
	if !authn.IsPasskeyAccount(ext) {
		t.Errorf("%s is not recognised", ext)
	}
	// The provider part is reserved, so no directory can issue this subject.
	if !strings.HasPrefix(ext, authn.ReservedProvider+":") {
		t.Errorf("%s is not under the reserved provider", ext)
	}
	for _, other := range []string{"", "google:123", "keera:operator-key"} {
		if authn.IsPasskeyAccount(other) {
			t.Errorf("%q counted as a passkey account", other)
		}
	}
}

func TestTheOptionsAskForAPasskey(t *testing.T) {
	rp := relyingParty(t)
	o := rp.CreationOptions("c", "user_1", "ada@example.ch", [][]byte{{1, 2}})
	sel := o.AuthenticatorSelection
	if sel.ResidentKey != "required" || sel.UserVerification != "required" || o.Attestation != "none" {
		t.Errorf("selection = %+v, attestation %q", sel, o.Attestation)
	}
	if o.RP.ID != rp.ID() || len(o.ExcludeCredentials) != 1 || o.ExcludeCredentials[0].ID != "AQI" {
		t.Errorf("options = %+v", o)
	}
	if r := rp.RequestOptions("c"); r.RPID != rp.ID() || r.UserVerification != "required" {
		t.Errorf("request options = %+v", r)
	}
}
