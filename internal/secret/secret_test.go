package secret

import (
	"bytes"
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

	sealed := b.Seal("keera-frontier", credential)
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
	again := b.Seal("keera-frontier", credential)
	if bytes.Equal(sealed, again) {
		t.Error("sealing twice produced identical ciphertext")
	}
}

func TestOpenRefusesWhatItShould(t *testing.T) {
	b, err := New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed := b.Seal("keera-frontier", "sk-ant-credential")

	if _, err := b.Open("keera-speed", sealed); err == nil {
		t.Error("a ciphertext opened under an alias it was not sealed for")
	}

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := b.Open("keera-frontier", tampered); err == nil {
		t.Error("a modified ciphertext opened")
	}

	// After rotating KEERA_SECRET_KEY this must fail loudly, not return rubbish.
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

func TestNewRejectsAKeyThatIsNotOne(t *testing.T) {
	_, err := New("short")
	if err == nil {
		t.Fatal("New accepted a guessable key")
	}
	if !strings.Contains(err.Error(), "openssl") {
		t.Errorf("error = %q; it should say how to generate one", err)
	}
}

func TestARotatedKeyStillOpensWhatTheOldOneSealed(t *testing.T) {
	const newKey = "a-new-key-long-enough-to-be-one"
	old, _ := New(testKey)
	sealed := old.Seal("model:org_1:frontier", "sk-old")

	b, err := New(newKey, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Rotating() {
		t.Error("a box with a previous key does not say it is rotating")
	}
	if got, err := b.Open("model:org_1:frontier", sealed); err != nil || got != "sk-old" {
		t.Fatalf("Open = %q, %v; want the old seal opened", got, err)
	}
	resealed, err := b.Reseal("model:org_1:frontier", sealed)
	if err != nil || resealed == nil {
		t.Fatalf("Reseal = %v, %v", resealed, err)
	}
	only, _ := New(newKey)
	if got, err := only.Open("model:org_1:frontier", resealed); err != nil || got != "sk-old" {
		t.Errorf("the new key alone cannot open the re-sealed value: %q, %v", got, err)
	}
	if again, err := b.Reseal("model:org_1:frontier", resealed); err != nil || again != nil {
		t.Errorf("re-sealing a current value = %v, %v; want nothing to do", again, err)
	}
	if _, err := b.Reseal("model:org_2:frontier", sealed); err == nil {
		t.Error("a value moved to another row was re-sealed")
	}
	if _, err := New(newKey, "short"); err == nil {
		t.Error("a short previous key was accepted")
	}
}
