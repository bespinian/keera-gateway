package store

import (
	"errors"
	"testing"
	"time"
)

func TestSessionsExpireAndAreTouchedAtMostOnceAMinute(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC()

	user, err := st.UpsertUser(ctx, "user_1", f.orgID, "dev@example.ch", "sub-1", "admin")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	live := []byte("session-hash-000000000000000001!")
	dead := []byte("session-hash-000000000000000002!")
	if err := st.CreateSession(ctx, live, user.ID, "csrf-1", now.Add(time.Hour), "Firefox", "10.0.0.1"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.CreateSession(ctx, dead, user.ID, "csrf-2", now.Add(-time.Minute), "Firefox", "10.0.0.1"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	su, err := st.LookupSession(ctx, live)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if su.User.Email != "dev@example.ch" || su.User.Role != "admin" || su.Session.CSRF != "csrf-1" {
		t.Errorf("SessionUser = %+v, want the person and their CSRF token", su)
	}
	// An expired session is reported as missing, so no caller has to remember
	// to check.
	if _, err := st.LookupSession(ctx, dead); err != ErrNotFound {
		t.Errorf("an expired session gave %v, want ErrNotFound", err)
	}

	// Touch is throttled in SQL: it is the only write on the control plane's
	// read path, so it must not fire on every request.
	if _, err := st.pool.Exec(ctx,
		"UPDATE sessions SET last_seen_at = now() - interval '2 minutes' WHERE id = $1",
		live); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchSession(ctx, live); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	var seen time.Time
	if err := st.pool.QueryRow(ctx,
		"SELECT last_seen_at FROM sessions WHERE id = $1", live).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if time.Since(seen) > time.Minute {
		t.Errorf("last_seen_at = %s, want it moved forward", seen)
	}
	if err := st.TouchSession(ctx, live); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	var again time.Time
	if err := st.pool.QueryRow(ctx,
		"SELECT last_seen_at FROM sessions WHERE id = $1", live).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if !again.Equal(seen) {
		t.Errorf("last_seen_at moved twice within a minute: %s then %s", seen, again)
	}

	// PurgeExpired keeps the table bounded; nothing depends on it for
	// correctness, which is why the expired row was already invisible above.
	if err := st.PurgeExpired(ctx); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	var left int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("%d sessions after the purge, want only the live one", left)
	}

	// A role change has to take effect, which means signing the person out.
	if err := st.DeleteUserSessions(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}
	if _, err := st.LookupSession(ctx, live); err != ErrNotFound {
		t.Errorf("the session survived DeleteUserSessions: %v", err)
	}
}

func TestLoginFlowIsSingleUseAndExpires(t *testing.T) {
	st, ctx := db(t)

	if err := st.CreateLoginFlow(ctx, LoginFlow{
		State: "state-1", Verifier: "verifier-1", Nonce: "nonce-1", RedirectTo: "/teams",
	}, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateLoginFlow: %v", err)
	}

	flow, err := st.TakeLoginFlow(ctx, "state-1")
	if err != nil {
		t.Fatalf("TakeLoginFlow: %v", err)
	}
	if flow.Verifier != "verifier-1" || flow.Nonce != "nonce-1" || flow.RedirectTo != "/teams" {
		t.Errorf("flow = %+v, want the PKCE verifier, nonce and path", flow)
	}
	// A replayed callback finds nothing, so a stolen code cannot be used twice.
	if _, err := st.TakeLoginFlow(ctx, "state-1"); err != ErrNotFound {
		t.Errorf("the second take gave %v, want ErrNotFound", err)
	}

	if err := st.CreateLoginFlow(ctx, LoginFlow{State: "state-2", Verifier: "v", Nonce: "n"},
		time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("CreateLoginFlow: %v", err)
	}
	if _, err := st.TakeLoginFlow(ctx, "state-2"); err != ErrNotFound {
		t.Errorf("an expired flow gave %v, want ErrNotFound", err)
	}
	if err := st.PurgeExpired(ctx); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	var left int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM login_flows").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d flows survived the purge, want 0", left)
	}
}

func TestLinkUserAdoptsAPersonInsteadOfDuplicatingThem(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	// An organisation that pre-creates its people, before anybody has signed
	// in and so before any subject claim exists.
	pre, err := st.UpsertUser(ctx, "user_pre", f.orgID, "dev@example.ch", "", "member")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	// The first sign-in matches on email and adopts the row, writing the
	// subject back so it only ever happens once.
	linked, err := st.LinkUser(ctx, "user_new", Link{
		OrgID: f.orgID, Email: "dev@example.ch", ExternalID: "sub-1", Role: "admin"})
	if err != nil {
		t.Fatalf("LinkUser: %v", err)
	}
	if linked.ID != pre.ID {
		t.Errorf("LinkUser minted %s, want the pre-created %s", linked.ID, pre.ID)
	}
	if linked.ExternalID != "sub-1" {
		t.Errorf("ExternalID = %q, want the subject written back", linked.ExternalID)
	}

	// The second sign-in matches on the subject, and follows a changed address.
	again, err := st.LinkUser(ctx, "user_newer", Link{
		OrgID: f.orgID, Email: "renamed@example.ch", ExternalID: "sub-1", Role: "admin"})
	if err != nil {
		t.Fatalf("LinkUser: %v", err)
	}
	if again.ID != pre.ID {
		t.Errorf("LinkUser minted %s, want %s", again.ID, pre.ID)
	}
	if again.Email != "renamed@example.ch" {
		t.Errorf("Email = %q, want the address from this sign-in", again.Email)
	}

	users, err := st.ListUsers(ctx, f.orgID)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("%d users, want one person: %+v", len(users), users)
	}

	byExternal, err := st.UserByExternalID(ctx, "sub-1")
	if err != nil || byExternal.ID != pre.ID {
		t.Errorf("UserByExternalID = %+v, %v", byExternal, err)
	}
	if err := st.SetUserRole(ctx, pre.ID, "operator"); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	if u, _ := st.UserByID(ctx, pre.ID); u.Role != "operator" {
		t.Errorf("Role = %q, want operator", u.Role)
	}
	if err := st.SetUserRole(ctx, "nobody", "member"); err != ErrNotFound {
		t.Errorf("SetUserRole on a missing user gave %v, want ErrNotFound", err)
	}
}

func TestFirstOrgOnAnEmptyDeployment(t *testing.T) {
	st, ctx := db(t)

	// Nothing to join yet: this is the only case where an operator-key sign-in
	// should stand an organisation up.
	if _, err := st.FirstOrg(ctx); err != ErrNotFound {
		t.Errorf("FirstOrg with no organisations gave %v, want ErrNotFound", err)
	}
}

func TestOrgLookupsForASignIn(t *testing.T) {
	st, ctx := db(t)

	// Exactly one organisation: a dedicated or on-premises deployment, where a
	// first sign-in needs no domain mapping at all.
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org, err := st.OnlyOrg(ctx); err != nil || org.ID != "org_1" {
		t.Errorf("OnlyOrg = %+v, %v", org, err)
	}

	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// Two, so there is no "only" one and the guess must not be made.
	if _, err := st.OnlyOrg(ctx); err != ErrNotFound {
		t.Errorf("OnlyOrg with two organisations gave %v, want ErrNotFound", err)
	}
	// The operator key still has somewhere to belong once a second tenant exists.
	// Answering "not found" here is what used to have the sign-in create a
	// fresh organisation on every attempt.
	if org, err := st.FirstOrg(ctx); err != nil || org.ID != "org_1" {
		t.Errorf("FirstOrg = %+v, %v; want the organisation the deployment started with", org, err)
	}

	if err := st.SetOrgEmailDomain(ctx, "org_2", "AnotherBank.CH"); err != nil {
		t.Fatalf("SetOrgEmailDomain: %v", err)
	}
	// Matched case-insensitively: nobody types their own domain consistently.
	if org, err := st.OrgByEmailDomain(ctx, "anotherbank.ch"); err != nil || org.ID != "org_2" {
		t.Errorf("OrgByEmailDomain = %+v, %v", org, err)
	}
	if _, err := st.OrgByEmailDomain(ctx, "nobody.ch"); err != ErrNotFound {
		t.Errorf("an unmapped domain gave %v, want ErrNotFound", err)
	}
	// Clearing it is how a tenant stops adopting sign-ins by domain.
	if err := st.SetOrgEmailDomain(ctx, "org_2", ""); err != nil {
		t.Fatalf("SetOrgEmailDomain: %v", err)
	}
	if _, err := st.OrgByEmailDomain(ctx, "anotherbank.ch"); err != ErrNotFound {
		t.Errorf("the domain still matches after being cleared: %v", err)
	}
	if err := st.SetOrgEmailDomain(ctx, "nobody", "x.ch"); err != ErrNotFound {
		t.Errorf("setting a domain on a missing org gave %v, want ErrNotFound", err)
	}
	// One domain places sign-ins in one tenant, so the second claim on it is
	// refused - and told apart from a driver failure, because the operator who
	// typed it against the wrong organisation is the one who has to hear which
	// it is.
	if err := st.SetOrgEmailDomain(ctx, "org_1", "shared.ch"); err != nil {
		t.Fatalf("SetOrgEmailDomain: %v", err)
	}
	if err := st.SetOrgEmailDomain(ctx, "org_2", "Shared.CH"); !errors.Is(err, ErrDomainTaken) {
		t.Errorf("a domain another organisation holds gave %v, want ErrDomainTaken", err)
	}
	// Setting the one it already has is not a collision with itself.
	if err := st.SetOrgEmailDomain(ctx, "org_1", "shared.ch"); err != nil {
		t.Errorf("re-setting an organisation's own domain gave %v", err)
	}
}

// The one that matters once a deployment has more than one identity provider.
//
// Matching on email is what adopts a row an administrator typed in. Left
// unqualified it would also let a second directory take over a person the first
// directory already vouched for - and with them, whatever role that person
// holds. Anyone who can get an address past the weaker of two directories would
// inherit the account it names in the stronger one.
func TestLinkUserRefusesToTakeOverAnotherProvidersIdentity(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	// Ada signs in through the first directory and is an administrator there.
	ada, err := st.LinkUser(ctx, "user_ada", Link{
		OrgID: f.orgID, Email: "ada@example.ch", ExternalID: "google:1", Role: "admin"})
	if err != nil {
		t.Fatalf("LinkUser: %v", err)
	}

	// Somebody arrives through the second directory carrying the same address.
	_, err = st.LinkUser(ctx, "user_other", Link{
		OrgID: f.orgID, Email: "ada@example.ch", ExternalID: "entra:2", Role: "member"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("LinkUser = %v, want ErrEmailTaken", err)
	}
	if u, err := st.UserByExternalID(ctx, "google:1"); err != nil || u.Role != "admin" {
		t.Errorf("Ada's row = %+v, %v; want it untouched and still an administrator", u, err)
	}
	if u, err := st.UserByExternalID(ctx, "entra:2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("UserByExternalID = %+v, %v; want the refused identity not to exist", u, err)
	}
	users, err := st.ListUsers(ctx, f.orgID)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("%d users, want the one that was already there: %+v", len(users), users)
	}

	// Moving an organisation from one provider to another is the case this is
	// in the way of, so it can be asked for explicitly.
	moved, err := st.LinkUser(ctx, "user_other", Link{
		OrgID: f.orgID, Email: "ada@example.ch", ExternalID: "entra:2",
		Role: "member", AdoptByEmail: true})
	if err != nil {
		t.Fatalf("LinkUser with AdoptByEmail: %v", err)
	}
	if moved.ID != ada.ID {
		t.Errorf("LinkUser minted %s, want the existing %s", moved.ID, ada.ID)
	}
	if moved.ExternalID != "entra:2" {
		t.Errorf("ExternalID = %q, want the new provider's", moved.ExternalID)
	}
	// The role is the row's, not this sign-in's: syncRole is what moves it, and
	// it reads the directory the person actually came through.
	if moved.Role != "admin" {
		t.Errorf("Role = %q, want the role the row already held", moved.Role)
	}
}

// A login in flight remembers which provider it was started against, because
// the callback has to complete the exchange with that provider's client and
// every provider shares one redirect URI.
func TestLoginFlowRemembersItsProvider(t *testing.T) {
	st, ctx := db(t)

	if err := st.CreateLoginFlow(ctx, LoginFlow{
		State: "state-p", Verifier: "v", Nonce: "n",
		Provider: "entra", RedirectTo: "/usage",
	}, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("CreateLoginFlow: %v", err)
	}
	got, err := st.TakeLoginFlow(ctx, "state-p")
	if err != nil {
		t.Fatalf("TakeLoginFlow: %v", err)
	}
	if got.Provider != "entra" {
		t.Errorf("Provider = %q, want the one the flow started against", got.Provider)
	}
	if got.RedirectTo != "/usage" {
		t.Errorf("RedirectTo = %q", got.RedirectTo)
	}
}
