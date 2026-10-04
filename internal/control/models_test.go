package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// callModel runs one model handler for alias, as p.
func callModel(h handler, p *authn.Principal, method, alias, query, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, httpx.ControlPrefix+"/v1/models/"+alias+query,
		strings.NewReader(body))
	r.SetPathValue("alias", alias)
	h(w, r, p)
	return w
}

const ownModel = `{"kind":"chat","backends":["http://10.0.0.7:8000/v1"],"backend_model":"mine",
	"enabled":true}`

// An administrator adds, lists and removes their organisation's own models,
// and nobody outside the organisation sees them.
func TestAnAdministratorManagesTheirOrganisationsOwnModels(t *testing.T) {
	st, ctx := routerStore(t)
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_2", Name: "Another Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	s := routerServer(ctx, t, st)

	if w := callModel(s.putModel, member("org_1"), http.MethodPut, "mine", "", ownModel); w.Code != http.StatusForbidden {
		t.Errorf("a member added a model: %d %s", w.Code, w.Body)
	}
	if w := callModel(s.putModel, admin("org_2"), http.MethodPut, "mine", "?org_id=org_1", ownModel); w.Code != http.StatusForbidden {
		t.Errorf("another tenant's administrator added a model here: %d %s", w.Code, w.Body)
	}
	w := callModel(s.putModel, admin("org_1"), http.MethodPut, "mine", "", ownModel)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	saved, err := st.Model(ctx, "org_1", "mine")
	if err != nil || saved.OrgID != "org_1" {
		t.Fatalf("saved = %+v, %v; want a model of org_1's own", saved, err)
	}
	// Another tenant may use the same alias for its own.
	if w := callModel(s.putModel, admin("org_2"), http.MethodPut, "mine", "", ownModel); w.Code != http.StatusOK {
		t.Errorf("org_2 could not use an alias org_1 has: %d %s", w.Code, w.Body)
	}

	list := func(p *authn.Principal) map[string]policy.Model {
		t.Helper()
		w := invoke(s.listModels, p, http.MethodGet, "/v1/models", "")
		var out struct{ Data []policy.Model }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("listModels: %v: %s", err, w.Body)
		}
		byAlias := map[string]policy.Model{}
		for _, m := range out.Data {
			byAlias[m.OrgID+"/"+m.Alias] = m
		}
		return byAlias
	}
	if m, ok := list(admin("org_1"))["org_1/mine"]; !ok || len(m.Backends) == 0 {
		t.Errorf("the administrator does not see their model's backend: %+v", m)
	}
	if m, ok := list(member("org_1"))["org_1/mine"]; !ok || len(m.Backends) != 0 {
		t.Errorf("a member sees %+v, want the model without its backend", m)
	}
	if _, ok := list(admin("org_2"))["org_1/mine"]; ok {
		t.Error("another tenant sees org_1's model")
	}

	// A router can send to it, and cannot take its name.
	router := func(alias string, destinations ...string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"mode": "fallback", "destinations": destinations})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, httpx.ControlPrefix+"/v1/routers/"+alias,
			strings.NewReader(string(body)))
		r.SetPathValue("alias", alias)
		s.putRouter(w, r, admin("org_1"))
		return w
	}
	if w := router("chain", "mine", "keera-small"); w.Code != http.StatusOK {
		t.Errorf("a router could not send to the organisation's model: %d %s", w.Code, w.Body)
	}
	if w := router("mine", "keera-small", "keera-large"); w.Code != http.StatusConflict {
		t.Errorf("a router took the model's alias: %d %s", w.Code, w.Body)
	}
	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "chain", "", ownModel); w.Code != http.StatusConflict {
		t.Errorf("a model took the router's alias: %d %s", w.Code, w.Body)
	}

	if w := callModel(s.deleteModel, admin("org_1"), http.MethodDelete, "mine", "", ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := st.Model(ctx, "org_2", "mine"); err != nil {
		t.Errorf("removing org_1's model removed org_2's: %v", err)
	}
}

// Every model belongs to an organisation, so an operator names one.
func TestAnOperatorAddsModelsToAnOrganisation(t *testing.T) {
	st, ctx := routerStore(t)
	s := routerServer(ctx, t, st)

	w := callModel(s.putModel, operator(), http.MethodPut, "mine", "", ownModel)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "organisation") {
		t.Errorf("a model of no organisation = %d %s, want 400 asking for one", w.Code, w.Body)
	}
	if w := callModel(s.putModel, operator(), http.MethodPut, "mine", "?org_id=org_1", ownModel); w.Code != http.StatusOK {
		t.Errorf("an operator could not add a model to an organisation: %d %s", w.Code, w.Body)
	}
	if _, err := st.Model(ctx, "org_1", "mine"); err != nil {
		t.Errorf("the model is not org_1's: %v", err)
	}
}

// A new organisation starts with a copy of each template model, and the copy
// is its own to change.
func TestANewOrganisationStartsWithTheTemplate(t *testing.T) {
	st, ctx := routerStore(t)
	reg, err := registry.New(ctx, st, registry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	s := New(st, reg, nil, nil, Options{
		OperatorKey: testOperatorKey,
		Template: store.OrgTemplate{Models: []policy.Model{{
			Alias: "keera-speed", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "speed", Location: policy.LocationOnPrem, Enabled: true,
		}}},
	}, slog.New(slog.DiscardHandler))

	w := invoke(s.createOrg, operator(), http.MethodPost, "/v1/orgs", `{"name":"New Bank"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("createOrg = %d %s", w.Code, w.Body)
	}
	var org struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &org); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Model(ctx, org.ID, "keera-speed"); err != nil {
		t.Fatalf("the new organisation has no keera-speed: %v", err)
	}
	edit := `{"kind":"chat","backends":["http://y/v1"],"backend_model":"speed","enabled":false}`
	if w := callModel(s.putModel, admin(org.ID), http.MethodPut, "keera-speed", "", edit); w.Code != http.StatusOK {
		t.Errorf("the administrator could not change the copy: %d %s", w.Code, w.Body)
	}
}

// A model added without saying "enabled" is served at once, and an edit that
// leaves it out does not switch it on or off behind the operator's back.
func TestAnUnsaidEnabledServesANewModelAndKeepsAnOldOnesState(t *testing.T) {
	st, ctx := routerStore(t)
	s := routerServer(ctx, t, st)
	const unsaid = `{"kind":"chat","backends":["http://10.0.0.7:8000/v1"],"backend_model":"mine"}`
	put := func(body string) policy.Model {
		t.Helper()
		if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "mine", "", body); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
		}
		m, err := st.Model(ctx, "org_1", "mine")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	if !put(unsaid).Enabled {
		t.Error("a new model without 'enabled' started disabled")
	}
	if put(`{"kind":"chat","backends":["http://10.0.0.7:8000/v1"],"backend_model":"mine",
		"enabled":false}`).Enabled {
		t.Error("'enabled': false did not disable the model")
	}
	if put(unsaid).Enabled {
		t.Error("an edit without 'enabled' switched a disabled model on")
	}
}

// A subscription model is sent each caller's Claude sign-in, so it takes no
// key of its own and goes nowhere but Anthropic's own API.
func TestASubscriptionModelIsAnthropicsAPIWithNoKey(t *testing.T) {
	st, ctx := routerStore(t)
	s := routerServer(ctx, t, st)
	const plan = `{"kind":"chat","provider":"anthropic","backends":["https://api.anthropic.com/v1"],
		"backend_model":"claude-opus-5-5","subscription":true,"enabled":true`

	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "claude", "",
		plan+`,"api_key":"sk-ant-api03-x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a subscription model took an API key: %d %s", w.Code, w.Body)
	}
	elsewhere := strings.Replace(plan, "https://api.anthropic.com/v1", "https://collect.example.ch/v1", 1)
	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "claude", "", elsewhere+"}"); w.Code != http.StatusBadRequest {
		t.Errorf("a subscription model was pointed elsewhere: %d %s", w.Code, w.Body)
	}
	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "claude", "", plan+"}"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	saved, err := st.Model(ctx, "org_1", "claude")
	if err != nil || !saved.Subscription {
		t.Errorf("saved = %+v, %v; want a subscription model", saved, err)
	}
}

// Only Claude Code signed in to a Claude plan reaches a subscription model,
// so a model a router uses does not become one: the router would fail.
func TestAModelARouterUsesDoesNotBecomeASubscriptionModel(t *testing.T) {
	st, ctx := routerStore(t)
	s := routerServer(ctx, t, st)
	if code, out := putRouter(t, s, "auto", validRouter()); code != http.StatusOK {
		t.Fatalf("putRouter: %d %s", code, out)
	}
	const plan = `{"kind":"chat","provider":"anthropic","backends":["https://api.anthropic.com/v1"],
		"backend_model":"claude-opus-5-5","subscription":true,"enabled":true}`

	for alias, role := range map[string]string{
		"keera-picker": "deciding model", "keera-large": "destination",
	} {
		w := callModel(s.putModel, admin("org_1"), http.MethodPut, alias, "", plan)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "model_in_use") ||
			!strings.Contains(w.Body.String(), "router 'auto' ("+role+")") {
			t.Errorf("%s: status = %d, want 409 naming the router: %s", alias, w.Code, w.Body)
		}
		saved, err := st.Model(ctx, "org_1", alias)
		if err != nil || saved.Subscription {
			t.Errorf("%s: saved = %+v, %v; want it unchanged", alias, saved, err)
		}
	}
}
