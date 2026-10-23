package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Authorization rules that depend on a row, such as whose key it is. Revoking
// has two refusals that differ on purpose: another tenant's key is missing,
// and a colleague's key in your own organisation is forbidden, with a reason.

// tenants is two organisations with people and keys in them.
type tenants struct {
	srv *Server
	ctx context.Context
}

func twoTenants(t *testing.T) tenants {
	t.Helper()
	st, ctx := testStore(t)

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

func TestAnotherTenantsPeopleAndGuardrailsAnswerAsMissing(t *testing.T) {
	// As with keys and projects: an administrator elsewhere must not be able to
	// confirm that an id exists by the difference between 403 and 404.
	tn := twoTenants(t)
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"changing a role": tn.call(tn.srv.updateUser, carol, http.MethodPatch,
			"/v1/users/user_dave", `{"role":"admin"}`),
		"disabling a person": tn.call(tn.srv.disableUser, carol, http.MethodPost,
			"/v1/users/user_dave/disable", ""),
		"setting a guardrail": tn.call(tn.srv.putGuardrails, carol, http.MethodPut,
			"/v1/guardrails/org/org_b", `{"rate_limits":[{"requests":1}]}`),
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
		`{"org_id":"org_nope","name":"Payments"}`)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no such organisation") {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body)
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
		w := tn.call(h, p, http.MethodGet, target, "")
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
