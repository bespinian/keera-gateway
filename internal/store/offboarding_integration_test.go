package store

import (
	"errors"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestDisableUserRevokesKeysAndSessions(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	user, err := st.UpsertUser(ctx, "user_1", f.orgID, "ada@example.ch", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.UpsertUser(ctx, "user_2", f.orgID, "bob@example.ch", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	for i, owner := range []string{user.ID, user.ID, other.ID} {
		if _, err := st.CreateKey(ctx, KeyInfo{
			ID: "key_u" + string(rune('a'+i)), OrgID: f.orgID, UserID: owner,
			Alias: "laptop", Prefix: "p",
		}, []byte("hash-"+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(time.Hour)
	if err := st.CreateSession(ctx, []byte("cookie"), user.ID, "csrf", later, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCLIToken(ctx, []byte("cli"), user.ID, "laptop", later); err != nil {
		t.Fatal(err)
	}

	revoked, err := st.DisableUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 2 {
		t.Errorf("revoked %d keys, want the 2 attributed to them", revoked)
	}
	if _, err := st.LookupKey(ctx, []byte("hash-a")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("their key = %v, want revoked", err)
	}
	if _, err := st.LookupKey(ctx, []byte("hash-c")); err != nil {
		t.Errorf("somebody else's key stopped working: %v", err)
	}
	if _, err := st.LookupSession(ctx, []byte("cookie")); !errors.Is(err, ErrNotFound) {
		t.Errorf("their session = %v, want gone", err)
	}
	if _, err := st.LookupCLIToken(ctx, []byte("cli")); !errors.Is(err, ErrNotFound) {
		t.Errorf("their command-line token = %v, want gone", err)
	}
	got, err := st.UserByID(ctx, user.ID)
	if err != nil || !got.Disabled() {
		t.Fatalf("UserByID = %+v, %v; want disabled", got, err)
	}
	first := *got.DisabledAt

	// A key issued to them later, by a bypass or a race, still does not work.
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_late", OrgID: f.orgID, UserID: user.ID, Alias: "late", Prefix: "p",
	}, []byte("hash-late")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupKey(ctx, []byte("hash-late")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("a key of a disabled person = %v, want refused as revoked", err)
	}
	// Disabling again revokes it, and keeps the first date.
	if revoked, err := st.DisableUser(ctx, user.ID); err != nil || revoked != 1 {
		t.Errorf("second DisableUser = %d, %v; want 1 revoked", revoked, err)
	}
	if got, _ := st.UserByID(ctx, user.ID); !got.DisabledAt.Equal(first) {
		t.Errorf("disabled_at moved from %s to %s", first, got.DisabledAt)
	}

	if err := st.EnableUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.UserByID(ctx, user.ID); got.Disabled() {
		t.Error("EnableUser left them disabled")
	}
	if _, err := st.LookupKey(ctx, []byte("hash-a")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("enabling brought an old key back: %v", err)
	}
	if _, err := st.DisableUser(ctx, "user_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabling nobody = %v, want ErrNotFound", err)
	}
}

func TestLiveSandboxByKey(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	sb := newSandbox(t, st, ctx, f, "fix-login")

	got, err := st.LiveSandboxByKey(ctx, f.hash)
	if err != nil || got.ID != sb.ID {
		t.Fatalf("LiveSandboxByKey = %s, %v; want %s", got.ID, err, sb.ID)
	}
	if err := st.SetSandboxGitCredential(ctx, sb.ID, "42"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Sandbox(ctx, sb.ID); got.GitCredentialID != "42" {
		t.Errorf("git credential id = %q, want 42", got.GitCredentialID)
	}

	if _, err := st.LiveSandboxByKey(ctx, []byte("some other key")); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown key = %v, want ErrNotFound", err)
	}
	if err := st.RevokeKey(ctx, f.keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LiveSandboxByKey(ctx, f.hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked key = %v, want ErrNotFound", err)
	}
}
