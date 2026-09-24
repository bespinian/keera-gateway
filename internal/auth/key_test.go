package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerateProducesDistinctRecognisableKeys(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		key, hash, prefix, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !strings.HasPrefix(key, Prefix) {
			t.Fatalf("key %q lacks the prefix that makes a leak recognisable", key)
		}
		if seen[key] {
			t.Fatal("Generate returned a key it had already returned")
		}
		seen[key] = true

		if !bytes.Equal(hash, Hash(key)) {
			t.Error("the stored hash does not verify against the key")
		}
		if !strings.HasPrefix(key, prefix) || prefix == key {
			t.Errorf("prefix %q must be a proper, short prefix of the key", prefix)
		}
		if strings.Contains(prefix, key[len(prefix):]) {
			t.Error("the display prefix leaks part of the secret")
		}
	}
}

func TestFromHeader(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "a bearer token", in: "Bearer keera_sk_abc", want: "keera_sk_abc"},
		{name: "lowercase scheme", in: "bearer keera_sk_abc", want: "keera_sk_abc"},
		// Some editor plugins send the key with no scheme at all.
		{name: "a bare token", in: "keera_sk_abc", want: "keera_sk_abc"},
		{name: "surrounding space", in: "  Bearer   keera_sk_abc  ", want: "keera_sk_abc"},
		{name: "empty", in: "", wantErr: true},
		{name: "a scheme with nothing after it", in: "Bearer ", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromHeader(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("FromHeader(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromHeader(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("FromHeader(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
