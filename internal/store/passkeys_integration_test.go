package store

import (
	"errors"
	"testing"
	"time"
)

func TestAPasskeyIsStoredFoundAndCounted(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	user, err := st.AddUser(ctx, "user_1", f.orgID, "ada@example.ch", "keera:passkey:user_1", "member")
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.AddPasskey(ctx, Passkey{
		ID: "passkey_1", UserID: user.ID, Name: "laptop",
		CredentialID: []byte{1, 2, 3}, PublicKey: []byte{9}, Algorithm: -7, SignCount: 4,
	})
	if err != nil {
		t.Fatalf("AddPasskey: %v", err)
	}
	if _, err := st.AddPasskey(ctx, Passkey{
		ID: "passkey_2", UserID: user.ID, CredentialID: []byte{1, 2, 3}, PublicKey: []byte{9},
	}); !errors.Is(err, ErrPasskeyTaken) {
		t.Errorf("a second registration of one credential gave %v, want ErrPasskeyTaken", err)
	}

	got, err := st.PasskeyByCredentialID(ctx, []byte{1, 2, 3})
	if err != nil || got.ID != p.ID || got.SignCount != 4 || got.Algorithm != -7 {
		t.Fatalf("PasskeyByCredentialID = %+v, %v", got, err)
	}
	// The counter only moves forward, also when two sign-ins race.
	if err := st.UsePasskey(ctx, p.ID, 4); !errors.Is(err, ErrNotFound) {
		t.Errorf("a counter that did not move gave %v, want ErrNotFound", err)
	}
	if err := st.UsePasskey(ctx, p.ID, 5); err != nil {
		t.Errorf("UsePasskey: %v", err)
	}
	got, _ = st.PasskeyByID(ctx, p.ID)
	if got.SignCount != 5 || got.LastUsedAt == nil {
		t.Errorf("after a sign-in: %+v", got)
	}

	list, err := st.ListPasskeys(ctx, user.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPasskeys = %v, %v", list, err)
	}
	if err := st.DeletePasskey(ctx, p.ID, true); !errors.Is(err, ErrLastPasskey) {
		t.Errorf("removing the only passkey gave %v, want ErrLastPasskey", err)
	}
	if err := st.DeletePasskey(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePasskey(ctx, p.ID, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it twice gave %v, want ErrNotFound", err)
	}
	if _, err := st.PasskeyByID(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted passkey gave %v", err)
	}
}

func TestOnlyTheNewestSetUpLinkWorks(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	user, err := st.AddUser(ctx, "user_1", f.orgID, "ada@example.ch", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	first, second := []byte("link-1"), []byte("link-2")
	if err := st.CreatePasskeyLink(ctx, first, user.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePasskeyLink(ctx, second, user.ID, later); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PasskeyLinkUser(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Errorf("the replaced link gave %v, want ErrNotFound", err)
	}
	// Reading does not use the link up; taking does.
	for range 2 {
		if u, err := st.PasskeyLinkUser(ctx, second); err != nil || u.ID != user.ID {
			t.Fatalf("PasskeyLinkUser = %+v, %v", u, err)
		}
	}
	if id, err := st.TakePasskeyLink(ctx, second); err != nil || id != user.ID {
		t.Fatalf("TakePasskeyLink = %q, %v", id, err)
	}
	if _, err := st.TakePasskeyLink(ctx, second); !errors.Is(err, ErrNotFound) {
		t.Errorf("a used link gave %v, want ErrNotFound", err)
	}

	expired := []byte("link-3")
	if err := st.CreatePasskeyLink(ctx, expired, user.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PasskeyLinkUser(ctx, expired); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired link gave %v, want ErrNotFound", err)
	}
}

func TestADirectoryAccountCannotBecomeAPasskeyAccount(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	fresh, _ := st.AddUser(ctx, "user_1", f.orgID, "new@example.ch", "", "member")
	linked, _ := st.AddUser(ctx, "user_2", f.orgID, "sso@example.ch", "google:123", "member")

	if err := st.MakePasskeyAccount(ctx, fresh.ID, "keera:passkey:user_1"); err != nil {
		t.Errorf("a person who never signed in: %v", err)
	}
	// Twice is fine: issuing a second link does it again.
	if err := st.MakePasskeyAccount(ctx, fresh.ID, "keera:passkey:user_1"); err != nil {
		t.Errorf("again: %v", err)
	}
	if err := st.MakePasskeyAccount(ctx, linked.ID, "keera:passkey:user_2"); !errors.Is(err, ErrDirectoryAccount) {
		t.Errorf("a directory account gave %v, want ErrDirectoryAccount", err)
	}
	if err := st.MakePasskeyAccount(ctx, "user_9", "keera:passkey:user_9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("nobody gave %v, want ErrNotFound", err)
	}

	// A first single sign-on with the same address does not take the row over.
	if _, err := st.LinkUser(ctx, "user_3", Link{
		OrgID: f.orgID, Email: "new@example.ch", ExternalID: "google:456", Role: "member",
	}); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("single sign-on adopting a passkey account gave %v, want ErrEmailTaken", err)
	}
}

func TestDisablingRemovesPasskeysAndLinks(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	user, _ := st.AddUser(ctx, "user_1", f.orgID, "ada@example.ch", "keera:passkey:user_1", "member")
	if _, err := st.AddPasskey(ctx, Passkey{
		ID: "passkey_1", UserID: user.ID, CredentialID: []byte{1}, PublicKey: []byte{9},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePasskeyLink(ctx, []byte("link"), user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePasskeyChallenge(ctx, "c1", user.ID, "challenge",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DisableUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListPasskeys(ctx, user.ID); len(list) != 0 {
		t.Errorf("%d passkeys left after disabling", len(list))
	}
	if _, err := st.TakePasskeyLink(ctx, []byte("link")); !errors.Is(err, ErrNotFound) {
		t.Errorf("the link survived disabling: %v", err)
	}
	if _, _, err := st.TakePasskeyChallenge(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the challenge survived disabling: %v", err)
	}
}

func TestARegistrationChallengeWorksOnce(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	user, _ := st.AddUser(ctx, "user_1", f.orgID, "ada@example.ch", "", "member")
	if err := st.CreatePasskeyChallenge(ctx, "c1", user.ID, "challenge",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if id, ch, err := st.TakePasskeyChallenge(ctx, "c1"); err != nil || id != user.ID || ch != "challenge" {
		t.Fatalf("TakePasskeyChallenge = %q, %q, %v", id, ch, err)
	}
	if _, _, err := st.TakePasskeyChallenge(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a used challenge gave %v", err)
	}
}
