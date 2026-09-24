package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The authorization rules that cannot be checked without a database, because
// what the caller may do depends on a row: who a key belongs to.
//
// Revoking is where this matters most. It is the one destructive thing a member
// can do, and it carries two separate refusals that say different things on
// purpose - a key in another tenant is reported as missing, and a colleague's
// key inside your own is reported as forbidden with a reason. Getting those the
// same way round is a working product either way, which is why nothing would
// have caught it.
//
// These skip without KEERA_TEST_DATABASE_URL, exactly as the store's own tests
// do; `make test-integration` provides one.

// tenants is two organisations with people and keys in them.
type tenants struct {
	srv *Server
	ctx context.Context
}

func twoTenants(t *testing.T) tenants {
	t.Helper()
	st, ctx := streamStore(t)
	if _, err := st.Pool().Exec(ctx,
		"TRUNCATE users, sessions, audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("emptying the tables: %v", err)
	}

	for _, org := range []struct{ id, name string }{
		{"org_a", "Example Bank"}, {"org_b", "Another Customer"},
	} {
		if _, err := st.CreateOrg(ctx, org.id, org.name); err != nil {
			t.Fatalf("CreateOrg %s: %v", org.id, err)
		}
	}
	people := []struct{ id, org, email, role string }{
		{"user_alice", "org_a", "alice@example.ch", "member"},
		{"user_bob", "org_a", "bob@example.ch", "member"},
		{"user_carol", "org_a", "carol@example.ch", "admin"},
		{"user_dave", "org_b", "dave@another.example.ch", "member"},
	}
	for _, p := range people {
		if _, err := st.UpsertUser(ctx, p.id, p.org, p.email, "sso:"+p.id, p.role); err != nil {
			t.Fatalf("UpsertUser %s: %v", p.id, err)
		}
	}
	keys := []store.KeyInfo{
		{ID: "key_alice", OrgID: "org_a", UserID: "user_alice", Alias: "alice's laptop", Prefix: "sk-a"},
		{ID: "key_bob", OrgID: "org_a", UserID: "user_bob", Alias: "bob's laptop", Prefix: "sk-b"},
		{ID: "key_orphan", OrgID: "org_a", Alias: "the build pipeline", Prefix: "sk-o"},
		{ID: "key_theirs", OrgID: "org_b", UserID: "user_dave", Alias: "dave's laptop", Prefix: "sk-d"},
	}
	for _, k := range keys {
		if _, err := st.CreateKey(ctx, k, []byte("hash-of-"+k.ID)); err != nil {
			t.Fatalf("CreateKey %s: %v", k.ID, err)
		}
	}

	// A real registry, because a successful revocation announces itself
	// through one: the whole point of revoking is that every gateway replica
	// drops the key, and a test with nothing to drop it from would not be
	// exercising the path a revocation actually takes.
	reg, err := registry.New(ctx, st, registry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}

	return tenants{
		srv: New(st, reg, nil, nil, Options{OperatorKey: testOperatorKey, Currency: "CHF"},
			slog.New(slog.DiscardHandler)),
		ctx: ctx,
	}
}

// revoke calls the handler directly, so that what is under test is the
// handler's own authorization and not the router's.
func (tn tenants) revoke(p *authn.Principal, keyID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete,
		httpx.ControlPrefix+"/v1/keys/"+keyID, nil).WithContext(tn.ctx)
	r.SetPathValue("id", keyID)
	tn.srv.revokeKey(w, r, p)
	return w
}

func TestAMemberRevokesTheirOwnKeyAndNobodyElses(t *testing.T) {
	// A member revoking their own key is the whole reason they can revoke at
	// all: a key that has leaked is killed by the person who noticed, not by
	// whoever answers the ticket.
	tn := twoTenants(t)
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}

	if w := tn.revoke(alice, "key_alice"); w.Code != http.StatusOK {
		t.Fatalf("status = %d revoking her own key, want 200: %s", w.Code, w.Body)
	}

	// A colleague's is refused, and refused with an explanation rather than a
	// 404: inside their own organisation a member already sees every key on
	// the keys screen, so pretending it does not exist would hide nothing and
	// explain nothing.
	w := tn.revoke(alice, "key_bob")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d revoking a colleague's key, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "attributed to themselves") {
		t.Errorf("body = %q, want it to say whose key a member may revoke", w.Body.String())
	}

	// A key attributed to nobody is nobody's to revoke. It is the pipeline's,
	// and taking it away stops a deployment rather than one laptop.
	if w := tn.revoke(alice, "key_orphan"); w.Code != http.StatusForbidden {
		t.Errorf("status = %d revoking an unattributed key, want 403", w.Code)
	}
}

func TestAnAdministratorRevokesAnyKeyInTheirOwnOrganisation(t *testing.T) {
	// The other half: taking somebody's access away is what an administrator is
	// for, and it has to work without knowing whose key it was.
	tn := twoTenants(t)
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}

	for _, keyID := range []string{"key_alice", "key_bob", "key_orphan"} {
		if w := tn.revoke(carol, keyID); w.Code != http.StatusOK {
			t.Errorf("status = %d revoking %s, want 200: %s", w.Code, keyID, w.Body)
		}
	}
}

func TestAKeyInAnotherTenantIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	// Answering "forbidden" for a key that exists somewhere else and "not
	// found" for one that exists nowhere turns this route into an oracle: an
	// administrator of one customer could walk the id space and learn which
	// keys another customer holds.
	//
	// So both are 404, and the same 404. The bodies are compared rather than
	// just the statuses, because a reason that differed would leak exactly the
	// same thing the status was made not to.
	tn := twoTenants(t)

	callers := []struct {
		name string
		p    *authn.Principal
	}{
		{"a member", &authn.Principal{
			Via: authn.MethodSession, Role: authn.RoleMember,
			OrgID: "org_a", UserID: "user_alice",
		}},
		{"an administrator", &authn.Principal{
			Via: authn.MethodSession, Role: authn.RoleAdmin,
			OrgID: "org_a", UserID: "user_carol",
		}},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			theirs := tn.revoke(c.p, "key_theirs")
			missing := tn.revoke(c.p, "key_no_such_thing")

			if theirs.Code != http.StatusNotFound {
				t.Errorf("another tenant's key = %d, want 404 so ids cannot be probed",
					theirs.Code)
			}
			if missing.Code != http.StatusNotFound {
				t.Errorf("a key that does not exist = %d, want 404", missing.Code)
			}
			if theirs.Body.String() != missing.Body.String() {
				t.Errorf("the two answers differ:\n existing elsewhere: %s\n not existing:       %s",
					theirs.Body, missing.Body)
			}
		})
	}
}

func TestARefusedRevocationLeavesTheKeyWorking(t *testing.T) {
	// The status is only half of it. A handler that refused and revoked anyway
	// would pass every check above.
	tn := twoTenants(t)
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}
	if w := tn.revoke(alice, "key_theirs"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if w := tn.revoke(alice, "key_bob"); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	dave := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_b", UserID: "user_dave",
	}
	// If either refusal had gone through, revoking it properly would now come
	// back as "no such live key".
	if w := tn.revoke(dave, "key_theirs"); w.Code != http.StatusOK {
		t.Errorf("status = %d revoking a key that a refused call should have left "+
			"alone, want 200: %s", w.Code, w.Body)
	}
	bob := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_bob",
	}
	if w := tn.revoke(bob, "key_bob"); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestTheDevelopersOwnScreenShowsOnlyTheirOwnKeys(t *testing.T) {
	// This screen's doc comment promises that it reads only what belongs to the
	// caller: a member sees their own keys and nobody else's, and an
	// administrator reading it sees theirs, not the organisation's. It is the
	// one screen in the panel shaped for the person using the gateway rather
	// than for whoever administers it, and it shows spend.
	tn := twoTenants(t)

	read := func(p *authn.Principal) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet,
			httpx.ControlPrefix+"/v1/access", nil).WithContext(tn.ctx)
		tn.srv.access(w, r, p)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return out
	}

	aliases := func(out map[string]any) []string {
		keys, _ := out["keys"].([]any)
		var got []string
		for _, k := range keys {
			m, _ := k.(map[string]any)
			if a, ok := m["alias"].(string); ok {
				got = append(got, a)
			}
		}
		return got
	}

	alice := read(&authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_a", UserID: "user_alice", Email: "alice@example.ch",
	})
	if got := aliases(alice); len(got) != 1 || got[0] != "alice's laptop" {
		t.Errorf("alice sees %v, want only her own key", got)
	}

	// An administrator is not exempt. This screen is "mine", not "my
	// organisation's" - the organisation's keys have a screen of their own.
	carol := read(&authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol", Email: "carol@example.ch",
	})
	if got := aliases(carol); len(got) != 0 {
		t.Errorf("an administrator sees %v on their own screen, want nothing: they "+
			"hold no keys of their own", got)
	}

	// And the organisation is the caller's, whoever else exists.
	org, _ := alice["org"].(map[string]any)
	if org["id"] != "org_a" {
		t.Errorf("org = %v, want org_a", org)
	}
}

func TestTheOperatorKeyHasNoScreenOfItsOwn(t *testing.T) {
	// It is a shared credential and not a person, so it has no keys and no
	// spend. Saying so beats an empty screen, which reads as a deployment with
	// nothing in it.
	tn := twoTenants(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		httpx.ControlPrefix+"/v1/access", nil).WithContext(tn.ctx)

	tn.srv.access(w, r, &authn.Principal{
		Via: authn.MethodOperatorKey, Role: authn.RoleOperator,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["anonymous"] != true {
		t.Errorf("anonymous = %v, want true", out["anonymous"])
	}
}

func TestRevokingIsRecordedAgainstWhoTheKeyBelongedTo(t *testing.T) {
	// A member killing their own leaked key and an administrator taking
	// somebody's access away are different events. The attribution goes in the
	// entry rather than being joined back later, because the row it would be
	// joined against may have changed by then - or been deleted with the person.
	tn := twoTenants(t)
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}
	if w := tn.revoke(carol, "key_bob"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}

	entries, err := tn.srv.st.ListAudit(tn.ctx, store.AuditQuery{
		OrgID: "org_a", From: time.Now().Add(-time.Minute), Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var found *store.AuditEntry
	for i, e := range entries {
		if e.Action == "key.revoke" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("revoking a key wrote no audit entry")
	}
	if found.TargetID != "key_bob" {
		t.Errorf("entry names %q, want the key that was revoked", found.TargetID)
	}
	if !strings.Contains(string(found.Detail), "user_bob") {
		t.Errorf("detail = %s, want it to name who the key was attributed to", found.Detail)
	}
}

// changeTeam runs one of the two handlers that name a team in the path, on
// team_a, the way the router would.
func (tn tenants) changeTeam(h handler, p *authn.Principal, method, body string) *httptest.ResponseRecorder {
	const teamID = "team_a"
	w := httptest.NewRecorder()
	target := httpx.ControlPrefix + "/v1/teams/" + teamID
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil).WithContext(tn.ctx)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(tn.ctx)
	}
	r.SetPathValue("id", teamID)
	h(w, r, p)
	return w
}

// A team names its organisation in a row rather than in the request, so the
// tenant boundary on these two routes is only as good as that lookup. This is
// the test that the lookup is made.
func TestATeamIsRenamedAndDeletedOnlyInsideItsOwnTenant(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	for _, team := range []struct{ id, org, name string }{
		{"team_a", "org_a", "Payments Platform"},
		{"team_a2", "org_a", "Data Science"},
	} {
		if _, err := st.CreateTeam(tn.ctx, team.id, team.org, team.name); err != nil {
			t.Fatalf("CreateTeam %s: %v", team.id, err)
		}
	}

	// The other customer's administrator is refused, and refused as missing:
	// a team id is not something they should be able to confirm exists.
	w := tn.changeTeam(tn.srv.updateTeam, admin("org_b"), http.MethodPatch,
		`{"name":"Ours Now"}`)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Errorf("another tenant's administrator renamed it: status = %d, %s", w.Code, w.Body)
	}

	w = tn.changeTeam(tn.srv.updateTeam, admin("org_a"), http.MethodPatch,
		`{"name":"Payments"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d renaming a team in the caller's own tenant, want 200: %s", w.Code, w.Body)
	}
	names, err := st.TeamNames(tn.ctx, "org_a")
	if err != nil || names["team_a"] != "Payments" {
		t.Errorf("after the rename TeamNames = %v, %v", names, err)
	}

	// The name a sibling team already holds comes back as a conflict that says
	// which name and why, not as an internal error over a unique index.
	w = tn.changeTeam(tn.srv.updateTeam, admin("org_a"), http.MethodPatch,
		`{"name":"Data Science"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d renaming onto a name in use, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Data Science") {
		t.Errorf("the conflict did not name the name: %s", w.Body)
	}
}

// Deleting a team with a working key in it would take the key with it, so it is
// refused - and the refusal names the credentials, because that is the part
// somebody has to deal with before they can try again.
func TestDeletingATeamIsRefusedWhileItHoldsAWorkingKey(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	if _, err := st.CreateTeam(tn.ctx, "team_a", "org_a", "Payments Platform"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := st.CreateKey(tn.ctx, store.KeyInfo{
		ID: "key_ci", OrgID: "org_a", TeamID: "team_a",
		Alias: "the build pipeline", Prefix: "sk-ci",
	}, []byte("hash-of-key_ci")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	w := tn.changeTeam(tn.srv.deleteTeam, admin("org_a"), http.MethodDelete, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d deleting a team with a live key, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "the build pipeline") {
		t.Errorf("the refusal did not name the key: %s", w.Body)
	}

	if err := st.RevokeKey(tn.ctx, "key_ci"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	w = tn.changeTeam(tn.srv.deleteTeam, admin("org_a"), http.MethodDelete, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d deleting a team whose keys are revoked, want 200: %s", w.Code, w.Body)
	}
	var body struct {
		Deleted      bool `json:"deleted"`
		DetachedKeys int  `json:"detached_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if !body.Deleted || body.DetachedKeys != 1 {
		t.Errorf("the deletion reported %+v", body)
	}
	if names, err := st.TeamNames(tn.ctx, "org_a"); err != nil || len(names) != 0 {
		t.Errorf("after the deletion TeamNames = %v, %v", names, err)
	}
}
