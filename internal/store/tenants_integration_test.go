package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestDeleteOrgTakesItsTenancyAndKeepsItsHistory(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC()

	user, err := st.UpsertUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	hash := []byte("session-hash-000000000000000001!")
	if err := st.CreateSession(ctx, hash, user.ID, "csrf", now.Add(time.Hour), "ua", "10.0.0.1"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, sc := range []struct {
		typ policy.ScopeType
		id  string
	}{{policy.ScopeOrg, f.orgID}, {policy.ScopeTeam, f.teamID}, {policy.ScopeKey, f.keyID}} {
		if err := st.PutPolicy(ctx, sc.typ, sc.id, policy.Limits{RPM: new(10)}); err != nil {
			t.Fatalf("PutPolicy %s: %v", sc.typ, err)
		}
	}
	if err := st.WriteEvents(ctx, []Event{{TS: now, OrgID: f.orgID, TeamID: f.teamID,
		KeyID: f.keyID, Alias: "keera-code", CostMicros: 500, Status: 200,
		Scopes: []policy.Scope{
			{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodMonth},
			{Type: policy.ScopeTeam, ID: f.teamID, Period: policy.PeriodMonth},
			{Type: policy.ScopeKey, ID: f.keyID, Period: policy.PeriodMonth},
		}}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	if err := st.Audit(ctx, "operator key", f.orgID, "org.delete", "org", f.orgID, nil); err != nil {
		t.Fatalf("Audit: %v", err)
	}

	gone, err := st.DeleteOrg(ctx, f.orgID)
	if err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if gone.Name != "Example Bank" || gone.Teams != 1 || gone.Users != 1 || gone.Keys != 1 {
		t.Errorf("DeletedOrg = %+v, want the counts of what went with it", gone)
	}

	// Teams, users, keys and sessions follow the foreign keys.
	for _, q := range []string{
		"SELECT count(*) FROM teams",
		"SELECT count(*) FROM users",
		"SELECT count(*) FROM api_keys",
		"SELECT count(*) FROM sessions",
		// Policies and the spend roll-up name their scope by plain id, so they
		// are cleared explicitly - and all three scopes have to go, not only
		// the org's.
		"SELECT count(*) FROM guardrails",
		"SELECT count(*) FROM spend",
	} {
		var n int
		if err := st.pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}

	// Usage and audit outlive the tenant: they are what a finance reader
	// invoices from and what a compliance reader audits, and the audit log has
	// to keep the record that this deletion happened.
	var events, entries int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM audit_log").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if events != 1 || entries != 1 {
		t.Errorf("usage_events = %d and audit_log = %d, want both kept", events, entries)
	}

	if _, err := st.DeleteOrg(ctx, f.orgID); err != ErrNotFound {
		t.Errorf("deleting it again gave %v, want ErrNotFound", err)
	}
}

func TestTenancyLookupsUsedForAuthorisation(t *testing.T) {
	// The control plane checks a named team or key against the caller's own
	// tenant before it reads or writes anything, so these have to answer for a
	// missing id rather than about one.
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if org, err := st.TeamOrg(ctx, f.teamID); err != nil || org != f.orgID {
		t.Errorf("TeamOrg = %q, %v", org, err)
	}
	if _, err := st.TeamOrg(ctx, "nobody"); err != ErrNotFound {
		t.Errorf("TeamOrg for a missing team gave %v, want ErrNotFound", err)
	}
	if org, err := st.KeyOrg(ctx, f.keyID); err != nil || org != f.orgID {
		t.Errorf("KeyOrg = %q, %v", org, err)
	}
	if _, err := st.KeyOrg(ctx, "nobody"); err != ErrNotFound {
		t.Errorf("KeyOrg for a missing key gave %v, want ErrNotFound", err)
	}

	// KeyOwner decides whether a member may revoke a key, so the attribution
	// has to come back exactly as stored. The fixture's key is attributed to
	// nobody, which is the case that must never read as "mine".
	if org, user, err := st.KeyOwner(ctx, f.keyID); err != nil || org != f.orgID || user != "" {
		t.Errorf("KeyOwner = %q, %q, %v; want the org and nobody", org, user, err)
	}
	person, err := st.UpsertUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_2", OrgID: f.orgID, UserID: person.ID,
		Alias: "theirs", Prefix: "keera_sk_theirs",
	}, []byte("hash-of-keera_sk_theirs-32-bytes!!")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if org, user, err := st.KeyOwner(ctx, "key_2"); err != nil || org != f.orgID || user != person.ID {
		t.Errorf("KeyOwner = %q, %q, %v; want the org and %q", org, user, err, person.ID)
	}
	if _, _, err := st.KeyOwner(ctx, "nobody"); err != ErrNotFound {
		t.Errorf("KeyOwner for a missing key gave %v, want ErrNotFound", err)
	}
	if ok, err := st.OrgExists(ctx, f.orgID); err != nil || !ok {
		t.Errorf("OrgExists = %v, %v", ok, err)
	}
	if ok, err := st.OrgExists(ctx, "nobody"); err != nil || ok {
		t.Errorf("OrgExists for a missing org = %v, %v", ok, err)
	}

	// A report grouped by team or key renders names, not ids.
	if names, err := st.TeamNames(ctx, f.orgID); err != nil || names[f.teamID] != "Payments Platform" {
		t.Errorf("TeamNames = %v, %v", names, err)
	}
	if aliases, err := st.KeyAliases(ctx, f.orgID); err != nil || aliases[f.keyID] != "a developer's laptop" {
		t.Errorf("KeyAliases = %v, %v", aliases, err)
	}

	// A team name is unique inside its organisation, and the error says so
	// rather than quoting a constraint.
	if _, err := st.CreateTeam(ctx, "team_2", f.orgID, "Payments Platform"); err == nil {
		t.Error("a duplicate team name was accepted")
	}
	// The same name in another tenant is a different team.
	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTeam(ctx, "team_3", "org_2", "Payments Platform"); err != nil {
		t.Errorf("the same team name in another organisation was refused: %v", err)
	}
}

// A rename is one column, and the point of the test is everything it did not
// touch: a key resolves by hash and not by team name, and the guardrails, the
// spend and the id all stay where they were.
func TestRenameTeamLeavesEverythingThatPointsAtItAlone(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID,
		policy.Limits{RPM: new(60)}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}

	renamed, err := st.RenameTeam(ctx, f.teamID, "Payments")
	if err != nil {
		t.Fatalf("RenameTeam: %v", err)
	}
	if renamed.ID != f.teamID || renamed.Name != "Payments" || renamed.OrgID != f.orgID {
		t.Errorf("RenameTeam returned %+v", renamed)
	}

	// The key still resolves, and it resolves to the same team carrying the
	// same limits. This is the whole claim the dialog in the panel makes.
	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey after the rename: %v", err)
	}
	if res.Key.TeamID != f.teamID {
		t.Errorf("the key's team = %q, want %q", res.Key.TeamID, f.teamID)
	}
	lim, err := st.GetPolicy(ctx, policy.ScopeTeam, f.teamID)
	if err != nil || lim.RPM == nil || *lim.RPM != 60 {
		t.Errorf("the team's guardrails after the rename = %+v, %v", lim, err)
	}
	if names, err := st.TeamNames(ctx, f.orgID); err != nil || names[f.teamID] != "Payments" {
		t.Errorf("TeamNames = %v, %v", names, err)
	}

	// The name is still one per organisation, and the refusal is the typed one
	// the control plane turns into a sentence rather than a constraint name.
	if _, err := st.CreateTeam(ctx, "team_2", f.orgID, "Data Science"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := st.RenameTeam(ctx, "team_2", "Payments"); !errors.Is(err, ErrTeamNameTaken) {
		t.Errorf("renaming onto a name in use = %v, want ErrTeamNameTaken", err)
	}
	// Renaming a team to what it is already called is not a collision with
	// itself. Somebody correcting the capitalisation of one word would meet
	// that as an error otherwise.
	if _, err := st.RenameTeam(ctx, f.teamID, "Payments"); err != nil {
		t.Errorf("renaming a team to its own name: %v", err)
	}
	if _, err := st.RenameTeam(ctx, "team_gone", "Anything"); !errors.Is(err, ErrNotFound) {
		t.Errorf("renaming a team that does not exist = %v, want ErrNotFound", err)
	}
}

// Deleting a team is refused while a key in it still works, because the foreign
// key cascades: the alternative to this check is credentials disappearing and a
// production job finding out.
func TestDeleteTeamIsRefusedWhileAKeyStillWorks(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID,
		policy.Limits{RPM: new(60)}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}

	_, err := st.DeleteTeam(ctx, f.teamID)
	var inUse *TeamInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("DeleteTeam with a live key = %v, want TeamInUseError", err)
	}
	// The aliases are carried because "in use" is not something anybody can
	// act on and "this credential" is.
	if !slices.Equal(inUse.Aliases, []string{"a developer's laptop"}) {
		t.Errorf("the refusal named %v", inUse.Aliases)
	}
	if names, err := st.TeamNames(ctx, f.orgID); err != nil || names[f.teamID] == "" {
		t.Errorf("the team went away despite the refusal: %v, %v", names, err)
	}

	// Revoked, the key is no longer a reason to refuse - but it is still a row
	// the usage log points at, so the delete detaches it instead of taking it.
	if err := st.RevokeKey(ctx, f.keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	gone, err := st.DeleteTeam(ctx, f.teamID)
	if err != nil {
		t.Fatalf("DeleteTeam after revoking: %v", err)
	}
	if gone.Name != "Payments Platform" || gone.DetachedKeys != 1 {
		t.Errorf("DeleteTeam reported %+v", gone)
	}

	var teamID *string
	if err := st.pool.QueryRow(ctx,
		"SELECT team_id FROM api_keys WHERE id = $1", f.keyID).Scan(&teamID); err != nil {
		t.Fatalf("the revoked key did not survive its team: %v", err)
	}
	if teamID != nil {
		t.Errorf("the revoked key still names team %q", *teamID)
	}
	// The guardrails name their scope by plain id with no foreign key to
	// follow, so nothing would have cleared them.
	if _, err := st.GetPolicy(ctx, policy.ScopeTeam, f.teamID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the team's guardrails outlived it: %v", err)
	}
	if _, err := st.DeleteTeam(ctx, f.teamID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it twice = %v, want ErrNotFound", err)
	}
}

func TestSetupStateCountsWhatAFirstRunHasToCreate(t *testing.T) {
	st, ctx := db(t)

	empty, err := st.SetupState(ctx, "")
	if err != nil {
		t.Fatalf("SetupState: %v", err)
	}
	if empty != (Setup{}) {
		t.Errorf("SetupState on an empty deployment = %+v, want all zeroes", empty)
	}

	f := newFixture(t, st, ctx)
	if _, err := st.UpsertUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertModel(ctx, policy.Model{Alias: "keera-code", Kind: policy.KindChat,
		Backends: []string{"http://vllm:8000/v1"}, BackendModel: "served", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// A disabled model is not one a client can reach, so it is not a step
	// somebody has completed.
	if err := st.UpsertModel(ctx, policy.Model{Alias: "keera-off", Kind: policy.KindChat,
		Backends: []string{"http://vllm:8000/v1"}, BackendModel: "served", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteEvents(ctx, []Event{{TS: time.Now(), OrgID: f.orgID,
		Alias: "keera-code", Status: 200}}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SetupState(ctx, f.orgID)
	if err != nil {
		t.Fatalf("SetupState: %v", err)
	}
	want := Setup{Orgs: 1, Teams: 1, Keys: 1, Models: 1, People: 1, Requests: 1}
	if got != want {
		t.Errorf("SetupState = %+v, want %+v", got, want)
	}
}
