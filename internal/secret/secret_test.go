package secret

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const testKey = "a-key-long-enough-to-be-one"

func TestRoundTrip(t *testing.T) {
	b, err := New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const credential = "sk-ant-a-real-looking-credential"

	sealed, err := b.Seal("keera-frontier", credential)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("sk-ant")) {
		t.Error("the credential is recognisable in its own ciphertext")
	}
	got, err := b.Open("keera-frontier", sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != credential {
		t.Errorf("Open = %q, want the credential back", got)
	}

	// Two seals of the same credential must not be the same bytes, or the
	// models table tells a reader which models share a key.
	again, err := b.Seal("keera-frontier", credential)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(sealed, again) {
		t.Error("sealing twice produced identical ciphertext")
	}
}

func TestOpenRefusesWhatItShould(t *testing.T) {
	b, err := New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := b.Seal("keera-frontier", "sk-ant-credential")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Moved to another alias's row.
	if _, err := b.Open("keera-speed", sealed); err == nil {
		t.Error("a ciphertext opened under an alias it was not sealed for")
	}

	// Tampered with in place.
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := b.Open("keera-frontier", tampered); err == nil {
		t.Error("a modified ciphertext opened")
	}

	// Sealed under a different key - the case an operator hits after rotating
	// KEERA_SECRET_KEY, which must fail loudly rather than return rubbish.
	other, err := New("a-different-key-of-its-own")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := other.Open("keera-frontier", sealed); err == nil {
		t.Error("a ciphertext opened under the wrong key")
	}

	if _, err := b.Open("keera-frontier", []byte("short")); err == nil {
		t.Error("a truncated ciphertext opened")
	}
}

func TestNoKeyMeansTheFeatureIsOffRatherThanInsecure(t *testing.T) {
	b, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Enabled() {
		t.Fatal("a deployment with no key reports the feature as available")
	}
	if _, err := b.Seal("keera-frontier", "sk-ant-credential"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Seal error = %v, want ErrDisabled - never a plaintext fallback", err)
	}
	if _, err := b.Open("keera-frontier", []byte("anything")); !errors.Is(err, ErrDisabled) {
		t.Errorf("Open error = %v, want ErrDisabled", err)
	}
}

func TestNewRejectsAKeyThatIsNotOne(t *testing.T) {
	_, err := New("short")
	if err == nil {
		t.Fatal("New accepted a guessable key")
	}
	if !strings.Contains(err.Error(), "openssl") {
		t.Errorf("error = %q; it should say how to generate one", err)
	}
}
