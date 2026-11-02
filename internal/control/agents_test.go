package control

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

const testServiceKey = "a-keera-agents-service-key-long-enough"

func agentsServer(key string) *Server {
	return New(nil, nil, nil, nil, Options{OperatorKey: testOperatorKey, AgentsServiceKey: key},
		slog.New(slog.DiscardHandler))
}

// The service routes take the service key and nothing else, and the service
// key opens nothing else. Either way round, a credential would reach more
// than it was made for.
func TestTheServiceKeyAndTheServiceRoutesOnlyFitEachOther(t *testing.T) {
	s := agentsServer(testServiceKey)
	var seen *authn.Principal
	h := s.serviceAuthenticated(echo(&seen))
	for name, header := range map[string]string{
		"nothing":          "",
		"the operator key": "Bearer " + testOperatorKey,
		"a CLI token":      "Bearer " + authn.CLITokenPrefix + "abc",
		"a wrong key":      "Bearer " + strings.Repeat("x", len(testServiceKey)),
		"a prefix of it":   "Bearer " + testServiceKey[:20],
	} {
		r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/service/users", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || seen != nil {
			t.Errorf("%s on a service route: %d, ran=%v; want 401", name, w.Code, seen != nil)
		}
	}

	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/service/users", nil)
	r.Header.Set("Authorization", "Bearer "+testServiceKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || seen == nil || seen.Via != authn.MethodService {
		t.Fatalf("the service key: %d, principal %+v; want it through as Keera Agents", w.Code, seen)
	}

	// On any other route it is an unknown bearer token.
	seen = nil
	r = httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/orgs", nil)
	r.Header.Set("Authorization", "Bearer "+testServiceKey)
	w = httptest.NewRecorder()
	s.authenticated(echo(&seen)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || seen != nil {
		t.Errorf("the service key on an ordinary route: %d; want 401", w.Code)
	}
}

// Without a service key the routes say the feature is off, whatever is sent.
func TestServiceRoutesAreOffWithoutAServiceKey(t *testing.T) {
	h := agentsServer("").Handler()
	for _, header := range []string{"", "Bearer " + testOperatorKey, "Bearer "} {
		r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/service/catalog?org_id=x", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "agents_disabled") {
			t.Errorf("%q: %d %s; want 501 agents_disabled", header, w.Code, w.Body)
		}
	}
}

// Each handler checks its caller too, so one registered as an ordinary route
// by mistake does not open to the operator key.
func TestServiceHandlersRefuseAnyoneButKeeraAgents(t *testing.T) {
	s := agentsServer(testServiceKey)
	operator := &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}
	for name, h := range map[string]handler{
		"create": s.serviceCreateKey, "revoke": s.serviceRevokeKey,
		"user": s.serviceUser, "catalog": s.serviceCatalog,
	} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/", nil), operator)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s as the operator key: %d, want 403", name, w.Code)
		}
	}
}

// The catalogue offers what a key in the project may use: enabled chat
// models and routers the guardrails allow, and enabled MCP servers they allow.
func TestTheCatalogueIsWhatAKeyInTheProjectMayUse(t *testing.T) {
	org := policy.Limits{
		AllowedModels: []string{"keera-code", "keera-speed", "safe"},
		AllowedTools:  []string{"github/search_code", "docs"},
	}
	project := policy.Limits{AllowedModels: []string{"keera-code", "safe"}}
	res := policy.Resolve(policy.Key{OrgID: "org_a", ProjectID: "project_a", Kind: policy.KeyStandard},
		&org, &project, nil)
	catalogue := []policy.Model{
		{Alias: "keera-code", Kind: policy.KindChat, Enabled: true},
		// Allowed by the organisation, not by the project.
		{Alias: "keera-speed", Kind: policy.KindChat, Enabled: true},
		{Alias: "embed", Kind: policy.KindEmbedding, Enabled: true},
		{Alias: "claude", Kind: policy.KindChat, Enabled: true, Subscription: true},
	}
	routers := []policy.Router{{Alias: "safe"}, {Alias: "other"}}
	servers := []policy.MCPServer{
		{Alias: "github", Enabled: true},
		{Alias: "docs", Enabled: false},
		{Alias: "jira", Enabled: true},
	}
	projects := []store.Project{{ID: "project_a", Name: "default"}, {ID: "project_b", Name: "other"}}

	got := catalogueOf("project_a", projects, res, catalogue, routers, servers)
	if got.ProjectID != "project_a" || len(got.Projects) != 2 || got.Projects[1].Name != "other" {
		t.Errorf("projects = %q %+v", got.ProjectID, got.Projects)
	}
	want := []serviceModel{{"keera-code", "model"}, {"safe", "router"}}
	if !slices.Equal(got.Models, want) {
		t.Errorf("models = %+v, want %+v", got.Models, want)
	}
	if len(got.MCPServers) != 1 || got.MCPServers[0].Alias != "github" {
		t.Errorf("MCP servers = %+v, want only github", got.MCPServers)
	}

	// Nothing allowed is an empty list, not null, so a client can range it.
	none := catalogueOf("project_a", projects,
		policy.Resolve(policy.Key{Kind: policy.KeyStandard}, &policy.Limits{AllowedModels: []string{}}, nil, nil),
		catalogue, nil, nil)
	if none.Models == nil || none.MCPServers == nil || len(none.Models) != 0 {
		t.Errorf("an empty catalogue = %+v", none)
	}
}
