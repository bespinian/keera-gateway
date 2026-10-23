package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func (tn tenants) revoke(p *authn.Principal, keyID string) *httptest.ResponseRecorder {
	return tn.call(tn.srv.revokeKey, p, http.MethodDelete, "/v1/keys/"+keyID, "")
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
	if err := tn.srv.st.PutGuardrail(tn.ctx, policy.ScopeKey, "key_alice",
		policy.Limits{RateLimits: []policy.RateLimit{{Requests: 120}}}); err != nil {
		t.Fatal(err)
	}
	rotate := func(p *authn.Principal, keyID string) *httptest.ResponseRecorder {
		return tn.call(tn.srv.rotateKey, p, http.MethodPost, "/v1/keys/"+keyID+"/rotate", `{}`)
	}

	if w := rotate(dave, "key_alice"); w.Code != http.StatusNotFound {
		t.Errorf("another tenant rotating her key = %d, want 404", w.Code)
	}
	if w := rotate(alice, "key_bob"); w.Code != http.StatusForbidden {
		t.Errorf("rotating a colleague's key = %d, want 403", w.Code)
	}
	// The lifetime is the administrator's choice, so a member cannot stretch it.
	w := tn.call(tn.srv.rotateKey, alice, http.MethodPost, "/v1/keys/key_alice/rotate",
		`{"expires_in_seconds":315360000}`)
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
	if err != nil || len(lim.RateLimits) != 1 || lim.RateLimits[0].Requests != 120 {
		t.Errorf("the new key's own guardrail = %+v, %v; want 120 requests", lim, err)
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
			`{}`)
	}
	rotate := tn.call(tn.srv.rotateKey, alice, http.MethodPost, "/v1/keys/"+issued.ID+"/rotate",
		`{}`)
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
		return tn.call(tn.srv.renameKey, p, http.MethodPatch, "/v1/keys/"+keyID, body)
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
	// Rotate and activate answer a revoked key the same way.
	if w := tn.revoke(bob, "key_bob"); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "key_revoked") {
		t.Errorf("revoking twice = %d %s, want 409 key_revoked", w.Code, w.Body)
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
	// are different events. The entry records whose key it was, because the
	// user row may later change or be deleted.
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
		w := tn.call(tn.srv.deleteProject, admin("org_a"), http.MethodDelete, "/v1/projects/"+projectID, "")
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
			if err := st.RevokeKey(tn.ctx, store.AnyOrg, k.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	remove(firstID)
	w, key := issue()
	if w.Code != http.StatusCreated || key.ProjectID != "project_a" {
		t.Errorf("with the first project gone = %d %s, want a key in project_a", w.Code, w.Body)
	}

	if err := st.RevokeKey(tn.ctx, store.AnyOrg, key.ID); err != nil {
		t.Fatal(err)
	}
	remove("project_a")
	if w, _ := issue(); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "create a project first") {
		t.Errorf("a key in an organisation with no projects = %d %s, want a 409 that says to "+
			"create one", w.Code, w.Body)
	}
}

// create calls createKey directly, as revoke calls revokeKey.
func (tn tenants) create(p *authn.Principal, body string) *httptest.ResponseRecorder {
	return tn.call(tn.srv.createKey, p, http.MethodPost, "/v1/keys", body)
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
	for verb, h := range map[string]handler{"activate": tn.srv.activateKey, "rotate": tn.srv.rotateKey} {
		w := tn.call(h, carol, http.MethodPost, "/v1/keys/"+got.ID+"/"+verb, `{}`)
		if w.Code != http.StatusNotImplemented || strings.Contains(w.Body.String(), `"key":`) {
			t.Errorf("%s with subscriptions off: %d %s; want 501 and no secret", verb, w.Code, w.Body)
		}
	}
}
