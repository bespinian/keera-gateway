package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The authorization rules that depend on a row, such as who a key belongs to.
//
// Revoking matters most. It has two refusals that differ on purpose: a key in
// another tenant is missing, and a colleague's key in your own is forbidden,
// with a reason.
//
// These skip without KEERA_TEST_DATABASE_URL; `make test-integration` provides
// one.

// tenants is two organisations with people and keys in them.
type tenants struct {
	srv *Server
	ctx context.Context
}

func twoTenants(t *testing.T) tenants {
	t.Helper()
	st, ctx := testStore(t, "users", "sessions", "audit_log")

	for _, org := range []struct{ id, name string }{
		{"org_a", "Example Bank"}, {"org_b", "Another Customer"},
	} {
		if _, err := st.CreateOrg(ctx, store.Org{ID: org.id, Name: org.name}, store.OrgTemplate{}); err != nil {
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
		if _, err := st.AddUser(ctx, p.id, p.org, p.email, "sso:"+p.id, p.role); err != nil {
			t.Fatalf("AddUser %s: %v", p.id, err)
		}
	}
	keys := []store.KeyInfo{
		{ID: "key_alice", OrgID: "org_a", UserID: "user_alice", Name: "alice's laptop", Prefix: "sk-a"},
		{ID: "key_bob", OrgID: "org_a", UserID: "user_bob", Name: "bob's laptop", Prefix: "sk-b"},
		{ID: "key_orphan", OrgID: "org_a", Name: "the build pipeline", Prefix: "sk-o"},
		{ID: "key_theirs", OrgID: "org_b", UserID: "user_dave", Name: "dave's laptop", Prefix: "sk-d"},
	}
	for _, k := range keys {
		if _, err := st.CreateKey(ctx, k, []byte("hash-of-"+k.ID)); err != nil {
			t.Fatalf("CreateKey %s: %v", k.ID, err)
		}
	}

	// A real registry, because a revocation is announced through it so every
	// gateway replica drops the key.
	reg, err := registry.New(ctx, st, registry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}

	return tenants{
		srv: New(st, reg, nil, nil, Options{OperatorKey: testOperatorKey},
			slog.New(slog.DiscardHandler)),
		ctx: ctx,
	}
}

// revoke calls the handler directly, to test its own authorization.
func (tn tenants) revoke(p *authn.Principal, keyID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete,
		httpx.ControlPrefix+"/v1/keys/"+keyID, nil).WithContext(tn.ctx)
	r.SetPathValue("id", keyID)
	tn.srv.revokeKey(w, r, p)
	return w
}

func TestAMemberRevokesTheirOwnKeyAndNobodyElses(t *testing.T) {
	// A member can revoke their own key, so whoever notices a leak can kill it.
	tn := twoTenants(t)
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}

	if w := tn.revoke(alice, "key_alice"); w.Code != http.StatusOK {
		t.Fatalf("status = %d revoking her own key, want 200: %s", w.Code, w.Body)
	}

	// A colleague's is refused with a reason, not a 404: a member already sees
	// every key in their organisation, so a 404 would hide nothing.
	w := tn.revoke(alice, "key_bob")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d revoking a colleague's key, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "attributed to themselves") {
		t.Errorf("body = %q, want it to say whose key a member may revoke", w.Body.String())
	}

	// A key attributed to nobody is a pipeline's, so a member may not revoke it.
	if w := tn.revoke(alice, "key_orphan"); w.Code != http.StatusForbidden {
		t.Errorf("status = %d revoking an unattributed key, want 403", w.Code)
	}
}

func TestAnAdministratorRevokesAnyKeyInTheirOwnOrganisation(t *testing.T) {
	// An administrator can take anybody's access away.
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

// Whoever may revoke a key may rotate it, so a member replaces their own
// leaked key. The new key keeps the old one's person and own guardrails, and
// the old one stops working in the same step.
func TestAMemberRotatesTheirOwnKeyAndItKeepsItsLimits(t *testing.T) {
	tn := twoTenants(t)
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}
	dave := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_b", UserID: "user_dave",
	}
	rpm := 120
	if err := tn.srv.st.PutGuardrail(tn.ctx, policy.ScopeKey, "key_alice", policy.Limits{RPM: &rpm}); err != nil {
		t.Fatal(err)
	}
	rotate := func(p *authn.Principal, keyID string) *httptest.ResponseRecorder {
		return tn.call(tn.srv.rotateKey, p, http.MethodPost, "/v1/keys/"+keyID+"/rotate", `{}`,
			map[string]string{"id": keyID})
	}

	if w := rotate(dave, "key_alice"); w.Code != http.StatusNotFound {
		t.Errorf("another tenant rotating her key = %d, want 404", w.Code)
	}
	if w := rotate(alice, "key_bob"); w.Code != http.StatusForbidden {
		t.Errorf("rotating a colleague's key = %d, want 403", w.Code)
	}
	// The lifetime is the administrator's choice, so a member cannot stretch it.
	w := tn.call(tn.srv.rotateKey, alice, http.MethodPost, "/v1/keys/key_alice/rotate",
		`{"expires_in_seconds":315360000}`, map[string]string{"id": "key_alice"})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "lifetime") {
		t.Errorf("a member choosing the lifetime = %d %s, want 403", w.Code, w.Body)
	}

	w = rotate(alice, "key_alice")
	if w.Code != http.StatusCreated {
		t.Fatalf("rotating her own key = %d, want 201: %s", w.Code, w.Body)
	}
	var created struct {
		store.KeyInfo
		Key           string `json:"key"`
		ReplacedKeyID string `json:"replaced_key_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" || created.ReplacedKeyID != "key_alice" || created.UserID != "user_alice" ||
		created.Name != "alice's laptop" {
		t.Errorf("the new key is %+v", created)
	}
	lim, err := tn.srv.st.GetGuardrail(tn.ctx, policy.ScopeKey, created.ID)
	if err != nil || lim.RPM == nil || *lim.RPM != 120 {
		t.Errorf("the new key's own guardrail = %+v, %v; want rpm 120", lim, err)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_alice")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("the old key = %v, want revoked", err)
	}
	if w := rotate(alice, "key_alice"); w.Code != http.StatusConflict {
		t.Errorf("rotating a revoked key = %d, want 409", w.Code)
	}
}

// A key left for its holder has no secret until they activate it, so the
// administrator who issued it never sees one. The holder may activate it even
// without the right to rotate their own keys.
func TestAPendingKeyIsActivatedByItsHolderOnce(t *testing.T) {
	tn := twoTenants(t)
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	alice := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_a", UserID: "user_alice"}
	bob := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_a", UserID: "user_bob"}

	if w := tn.create(carol, `{"org_id":"org_a","name":"ci","pending":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("a pending key for nobody: %d %s", w.Code, w.Body)
	}
	w := tn.create(carol, `{"org_id":"org_a","user_id":"user_alice","name":"alice's laptop","pending":true}`)
	var issued struct {
		store.KeyInfo
		Key string `json:"key"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &issued) != nil ||
		!issued.Pending || issued.Key != "" || issued.Prefix != "" {
		t.Fatalf("issuing a pending key: %d %s", w.Code, w.Body)
	}
	activate := func(p *authn.Principal) *httptest.ResponseRecorder {
		return tn.call(tn.srv.activateKey, p, http.MethodPost, "/v1/keys/"+issued.ID+"/activate",
			`{}`, map[string]string{"id": issued.ID})
	}
	rotate := tn.call(tn.srv.rotateKey, alice, http.MethodPost, "/v1/keys/"+issued.ID+"/rotate",
		`{}`, map[string]string{"id": issued.ID})
	if rotate.Code != http.StatusConflict {
		t.Errorf("rotating a pending key = %d, want 409", rotate.Code)
	}
	if w := activate(bob); w.Code != http.StatusForbidden {
		t.Errorf("a colleague activating it = %d, want 403", w.Code)
	}

	w = activate(alice)
	var active struct {
		store.KeyInfo
		Key string `json:"key"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &active) != nil ||
		active.Key == "" || active.Pending || active.Prefix == "" {
		t.Fatalf("activating her own key: %d %s", w.Code, w.Body)
	}
	if w := activate(alice); w.Code != http.StatusConflict {
		t.Errorf("activating it twice = %d, want 409", w.Code)
	}
}

// Whoever may revoke a key may rename it: a member their own, an
// administrator any in their organisation, an operator any at all.
func TestKeysAreRenamedByWhoeverMayRevokeThem(t *testing.T) {
	tn := twoTenants(t)
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}
	rename := func(p *authn.Principal, keyID, body string) *httptest.ResponseRecorder {
		return tn.call(tn.srv.renameKey, p, http.MethodPatch, "/v1/keys/"+keyID, body,
			map[string]string{"id": keyID})
	}
	named := func(keyID string) string {
		names, err := tn.srv.st.KeyNames(tn.ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return names[keyID]
	}

	for _, c := range []struct {
		who   string
		p     *authn.Principal
		keyID string
		want  int
	}{
		{"a member, their own", alice, "key_alice", http.StatusOK},
		{"a member, a colleague's", alice, "key_bob", http.StatusForbidden},
		{"a member, nobody's", alice, "key_orphan", http.StatusForbidden},
		{"a member, another tenant's", alice, "key_theirs", http.StatusNotFound},
		{"an administrator, a member's", carol, "key_bob", http.StatusOK},
		{"an administrator, nobody's", carol, "key_orphan", http.StatusOK},
		{"an administrator, another tenant's", carol, "key_theirs", http.StatusNotFound},
		{"an operator, any", operator, "key_theirs", http.StatusOK},
	} {
		before := named(c.keyID)
		w := rename(c.p, c.keyID, `{"name":"  renamed  "}`)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d: %s", c.who, w.Code, c.want, w.Body)
		}
		want := before
		if c.want == http.StatusOK {
			want = "renamed"
		}
		if got := named(c.keyID); got != want {
			t.Errorf("%s: the key is called %q, want %q", c.who, got, want)
		}
	}

	if w := rename(carol, "key_alice", `{"name":" "}`); w.Code != http.StatusBadRequest {
		t.Errorf("an empty name = %d, want 400", w.Code)
	}

	entries, err := tn.srv.st.ListAudit(tn.ctx, store.AuditQuery{
		OrgID: "org_a", From: time.Now().Add(-time.Minute), Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "key.rename" && e.TargetID == "key_bob" {
			if !strings.Contains(string(e.Detail), "bob's laptop") {
				t.Errorf("detail = %s, want the old name kept", e.Detail)
			}
			return
		}
	}
	t.Error("renaming a key wrote no audit entry")
}

func TestAKeyInAnotherTenantIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	// A key in another tenant and a key that does not exist must get the same
	// 404, body included. Otherwise an administrator could probe ids to learn
	// another customer's keys.
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
	// A handler that refused and revoked anyway would pass every check above.
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
	// This screen shows only the caller's own keys and spend, for members and
	// administrators alike.
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
			if a, ok := m["name"].(string); ok {
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

	// An administrator is not exempt: the organisation's keys have their own
	// screen.
	carol := read(&authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol", Email: "carol@example.ch",
	})
	if got := aliases(carol); len(got) != 0 {
		t.Errorf("an administrator sees %v on their own screen, want nothing: they "+
			"hold no keys of their own", got)
	}

	// The organisation is the caller's.
	org, _ := alice["org"].(map[string]any)
	if org["id"] != "org_a" {
		t.Errorf("org = %v, want org_a", org)
	}
}

func TestTheOperatorKeyHasNoScreenOfItsOwn(t *testing.T) {
	// It is not a person, so it has no keys or spend. Saying so beats an empty
	// screen.
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
	// A member revoking their own key and an administrator revoking somebody's
	// are different events. The entry records who, because the user row may
	// later change or be deleted.
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

// changeProject runs one of the two handlers that name a project in the path, on
// project_a, the way the router would.
func (tn tenants) changeProject(h handler, p *authn.Principal, method, body string) *httptest.ResponseRecorder {
	const projectID = "project_a"
	w := httptest.NewRecorder()
	target := httpx.ControlPrefix + "/v1/projects/" + projectID
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil).WithContext(tn.ctx)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(tn.ctx)
	}
	r.SetPathValue("id", projectID)
	h(w, r, p)
	return w
}

// A project's organisation comes from its row, not the request, so these
// routes must look it up.
func TestAProjectIsChangedAndDeletedOnlyInsideItsOwnTenant(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	for _, project := range []struct{ id, org, name string }{
		{"project_a", "org_a", "Payments Platform"},
		{"project_a2", "org_a", "Data Science"},
	} {
		if _, err := st.CreateProject(tn.ctx, store.Project{ID: project.id, OrgID: project.org, Name: project.name}); err != nil {
			t.Fatalf("CreateProject %s: %v", project.id, err)
		}
	}

	// The other customer's administrator is told it is missing, so they cannot
	// confirm the id exists.
	w := tn.changeProject(tn.srv.updateProject, admin("org_b"), http.MethodPatch,
		`{"name":"Ours Now"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("another tenant's administrator changed it: status = %d, want 404: %s", w.Code, w.Body)
	}

	w = tn.changeProject(tn.srv.updateProject, admin("org_a"), http.MethodPatch,
		`{"name":"Payments","description":"Card payments"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d changing a project in the caller's own tenant, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"description":"Card payments"`) {
		t.Errorf("the change did not keep the description: %s", w.Body)
	}
	names, err := st.ProjectNames(tn.ctx, "org_a")
	if err != nil || names["project_a"] != "Payments" {
		t.Errorf("after the change ProjectNames = %v, %v", names, err)
	}

	// The name a sibling project already holds comes back as a conflict that
	// says which name and why, not as an internal error over a unique index.
	w = tn.changeProject(tn.srv.updateProject, admin("org_a"), http.MethodPatch,
		`{"name":"Data Science"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d renaming onto a name in use, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Data Science") {
		t.Errorf("the conflict did not name the name: %s", w.Body)
	}
}

// Deleting a project with a working key would take the key with it, so it is
// refused, naming the keys to deal with first.
func TestDeletingAProjectIsRefusedWhileItHoldsAWorkingKey(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	if _, err := st.CreateProject(tn.ctx, store.Project{ID: "project_a", OrgID: "org_a", Name: "Payments Platform"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateKey(tn.ctx, store.KeyInfo{
		ID: "key_ci", OrgID: "org_a", ProjectID: "project_a",
		Name: "the build pipeline", Prefix: "sk-ci",
	}, []byte("hash-of-key_ci")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	w := tn.changeProject(tn.srv.deleteProject, admin("org_a"), http.MethodDelete, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d deleting a project with a live key, want 409: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "the build pipeline") {
		t.Errorf("the refusal did not name the key: %s", w.Body)
	}

	if err := st.RevokeKey(tn.ctx, "key_ci"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	w = tn.changeProject(tn.srv.deleteProject, admin("org_a"), http.MethodDelete, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d deleting a project whose keys are revoked, want 200: %s", w.Code, w.Body)
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
	if names, err := st.ProjectNames(tn.ctx, "org_a"); err != nil || names["project_a"] != "" {
		t.Errorf("after the deletion ProjectNames = %v, %v", names, err)
	}
}

// The project an organisation is created with is a project like any other.
// A key that names none goes in the oldest project there is, and with no
// projects at all there can be no keys - and the refusal says so.
func TestAKeyWithoutAProjectGoesInTheOldestOne(t *testing.T) {
	tn := twoTenants(t)
	st := tn.srv.st
	firstID, err := st.FirstProject(tn.ctx, "org_a")
	if err != nil {
		t.Fatalf("FirstProject: %v", err)
	}
	if _, err := st.CreateProject(tn.ctx, store.Project{ID: "project_a", OrgID: "org_a", Name: "Payments"}); err != nil {
		t.Fatal(err)
	}
	remove := func(projectID string) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete,
			httpx.ControlPrefix+"/v1/projects/"+projectID, nil).WithContext(tn.ctx)
		r.SetPathValue("id", projectID)
		tn.srv.deleteProject(w, r, admin("org_a"))
		if w.Code != http.StatusOK {
			t.Fatalf("deleting %s = %d %s, want 200", projectID, w.Code, w.Body)
		}
	}
	issue := func() (*httptest.ResponseRecorder, store.KeyInfo) {
		t.Helper()
		w := invoke(tn.srv.createKey, admin("org_a"), http.MethodPost, "/v1/keys",
			`{"org_id":"org_a","name":"laptop"}`)
		var key store.KeyInfo
		if w.Code == http.StatusCreated {
			if err := json.Unmarshal(w.Body.Bytes(), &key); err != nil {
				t.Fatal(err)
			}
		}
		return w, key
	}

	if _, key := issue(); key.ProjectID != firstID {
		t.Errorf("the key is in %q, want the organisation's first project %q", key.ProjectID, firstID)
	}

	// The fixture's keys are in the first project too, so they go first.
	keys, err := st.KeySummaries(tn.ctx, store.KeyQuery{OrgID: "org_a", Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.RevokedAt == nil {
			if err := st.RevokeKey(tn.ctx, k.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	remove(firstID)
	w, key := issue()
	if w.Code != http.StatusCreated || key.ProjectID != "project_a" {
		t.Errorf("with the first project gone = %d %s, want a key in project_a", w.Code, w.Body)
	}

	if err := st.RevokeKey(tn.ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	remove("project_a")
	if w, _ := issue(); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "create a project first") {
		t.Errorf("a key in an organisation with no projects = %d %s, want a 409 that says to "+
			"create one", w.Code, w.Body)
	}
}

func TestAnotherTenantsPeopleAndGuardrailsAnswerAsMissing(t *testing.T) {
	// As with keys and projects: an administrator elsewhere must not be able to
	// confirm that an id exists by the difference between 403 and 404.
	tn := twoTenants(t)
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}
	dave := map[string]string{"id": "user_dave"}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"changing a role": tn.call(tn.srv.updateUser, carol, http.MethodPatch,
			"/v1/users/user_dave", `{"role":"admin"}`, dave),
		"disabling a person": tn.call(tn.srv.disableUser, carol, http.MethodPost,
			"/v1/users/user_dave/disable", "", dave),
		"setting a guardrail": tn.call(tn.srv.putGuardrails, carol, http.MethodPut,
			"/v1/guardrails/org/org_b", `{"rpm":1}`, map[string]string{"scope": "org", "id": "org_b"}),
	} {
		if w.Code != http.StatusNotFound {
			t.Errorf("%s in another organisation: status = %d, want 404: %s", name, w.Code, w.Body)
		}
	}
}

func TestAnOperatorNamingAMissingOrganisationIsToldSo(t *testing.T) {
	// Only an operator can name an organisation other than their own. A typo
	// there is a missing organisation, not an internal error.
	tn := twoTenants(t)
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}
	w := tn.call(tn.srv.createProject, operator, http.MethodPost, "/v1/projects",
		`{"org_id":"org_nope","name":"Payments"}`, nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no such organisation") {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body)
	}
}

// create calls createKey directly, as revoke calls revokeKey.
func (tn tenants) create(p *authn.Principal, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/keys",
		strings.NewReader(body)).WithContext(tn.ctx)
	tn.srv.createKey(w, r, p)
	return w
}

func TestASubscriptionKeyAlwaysBelongsToSomebody(t *testing.T) {
	tn := twoTenants(t)
	tn.srv.opts.ClaudeSubscriptions = true
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}

	// What an administrator issues for a member to claim with `keera connect
	// claude-code --subscription`.
	w := tn.create(carol, `{"org_id":"org_a","user_id":"user_alice","name":"alice's Claude Code",
		"kind":"subscription"}`)
	var got store.KeyInfo
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &got) != nil ||
		got.Kind != policy.KeySubscription || got.UserID != "user_alice" {
		t.Errorf("member's subscription key: %d %s", w.Code, w.Body)
	}

	// A key the administrator leaves unattributed is for nobody, and a
	// subscription key next to nobody's sign-in is refused.
	if w := tn.create(carol, `{"org_id":"org_a","kind":"subscription"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a subscription key for nobody: %d %s", w.Code, w.Body)
	}
	if w := tn.create(carol, `{"org_id":"org_a","user_id":"user_bob","kind":"pro"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown kind: %d %s", w.Code, w.Body)
	}

	// With subscriptions turned off since, the key it issued gets no secret.
	tn.srv.opts.ClaudeSubscriptions = false
	for verb, handler := range map[string]func(http.ResponseWriter, *http.Request, *authn.Principal){
		"activate": tn.srv.activateKey, "rotate": tn.srv.rotateKey,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, httpx.ControlPrefix+"/v1/keys/"+got.ID+"/"+verb,
			strings.NewReader(`{}`)).WithContext(tn.ctx)
		r.SetPathValue("id", got.ID)
		handler(w, r, carol)
		if w.Code != http.StatusNotImplemented || strings.Contains(w.Body.String(), `"key":`) {
			t.Errorf("%s with subscriptions off: %d %s; want 501 and no secret", verb, w.Code, w.Body)
		}
	}
}

// A member reads the request log, its sessions and the map, but only for the
// requests of their own keys. The filter choices are counted the same way, or
// they would tell a member whose keys were busy.
func TestAMemberSeesOnlyTheTrafficOfTheirOwnKeys(t *testing.T) {
	tn := twoTenants(t)
	now := time.Now()
	if err := tn.srv.st.WriteEvents(tn.ctx, []store.Event{
		{TS: now.Add(-2 * time.Minute), OrgID: "org_a", UserID: "user_alice", KeyID: "key_alice",
			Alias: "keera-code", Client: "claude-code", Status: 200, SessionKey: "p-alice"},
		{TS: now.Add(-time.Minute), OrgID: "org_a", UserID: "user_bob", KeyID: "key_bob",
			Alias: "keera-chat", Client: "codex", Status: 500, SessionKey: "p-bob"},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	alice := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_a", UserID: "user_alice"}
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}

	read := func(h handler, p *authn.Principal, target string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+target, nil).WithContext(tn.ctx)
		path, _, _ := strings.Cut(target, "?")
		r.SetPathValue("id", path[strings.LastIndex(path, "/")+1:])
		h(w, r, p)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	count := func(out map[string]any, field string) int {
		rows, _ := out[field].([]any)
		return len(rows)
	}

	_, log := read(tn.srv.requests, alice, "/v1/requests?facets=1")
	if n := count(log, "data"); n != 1 {
		t.Errorf("alice reads %d requests, want only her own", n)
	}
	facets, _ := log["filters"].(map[string]any)
	if keys, _ := facets["keys"].([]any); len(keys) != 1 {
		t.Errorf("alice is offered %d keys to filter by, want only her own", len(keys))
	}
	if _, all := read(tn.srv.requests, carol, "/v1/requests"); count(all, "data") != 2 {
		t.Errorf("an administrator reads %d requests, want everyone's", count(all, "data"))
	}

	_, sessions := read(tn.srv.sessions, alice, "/v1/sessions")
	if n := count(sessions, "data"); n != 1 {
		t.Fatalf("alice reads %d sessions, want only her own", n)
	}
	var bobs float64
	_, everyone := read(tn.srv.sessions, carol, "/v1/sessions")
	for _, row := range everyone["data"].([]any) {
		if s := row.(map[string]any); s["user_id"] == "user_bob" {
			bobs = s["id"].(float64)
		}
	}
	if bobs == 0 {
		t.Fatal("an administrator does not see bob's session")
	}
	if code, _ := read(tn.srv.session, alice, fmt.Sprintf("/v1/sessions/%d", int64(bobs))); code != http.StatusNotFound {
		t.Errorf("alice opening bob's session got %d, want 404", code)
	}

	_, m := read(tn.srv.trafficMap, alice, "/v1/map")
	if n := count(m, "clients"); n != 1 {
		t.Errorf("alice's map draws %d clients, want only her own", n)
	}
	if gw, _ := m["gateway"].(map[string]any); gw["requests"] != float64(1) {
		t.Errorf("alice's map counts %v requests, want her one", gw["requests"])
	}
}
