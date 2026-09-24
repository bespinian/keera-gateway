package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The map names other people's clients and every model in the organisation,
// which is the request log's audience and not a member's.
func TestTheMapIsForAdministrators(t *testing.T) {
	s := newServer()
	member := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1"}

	w := httptest.NewRecorder()
	s.trafficMap(w, httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/map", nil), member)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d for a member, want 403", w.Code)
	}
}

// What a model's box says about where it runs, and what it is allowed to say
// about it. An external endpoint is the whole point of the screen and is the
// provider's own published address; an internal one is the inference plane's
// private topology, which the Models screen already keeps to operators.
func TestAModelsBoxNamesTheProviderButNotTheCluster(t *testing.T) {
	admin := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_1"}
	operator := &authn.Principal{Via: authn.MethodOperatorKey}

	inside := policy.Model{Alias: "keera-code", Backends: []string{"http://vllm:8000/v1"}}
	outside := policy.Model{Alias: "keera-frontier", Backends: []string{"https://api.anthropic.com/v1"}}

	if node := modelNode(inside, store.Cell{}, admin); node.Endpoint != "" {
		t.Errorf("an administrator was shown the internal backend %q", node.Endpoint)
	}
	if node := modelNode(inside, store.Cell{}, operator); node.Endpoint != "vllm:8000" {
		t.Errorf("an operator was not shown the backend: %q", node.Endpoint)
	}
	node := modelNode(outside, store.Cell{}, admin)
	if node.Endpoint != "api.anthropic.com" {
		t.Errorf("endpoint = %q; where prompts go is the point of this screen", node.Endpoint)
	}
	if node.Hosting != string(policy.HostedExternal) {
		t.Errorf("hosting = %q, want external", node.Hosting)
	}
}

// The whole screen, against a real database: the catalogue as boxes, the window
// as numbers on them, and the traffic between them as edges.
func TestTheMapDrawsTheCatalogueAndTheTrafficTogether(t *testing.T) {
	st, ctx := streamStore(t)
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	for _, m := range []policy.Model{
		{Alias: "keera-code", Kind: policy.KindChat, Backends: []string{"http://vllm:8000/v1"},
			BackendModel: "served-name", Enabled: true},
		// Enabled, hosted, and deliberately quiet: a way out of the building
		// that nobody used this week is still a way out of the building, and
		// leaving it off the map is the one omission this screen cannot afford.
		{Alias: "keera-frontier", Kind: policy.KindChat,
			Backends:     []string{"https://api.anthropic.com/v1"},
			BackendModel: "claude-opus-5", Enabled: true},
	} {
		if err := st.UpsertModel(ctx, m); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
	}

	now := time.Now()
	if err := st.WriteEvents(ctx, []store.Event{
		{TS: now, OrgID: "org_1", Alias: "keera-code", Client: "claude-code",
			Status: 200, CostMicros: 100, InputTokens: 10, OutputTokens: 5},
		{TS: now, OrgID: "org_1", Alias: "keera-code", Client: "opencode",
			Status: 429},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	srv := New(st, nil, nil, nil, Options{OperatorKey: testOperatorKey, Currency: "CHF"},
		slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+httpx.ControlPrefix+"/v1/map?org_id=org_1&since=24h", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/map: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	var got struct {
		Gateway store.Cell `json:"gateway"`
		Clients []struct {
			Key      string `json:"key"`
			Label    string `json:"label"`
			Requests int64  `json:"requests"`
			Refused  int64  `json:"refused"`
		} `json:"clients"`
		Models []struct {
			Alias    string `json:"alias"`
			Hosting  string `json:"hosting"`
			Endpoint string `json:"endpoint"`
			Requests int64  `json:"requests"`
		} `json:"models"`
		Flows []struct {
			Client string `json:"client"`
			Alias  string `json:"alias"`
		} `json:"flows"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the map: %v", err)
	}

	if got.Gateway.Requests != 2 || got.Gateway.Refused != 1 {
		t.Errorf("gateway = %+v, want two requests one of which was refused", got.Gateway)
	}
	if len(got.Clients) != 2 {
		t.Fatalf("clients = %+v, want the two that called", got.Clients)
	}
	// Busiest first, so a map with more clients than room draws the ones the
	// deployment is actually made of - and each is named the way the Connect
	// screen names it.
	if got.Clients[0].Key != "claude-code" || got.Clients[0].Label != "Claude Code" {
		t.Errorf("the first client is %+v, want Claude Code", got.Clients[0])
	}

	byAlias := map[string]string{}
	for _, m := range got.Models {
		byAlias[m.Alias] = m.Hosting + " " + m.Endpoint
	}
	// The operator key is an operator, so it sees the internal backend too - what
	// an organisation's own administrator is shown instead is the case above.
	if byAlias["keera-code"] != "internal vllm:8000" {
		t.Errorf("keera-code = %q, want it drawn inside the boundary", byAlias["keera-code"])
	}
	if byAlias["keera-frontier"] != "external api.anthropic.com" {
		t.Errorf("keera-frontier = %q, want the provider it forwards to", byAlias["keera-frontier"])
	}
	if len(got.Flows) != 2 {
		t.Errorf("flows = %+v, want one per client and model pair", got.Flows)
	}
}
