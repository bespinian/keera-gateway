package authn

import (
	"strings"
	"testing"
)

func TestGUID(t *testing.T) {
	// An objectGUID as AD stores it, and as Windows shows it.
	raw := []byte{
		0x78, 0x56, 0x34, 0x12, 0xbc, 0x9a, 0xf0, 0xde,
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
	}
	const text = "12345678-9abc-def0-0123-456789abcdef"
	if got := guidString(raw); got != text {
		t.Errorf("guidString = %s, want %s", got, text)
	}
	back, err := guidBytes(strings.ToUpper(text))
	if err != nil || string(back) != string(raw) {
		t.Errorf("guidBytes = %x, %v", back, err)
	}
	if _, err := guidBytes("not-a-guid"); err == nil {
		t.Error("guidBytes accepted nonsense")
	}
}

func TestNormalDNs(t *testing.T) {
	got := normalDNs([]string{"CN=Keera Admins, OU=Groups,DC=corp", " ", "not a dn"})
	want := []string{"cn=keera admins,ou=groups,dc=corp", "not a dn"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("normalDNs = %q, want %q", got, want)
	}
}
