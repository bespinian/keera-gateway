package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// agentsGateway is twoTenants behind a whole control server that takes
// Keera Agents' service key.
type agentsGateway struct {
	t   *testing.T
	st  *store.Store
	ctx context.Context
	url string
}

func newAgentsGateway(t *testing.T) agentsGateway {
	t.Helper()
	tn := twoTenants(t)
	srv := New(tn.srv.st, tn.srv.reg, nil, nil, Options{
		OperatorKey: testOperatorKey, AgentsServiceKey: testServiceKey,
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return agentsGateway{t: t, st: tn.srv.st, ctx: tn.ctx, url: ts.URL}
}

// send calls a route under the control prefix with the bearer credential
// given, and decodes a JSON answer into out when it is not nil.
func (g agentsGateway) send(credential, method, path, body string, out any) (int, string) {
	g.t.Helper()
	req, err := http.NewRequestWithContext(g.ctx, method, g.url+httpx.ControlPrefix+path,
		strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			g.t.Fatalf("%s %s: %v in %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

func (g agentsGateway) service(method, path, body string, out any) (int, string) {
	g.t.Helper()
	return g.send(testServiceKey, method, path, body, out)
}

type issuedKey struct {
	store.KeyInfo
	Key string `json:"key"`
}

// A key issued for an agent is the person's: their project, their name in
// the audit log's detail, and a secret that works at once. The issuer is
// Keera Agents, and the log says so.
func TestKeeraAgentsIssuesAKeyInAPersonsName(t *testing.T) {
	g := newAgentsGateway(t)
	var k issuedKey
	code, body := g.service("POST", "/v1/service/keys",
		`{"org_id":"org_a","user_id":"user_alice","name":"alice's agent"}`, &k)
	if code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", code, body)
	}
	oldest, err := g.st.FirstProject(g.ctx, "org_a")
	if err != nil {
		t.Fatal(err)
	}
	if k.Key == "" || k.IssuedBy != store.IssuedByAgents || k.UserID != "user_alice" ||
		k.ProjectID != oldest || k.Kind != policy.KeyStandard || k.Prefix == "" {
		t.Errorf("issued %+v; want alice's standard key in the oldest project, marked agents", k)
	}
	res, err := g.st.LookupKey(g.ctx, auth.Hash(k.Key))
	if err != nil || res.Key.ID != k.ID {
		t.Fatalf("the new key does not work: %v", err)
	}
	// Without a budget it has no guardrail of its own.
	if _, err := g.st.GetGuardrail(g.ctx, policy.ScopeKey, k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a key without a budget has a guardrail: %v", err)
	}
	entries, err := g.st.ListAudit(g.ctx, store.AuditQuery{OrgID: "org_a", Action: "key.create"})
	if err != nil || len(entries) != 1 || entries[0].Actor != authn.ServiceActor {
		t.Fatalf("audit = %+v, %v; want one key.create by keera-agents", entries, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(entries[0].Detail, &detail); err != nil ||
		detail["issued_by"] != "agents" || detail["user_id"] != "user_alice" {
		t.Errorf("audit detail = %s, %v", entries[0].Detail, err)
	}
}

// A budget becomes a monthly budget on the key itself, so one agent cannot
// spend the person's whole allowance.
func TestAnAgentsKeyCarriesItsOwnBudget(t *testing.T) {
	g := newAgentsGateway(t)
	var k issuedKey
	code, body := g.service("POST", "/v1/service/keys",
		`{"org_id":"org_a","user_id":"user_alice","name":"capped","budget_micros":5000000}`, &k)
	if code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", code, body)
	}
	lim, err := g.st.GetGuardrail(g.ctx, policy.ScopeKey, k.ID)
	if err != nil || len(lim.Budgets) != 1 || lim.Budgets[0].BudgetMicros != 5_000_000 ||
		lim.Budgets[0].Period != policy.PeriodMonth {
		t.Errorf("guardrail = %+v, %v; want CHF 5 a month", lim, err)
	}
	res, err := g.st.LookupKey(g.ctx, auth.Hash(k.Key))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sc := range res.Scopes {
		if sc.Type == policy.ScopeKey && sc.BudgetMicros == 5_000_000 {
			found = true
		}
	}
	if !found {
		t.Errorf("the gateway does not see the key's budget: %+v", res.Scopes)
	}
}

// A key is refused for somebody outside the organisation, somebody disabled,
// or on another organisation's project, with a reason Keera Agents can log.
func TestAKeyForAnAgentIsRefusedWhereItWouldNotBelong(t *testing.T) {
	g := newAgentsGateway(t)
	if _, err := g.st.DisableUser(g.ctx, "org_a", "user_bob"); err != nil {
		t.Fatal(err)
	}
	theirs, err := g.st.FirstProject(g.ctx, "org_b")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		code       int
		says       string
	}{
		{"no organisation", `{"user_id":"user_alice","name":"x"}`, 400, "org_required"},
		{"an unknown organisation", `{"org_id":"org_x","user_id":"user_alice","name":"x"}`, 404, "no organisation"},
		{"no person", `{"org_id":"org_a","name":"x"}`, 400, "user_id"},
		{"no name", `{"org_id":"org_a","user_id":"user_alice","name":" "}`, 400, "name"},
		{"a negative budget", `{"org_id":"org_a","user_id":"user_alice","name":"x","budget_micros":-1}`, 400, "negative"},
		{"another organisation's person", `{"org_id":"org_a","user_id":"user_dave","name":"x"}`, 404, "nobody"},
		{"a disabled person", `{"org_id":"org_a","user_id":"user_bob","name":"x"}`, 403, "disabled"},
		{"another organisation's project", `{"org_id":"org_a","user_id":"user_alice","project_id":"` + theirs + `","name":"x"}`, 404, "no project"},
		{"a field it does not know", `{"org_id":"org_a","user_id":"user_alice","name":"x","kind":"subscription"}`, 400, "kind"},
	} {
		code, body := g.service("POST", "/v1/service/keys", tc.body, nil)
		if code != tc.code || !strings.Contains(body, tc.says) {
			t.Errorf("%s: %d %s; want %d saying %q", tc.name, code, body, tc.code, tc.says)
		}
	}
}

// Keera Agents can take back the keys it issued, and only those. People can
// revoke them too, as a kill switch, but cannot rotate them.
func TestOnlyKeeraAgentsKeysAreItsToRevoke(t *testing.T) {
	g := newAgentsGateway(t)
	var k issuedKey
	if code, body := g.service("POST", "/v1/service/keys",
		`{"org_id":"org_a","user_id":"user_alice","name":"agent"}`, &k); code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", code, body)
	}

	for name, path := range map[string]string{
		"a person's key":            "/v1/service/keys/key_alice?org_id=org_a",
		"its key, named elsewhere":  "/v1/service/keys/" + k.ID + "?org_id=org_b",
		"its key, with no org":      "/v1/service/keys/" + k.ID,
		"a key that does not exist": "/v1/service/keys/key_nope?org_id=org_a",
	} {
		if code, body := g.service("DELETE", path, "", nil); code != http.StatusNotFound &&
			code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want it refused", name, code, body)
		}
	}
	if _, err := g.st.LookupKeyByID(g.ctx, "key_alice"); err != nil {
		t.Errorf("alice's own key stopped working: %v", err)
	}

	// An administrator cannot rotate it: Keera Agents holds the only copy.
	code, body := g.send(testOperatorKey, "POST", "/v1/keys/"+k.ID+"/rotate", "{}", nil)
	if code != http.StatusConflict || !strings.Contains(body, "agents_key") {
		t.Errorf("rotating an agent's key: %d %s, want 409 agents_key", code, body)
	}

	if code, body := g.service("DELETE", "/v1/service/keys/"+k.ID+"?org_id=org_a", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoking its own key: %d %s", code, body)
	}
	if _, err := g.st.LookupKeyByID(g.ctx, k.ID); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("after revoking: %v, want it revoked", err)
	}
	// Again is not an error: the key is gone either way.
	if code, body := g.service("DELETE", "/v1/service/keys/"+k.ID+"?org_id=org_a", "", nil); code != http.StatusNoContent {
		t.Errorf("revoking twice: %d %s, want 204", code, body)
	}
}

// An administrator can revoke a key Keera Agents issued, from the panel.
func TestPeopleCanRevokeAnAgentsKey(t *testing.T) {
	g := newAgentsGateway(t)
	var k issuedKey
	if code, body := g.service("POST", "/v1/service/keys",
		`{"org_id":"org_a","user_id":"user_alice","name":"agent"}`, &k); code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", code, body)
	}
	if code, body := g.send(testOperatorKey, "DELETE", "/v1/keys/"+k.ID, "", nil); code != http.StatusOK {
		t.Fatalf("revoking it as an operator: %d %s", code, body)
	}
	if _, err := g.st.LookupKeyByID(g.ctx, k.ID); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("after revoking: %v, want it revoked", err)
	}
}

// When the person an agent works for is disabled, the agent's key stops
// working, and Keera Agents can see why.
func TestAnAgentsKeyEndsWithItsPerson(t *testing.T) {
	g := newAgentsGateway(t)
	var k issuedKey
	if code, body := g.service("POST", "/v1/service/keys",
		`{"org_id":"org_a","user_id":"user_alice","name":"agent"}`, &k); code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", code, body)
	}
	if _, err := g.st.DisableUser(g.ctx, "org_a", "user_alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.LookupKey(g.ctx, auth.Hash(k.Key)); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("the key of a disabled person: %v, want it revoked", err)
	}
	var u struct {
		Disabled bool `json:"disabled"`
	}
	if code, body := g.service("GET", "/v1/service/users?org_id=org_a&id=user_alice", "", &u); code != http.StatusOK || !u.Disabled {
		t.Errorf("looking alice up: %d %s, want her disabled", code, body)
	}
}

// A chat app may spell an address in another case than the directory. The
// person is found in the organisation asked about, and nowhere else.
func TestKeeraAgentsFindsThePersonBehindAnAddress(t *testing.T) {
	g := newAgentsGateway(t)
	var u struct {
		ID, Email, Name, Role string
		Disabled              bool
	}
	code, body := g.service("GET", "/v1/service/users?org_id=org_a&email=Carol@Example.CH", "", &u)
	if code != http.StatusOK || u.ID != "user_carol" || u.Role != "admin" || u.Disabled {
		t.Errorf("by address: %d %s", code, body)
	}
	code, body = g.service("GET", "/v1/service/users?org_id=org_a&id=user_alice", "", &u)
	if code != http.StatusOK || u.Email != "alice@example.ch" {
		t.Errorf("by id: %d %s", code, body)
	}
	for name, path := range map[string]string{
		"another organisation's person": "/v1/service/users?org_id=org_a&email=dave@another.example.ch",
		"by id, elsewhere":              "/v1/service/users?org_id=org_b&id=user_alice",
		"an unknown address":            "/v1/service/users?org_id=org_a&email=nobody@example.ch",
	} {
		if code, body := g.service("GET", path, "", nil); code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404", name, code, body)
		}
	}
	for name, path := range map[string]string{
		"both":            "/v1/service/users?org_id=org_a&email=a@example.ch&id=user_alice",
		"neither":         "/v1/service/users?org_id=org_a",
		"no organisation": "/v1/service/users?email=alice@example.ch",
	} {
		if code, body := g.service("GET", path, "", nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, body)
		}
	}
}

// The catalogue follows the project's guardrails, and names the project it
// was read for.
func TestTheServiceCatalogueFollowsTheProjectsGuardrails(t *testing.T) {
	g := newAgentsGateway(t)
	for _, m := range []policy.Model{
		{OrgID: "org_a", Alias: "keera-code", Kind: policy.KindChat, Enabled: true, Backends: []string{"http://x"}},
		{OrgID: "org_a", Alias: "keera-speed", Kind: policy.KindChat, Enabled: true, Backends: []string{"http://x"}},
	} {
		if err := g.st.UpsertModel(g.ctx, m, store.Credential{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.st.UpsertMCPServer(g.ctx, policy.MCPServer{
		OrgID: "org_a", Alias: "github", URL: "https://mcp.example.ch", Enabled: true,
	}, store.Credential{}); err != nil {
		t.Fatal(err)
	}
	narrow, err := g.st.CreateProject(g.ctx, store.Project{ID: "project_narrow", OrgID: "org_a", Name: "narrow"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.st.PutGuardrail(g.ctx, policy.ScopeProject, narrow.ID, policy.Limits{
		AllowedModels: []string{"keera-code"}, AllowedTools: []string{},
	}); err != nil {
		t.Fatal(err)
	}

	var all, few serviceCatalogue
	if code, body := g.service("GET", "/v1/service/catalog?org_id=org_a", "", &all); code != http.StatusOK {
		t.Fatalf("the oldest project: %d %s", code, body)
	}
	oldest, _ := g.st.FirstProject(g.ctx, "org_a")
	if all.ProjectID != oldest || len(all.Projects) != 2 || len(all.Models) != 2 ||
		len(all.MCPServers) != 1 {
		t.Errorf("the oldest project's catalogue = %+v", all)
	}
	if code, body := g.service("GET", "/v1/service/catalog?org_id=org_a&project_id=project_narrow", "", &few); code != http.StatusOK {
		t.Fatalf("the narrow project: %d %s", code, body)
	}
	if few.ProjectID != "project_narrow" || len(few.Models) != 1 || few.Models[0].Alias != "keera-code" ||
		len(few.MCPServers) != 0 {
		t.Errorf("the narrow project's catalogue = %+v", few)
	}
	if code, body := g.service("GET", "/v1/service/catalog?org_id=org_b&project_id=project_narrow", "", nil); code != http.StatusNotFound {
		t.Errorf("a project named in another organisation: %d %s, want 404", code, body)
	}
}
