package store

import (
	"testing"
	"time"
)

func TestKeyState(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	tests := []struct {
		name string
		key  KeyInfo
		want string
	}{
		{"nothing set", KeyInfo{}, "active"},
		{"expiry ahead", KeyInfo{ExpiresAt: &future}, "active"},
		{"expiry behind", KeyInfo{ExpiresAt: &past}, "expired"},
		{"revoked", KeyInfo{RevokedAt: &past}, "revoked"},
		// Revocation is the one that matters: an expired key that was also
		// revoked is revoked, because that is the one somebody did on purpose.
		{"revoked and expired", KeyInfo{RevokedAt: &past, ExpiresAt: &past}, "revoked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.State(now); got != tc.want {
				t.Errorf("State = %q, want %q", got, tc.want)
			}
		})
	}
}
