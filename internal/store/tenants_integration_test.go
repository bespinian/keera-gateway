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

	user, err := st.AddUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	hash := []byte("session-hash-000000000000000001!")
	if err := st.CreateSession(ctx, hash, user.ID, "csrf", now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, sc := range []struct {
		typ policy.ScopeType
		id  string
	}{{policy.ScopeOrg, f.orgID}, {policy.ScopeProject, f.projectID}, {policy.ScopeKey, f.keyID}} {
		if err := st.PutGuardrail(ctx, sc.typ, sc.id, policy.Limits{RPM: new(10)}); err != nil {
			t.Fatalf("PutGuardrail %s: %v", sc.typ, err)
		}
	}
	if err := st.WriteEvents(ctx, []Event{{TS: now, OrgID: f.orgID, ProjectID: f.projectID,
		KeyID: f.keyID, Alias: "keera-code", CostMicros: 500, Status: 200,
		Scopes: []policy.Scope{
			{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodMonth},
			{Type: policy.ScopeProject, ID: f.projectID, Period: policy.PeriodMonth},
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
	if gone.Name != "Example Bank" || gone.Projects != 2 || gone.Users != 1 || gone.Keys != 1 {
		t.Errorf("DeletedOrg = %+v, want the counts of what went with it", gone)
	}

	// Projects, users, keys and sessions follow the foreign keys.
	for _, q := range []string{
		"SELECT count(*) FROM projects",
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
	// The control plane checks a named project or key against the caller's own
	// tenant before it reads or writes anything, so these have to answer for a
	// missing id rather than about one.
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if org, err := st.ProjectOrg(ctx, f.projectID); err != nil || org != f.orgID {
		t.Errorf("ProjectOrg = %q, %v", org, err)
	}
	if _, err := st.ProjectOrg(ctx, "nobody"); err != ErrNotFound {
		t.Errorf("ProjectOrg for a missing project gave %v, want ErrNotFound", err)
	}
	// KeyOwnerOf decides whether a member may revoke a key, so the
	// attribution has to come back exactly as stored. The fixture's key is
	// attributed to nobody, which is the case that must never read as "mine".
	want := KeyOwner{OrgID: f.orgID, ProjectID: f.projectID}
	if o, err := st.KeyOwnerOf(ctx, f.keyID); err != nil || o != want {
		t.Errorf("KeyOwnerOf = %+v, %v; want %+v", o, err, want)
	}
	person, err := st.AddUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if _, err := st.CreateKey(ctx, KeyInfo{
		ID: "key_2", OrgID: f.orgID, UserID: person.ID,
		Name: "theirs", Prefix: "keera_sk_theirs",
	}, []byte("hash-of-keera_sk_theirs-32-bytes!!")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// A key issued without a project is in the organisation's oldest one.
	firstID, err := st.FirstProject(ctx, f.orgID)
	if err != nil {
		t.Fatalf("FirstProject: %v", err)
	}
	want = KeyOwner{OrgID: f.orgID, ProjectID: firstID, UserID: person.ID}
	if o, err := st.KeyOwnerOf(ctx, "key_2"); err != nil || o != want {
		t.Errorf("KeyOwnerOf = %+v, %v; want %+v", o, err, want)
	}
	if _, err := st.KeyOwnerOf(ctx, "nobody"); err != ErrNotFound {
		t.Errorf("KeyOwnerOf for a missing key gave %v, want ErrNotFound", err)
	}

	// A report grouped by project or key renders names, not ids.
	if names, err := st.ProjectNames(ctx, f.orgID); err != nil || names[f.projectID] != "Payments Platform" {
		t.Errorf("ProjectNames = %v, %v", names, err)
	}
	if names, err := st.KeyNames(ctx, f.orgID); err != nil || names[f.keyID] != "a developer's laptop" {
		t.Errorf("KeyNames = %v, %v", names, err)
	}

	// A project name is unique inside its organisation, and the error says so
	// rather than quoting a constraint.
	if _, err := st.CreateProject(ctx, Project{ID: "project_2", OrgID: f.orgID, Name: "Payments Platform"}); !errors.Is(err, ErrProjectNameTaken) {
		t.Errorf("a duplicate project name = %v, want ErrProjectNameTaken", err)
	}
	// The same name in another tenant is a different project.
	if _, err := st.CreateOrg(ctx, Org{ID: "org_2", Name: "Another Bank"}, OrgTemplate{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(ctx, Project{ID: "project_3", OrgID: "org_2", Name: "Payments Platform"}); err != nil {
		t.Errorf("the same project name in another organisation was refused: %v", err)
	}
}

// A rename is one column, and the point of the test is everything it did not
// touch: a key resolves by hash and not by project name, and the guardrails,
// the spend and the id all stay where they were.
func TestUpdateProjectLeavesEverythingThatPointsAtItAlone(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID,
		policy.Limits{RPM: new(60)}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}

	renamed, err := st.UpdateProject(ctx, f.projectID, ProjectChange{Name: new("Payments")})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if renamed.ID != f.projectID || renamed.Name != "Payments" || renamed.OrgID != f.orgID {
		t.Errorf("UpdateProject returned %+v", renamed)
	}

	// A description leaves the name alone, and an empty one clears it.
	described, err := st.UpdateProject(ctx, f.projectID,
		ProjectChange{Description: new("Card payments and refunds")})
	if err != nil || described.Name != "Payments" || described.Description != "Card payments and refunds" {
		t.Errorf("setting the description = %+v, %v", described, err)
	}
	cleared, err := st.UpdateProject(ctx, f.projectID, ProjectChange{Description: new("")})
	if err != nil || cleared.Description != "" {
		t.Errorf("clearing the description = %+v, %v", cleared, err)
	}

	// The key still resolves, and it resolves to the same project carrying the
	// same limits. This is the whole claim the dialog in the panel makes.
	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey after the rename: %v", err)
	}
	if res.Key.ProjectID != f.projectID {
		t.Errorf("the key's project = %q, want %q", res.Key.ProjectID, f.projectID)
	}
	lim, err := st.GetGuardrail(ctx, policy.ScopeProject, f.projectID)
	if err != nil || lim.RPM == nil || *lim.RPM != 60 {
		t.Errorf("the project's guardrails after the rename = %+v, %v", lim, err)
	}
	if names, err := st.ProjectNames(ctx, f.orgID); err != nil || names[f.projectID] != "Payments" {
		t.Errorf("ProjectNames = %v, %v", names, err)
	}

	// The name is still one per organisation, and the refusal is the typed one
	// the control plane turns into a sentence rather than a constraint name.
	if _, err := st.CreateProject(ctx, Project{ID: "project_2", OrgID: f.orgID, Name: "Data Science"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.UpdateProject(ctx, "project_2", ProjectChange{Name: new("Payments")}); !errors.Is(err, ErrProjectNameTaken) {
		t.Errorf("renaming onto a name in use = %v, want ErrProjectNameTaken", err)
	}
	// Renaming a project to what it is already called is not a collision with
	// itself. Somebody correcting the capitalisation of one word would meet
	// that as an error otherwise.
	if _, err := st.UpdateProject(ctx, f.projectID, ProjectChange{Name: new("Payments")}); err != nil {
		t.Errorf("renaming a project to its own name: %v", err)
	}
	if _, err := st.UpdateProject(ctx, "project_gone", ProjectChange{Name: new("Anything")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("changing a project that does not exist = %v, want ErrNotFound", err)
	}
}

// Deleting a project is refused while a key in it still works, because the foreign
// key cascades: the alternative to this check is credentials disappearing and a
// production job finding out.
func TestDeleteProjectIsRefusedWhileAKeyStillWorks(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID,
		policy.Limits{RPM: new(60)}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}

	_, err := st.DeleteProject(ctx, f.projectID)
	var inUse *ProjectInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("DeleteProject with a live key = %v, want ProjectInUseError", err)
	}
	// The names are carried because "in use" is not something anybody can
	// act on and "this credential" is.
	if !slices.Equal(inUse.Keys, []string{"a developer's laptop"}) {
		t.Errorf("the refusal named %v", inUse.Keys)
	}
	if names, err := st.ProjectNames(ctx, f.orgID); err != nil || names[f.projectID] == "" {
		t.Errorf("the project went away despite the refusal: %v, %v", names, err)
	}

	// Revoked, the key is no longer a reason to refuse - but it is still a row
	// the usage log points at, so the delete detaches it instead of taking it.
	if err := st.RevokeKey(ctx, f.keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	gone, err := st.DeleteProject(ctx, f.projectID)
	if err != nil {
		t.Fatalf("DeleteProject after revoking: %v", err)
	}
	if gone.Name != "Payments Platform" || gone.DetachedKeys != 1 {
		t.Errorf("DeleteProject reported %+v", gone)
	}

	var projectID *string
	if err := st.pool.QueryRow(ctx,
		"SELECT project_id FROM api_keys WHERE id = $1", f.keyID).Scan(&projectID); err != nil {
		t.Fatalf("the revoked key did not survive its project: %v", err)
	}
	if projectID != nil {
		t.Errorf("the revoked key still names project %q", *projectID)
	}
	// The guardrails name their scope by plain id with no foreign key to
	// follow, so nothing would have cleared them.
	if _, err := st.GetGuardrail(ctx, policy.ScopeProject, f.projectID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the project's guardrails outlived it: %v", err)
	}
	if _, err := st.DeleteProject(ctx, f.projectID); !errors.Is(err, ErrNotFound) {
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
	if _, err := st.AddUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertModel(ctx, policy.Model{OrgID: f.orgID, Alias: "keera-code", Kind: policy.KindChat,
		Backends: []string{"http://vllm:8000/v1"}, BackendModel: "served", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// A disabled model is not one a client can reach, so it is not a step
	// somebody has completed.
	if err := st.UpsertModel(ctx, policy.Model{OrgID: f.orgID, Alias: "keera-off", Kind: policy.KindChat,
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
	want := Setup{Orgs: 1, Keys: 1, Models: 1, People: 1, Requests: 1}
	if got != want {
		t.Errorf("SetupState = %+v, want %+v", got, want)
	}
}

// Adding someone never changes a person who is already there. Otherwise an
// administrator could rebind a member's subject to an identity of their own
// and sign in as them, or re-role them past the checks a role change has.
func TestAddUserLeavesAnExistingPersonAlone(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	first, err := st.AddUser(ctx, "user_1", f.orgID, "dev@example.ch", "sso:dev", "admin")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if _, err := st.AddUser(ctx, "user_2", f.orgID, "dev@example.ch", "sso:attacker", "member"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("adding the same address again gave %v, want ErrUserExists", err)
	}
	got, err := st.UserByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.ExternalID != "sso:dev" || got.Role != "admin" {
		t.Errorf("the person became %q as %s, want them untouched", got.ExternalID, got.Role)
	}

	if _, err := st.AddUser(ctx, "user_3", f.orgID, "other@example.ch", "sso:dev", "member"); !errors.Is(err, ErrExternalIDTaken) {
		t.Errorf("reusing another person's subject gave %v, want ErrExternalIDTaken", err)
	}
}

// A taken email domain refuses the whole create. An organisation created
// before the domain was refused would be made a second time on the retry.
func TestCreateOrgWithATakenDomainCreatesNothing(t *testing.T) {
	st, ctx := db(t)
	if _, err := st.CreateOrg(ctx, Org{ID: "org_1", Name: "Example Bank",
		EmailDomain: "example.ch"}, OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	_, err := st.CreateOrg(ctx, Org{ID: "org_2", Name: "Another Bank",
		EmailDomain: "Example.ch"}, OrgTemplate{})
	if !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("CreateOrg with a taken domain = %v, want ErrDomainTaken", err)
	}
	orgs, err := st.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}
	if len(orgs) != 1 || orgs[0].EmailDomain != "example.ch" {
		t.Errorf("ListOrgs = %+v, want only the first organisation", orgs)
	}
}

// Names are unique across organisations, ignoring case, on create and on
// rename alike. A rename and a domain change go in one statement, so a refused
// domain keeps the old name too.
func TestOrgNamesAreUnique(t *testing.T) {
	st, ctx := db(t)
	for _, o := range []Org{
		{ID: "org_1", Name: "Example Bank", EmailDomain: "example.ch"},
		{ID: "org_2", Name: "Another Bank"},
	} {
		if _, err := st.CreateOrg(ctx, o, OrgTemplate{}); err != nil {
			t.Fatalf("CreateOrg %s: %v", o.ID, err)
		}
	}
	if _, err := st.CreateOrg(ctx, Org{ID: "org_3", Name: "example bank"}, OrgTemplate{}); !errors.Is(err, ErrOrgNameTaken) {
		t.Errorf("CreateOrg with a taken name = %v, want ErrOrgNameTaken", err)
	}

	name := "Example Bank AG"
	org, err := st.UpdateOrg(ctx, "org_1", OrgChange{Name: &name})
	if err != nil {
		t.Fatalf("UpdateOrg: %v", err)
	}
	if org.Name != name || org.EmailDomain != "example.ch" {
		t.Errorf("UpdateOrg = %+v, want the new name and the domain it had", org)
	}

	taken := "EXAMPLE BANK AG"
	if _, err := st.UpdateOrg(ctx, "org_2", OrgChange{Name: &taken}); !errors.Is(err, ErrOrgNameTaken) {
		t.Errorf("renaming onto a name in use = %v, want ErrOrgNameTaken", err)
	}
	// Changing only the capitalisation is not a collision with itself.
	lower := "example bank ag"
	if _, err := st.UpdateOrg(ctx, "org_1", OrgChange{Name: &lower}); err != nil {
		t.Errorf("renaming an organisation to its own name in lower case: %v", err)
	}

	fresh, domain := "Another Bank AG", "Example.CH"
	if _, err := st.UpdateOrg(ctx, "org_2", OrgChange{Name: &fresh, EmailDomain: &domain}); !errors.Is(err, ErrDomainTaken) {
		t.Errorf("a taken domain = %v, want ErrDomainTaken", err)
	}
	orgs, err := st.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}
	for _, o := range orgs {
		if o.ID == "org_2" && o.Name != "Another Bank" {
			t.Errorf("org_2 is called %q after a refused update, want the old name", o.Name)
		}
	}

	if _, err := st.UpdateOrg(ctx, "nobody", OrgChange{Name: &fresh}); err != ErrNotFound {
		t.Errorf("renaming a missing org = %v, want ErrNotFound", err)
	}
}

// Every working key is in a project, so a new organisation comes with one,
// and a key issued without a project goes in the oldest project there is.
func TestAKeyWithoutAProjectGoesInTheOldestOne(t *testing.T) {
	st, ctx := db(t)
	if _, err := st.CreateOrg(ctx, Org{ID: "org_1", Name: "Example Bank"}, OrgTemplate{}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ProjectSummaries(ctx, "org_1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Name != DefaultProjectName ||
		projects[0].Description != "The default project of Example Bank" {
		t.Fatalf("a new organisation's projects = %+v, want one called %q", projects, DefaultProjectName)
	}
	first := projects[0].ID
	if _, err := st.CreateProject(ctx, Project{ID: "project_zz", OrgID: "org_1", Name: "Payments"}); err != nil {
		t.Fatal(err)
	}

	newKey := func(id string) (KeyInfo, error) {
		return st.CreateKey(ctx, KeyInfo{ID: id, OrgID: "org_1", Name: id, Prefix: id},
			[]byte("hash-of-"+id))
	}
	key, err := newKey("key_1")
	if err != nil {
		t.Fatalf("CreateKey without a project: %v", err)
	}
	if key.ProjectID != first {
		t.Errorf("the key is in project %q, want the first %q", key.ProjectID, first)
	}

	// The first project is deleted like any other, and the next oldest takes
	// its place.
	if err := st.RevokeKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteProject(ctx, first); err != nil {
		t.Fatalf("deleting the first project: %v", err)
	}
	if key, err = newKey("key_2"); err != nil || key.ProjectID != "project_zz" {
		t.Errorf("with the first project gone = %+v, %v; want a key in project_zz", key, err)
	}

	// With no project at all there can be no keys, because only a revoked
	// key may have none.
	if err := st.RevokeKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteProject(ctx, "project_zz"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FirstProject(ctx, "org_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FirstProject with none = %v, want ErrNotFound", err)
	}
	if _, err := newKey("key_3"); !errors.Is(err, ErrNoProject) {
		t.Errorf("CreateKey with no projects = %v, want ErrNoProject", err)
	}
}
