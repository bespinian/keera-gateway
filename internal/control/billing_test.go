package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

var testPlatform = catalog.Platform{"anthropic": {APIKey: "sk-deployment", DiscountBP: 2000}}

// platformRegistry is the registry of a deployment that holds the Anthropic
// key, loaded from st as it is now.
func platformRegistry(ctx context.Context, t *testing.T, st *store.Store) *registry.Registry {
	t.Helper()
	box, err := secret.New("a-secret-key-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(ctx, st, registry.Options{Secrets: box, Platform: testPlatform},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	return reg
}

// platformServer is routerServer on a deployment that holds the Anthropic key.
func platformServer(ctx context.Context, t *testing.T, st *store.Store) *Server {
	t.Helper()
	box, err := secret.New("a-secret-key-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	return New(st, platformRegistry(ctx, t, st), nil, nil, Options{OperatorKey: testOperatorKey,
		Currency: "CHF", Secrets: box, Platform: testPlatform}, slog.New(slog.DiscardHandler))
}

const haiku = `{"provider":"anthropic","backend_model":"claude-haiku-4-5",
	"backends":["https://proxy.example/v1"],"input_micros_per_mtok":1,
	"output_micros_per_mtok":1,"enabled":true}`

func TestAModelOnTheDeploymentsKeyTakesNoKeyAndListPrices(t *testing.T) {
	st, ctx := routerStore(t)
	s := platformServer(ctx, t, st)

	withKey := strings.Replace(haiku, `"enabled"`, `"api_key":"sk-mine","enabled"`, 1)
	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "fast", "", withKey); w.Code != http.StatusBadRequest {
		t.Errorf("a key was accepted: %d %s", w.Code, w.Body)
	}
	w := callModel(s.putModel, admin("org_1"), http.MethodPut, "fast", "", haiku)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var got policy.Model
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	prov, _ := catalog.ProviderByName("anthropic")
	known, _ := prov.Model("claude-haiku-4-5")
	if !got.PlatformKey || got.Backends[0] != prov.Endpoint ||
		got.InputMicrosPerMTok != known.InputMicrosPerMTok {
		t.Errorf("saved = %+v, want the provider's endpoint and list prices", got)
	}

	// The registry sends it with the deployment's key, and bills it.
	m, _ := platformRegistry(ctx, t, st).Model("org_1", "fast")
	if m.APIKey != "sk-deployment" || m.Billing == nil || m.Billing.DiscountBP != 2000 {
		t.Errorf("served = key %q, billing %+v", m.APIKey, m.Billing)
	}

	// A model the price table does not know cannot be billed.
	unknown := strings.Replace(haiku, "claude-haiku-4-5", "claude-from-the-future", 1)
	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "new", "", unknown); w.Code != http.StatusBadRequest {
		t.Errorf("an unpriced model was accepted: %d %s", w.Code, w.Body)
	}
}

// A model saved before the price table lost it is shown as the gateway sends
// it: not on the deployment's key, and without a key of its own.
func TestAnUnpricedModelIsNotShownOnTheDeploymentsKey(t *testing.T) {
	st, ctx := routerStore(t)
	s := platformServer(ctx, t, st)
	if err := st.UpsertModel(ctx, policy.Model{OrgID: "org_1", Alias: "old", Kind: policy.KindChat,
		Provider: "anthropic", BackendModel: "claude-from-the-past", Location: "usa",
		Backends: []string{"https://proxy.example/v1"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelCredential(ctx, "org_1", "old", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	w := invoke(s.listModels, admin("org_1"), http.MethodGet, "/v1/models", "")
	var got struct {
		Data []policy.Model `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("listModels: %v: %s", err, w.Body)
	}
	i := slices.IndexFunc(got.Data, func(m policy.Model) bool { return m.Alias == "old" })
	if i < 0 {
		t.Fatalf("the model is not listed: %s", w.Body)
	}
	if m := got.Data[i]; m.PlatformKey || m.HasAPIKey {
		t.Errorf("shown = platform_key %v, has_api_key %v, want neither", m.PlatformKey, m.HasAPIKey)
	}
}

// A key stored before the deployment had its own is removed, so it can never
// be sent to the provider's endpoint instead of where it was entered for.
func TestSavingAModelOnTheDeploymentsKeyRemovesItsOwn(t *testing.T) {
	st, ctx := routerStore(t)
	s := platformServer(ctx, t, st)
	if err := st.UpsertModel(ctx, policy.Model{OrgID: "org_1", Alias: "fast", Kind: policy.KindChat,
		Provider: "anthropic", BackendModel: "claude-haiku-4-5", Location: "usa",
		Backends: []string{"https://proxy.example/v1"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelCredential(ctx, "org_1", "fast", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if w := callModel(s.putModel, admin("org_1"), http.MethodPut, "fast", "", haiku); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if m, _ := st.Model(ctx, "org_1", "fast"); m.HasAPIKey {
		t.Error("the organisation's own key is still stored")
	}
}

func TestTheProviderListSaysWhichKeysTheDeploymentHolds(t *testing.T) {
	st, ctx := routerStore(t)
	s := platformServer(ctx, t, st)
	w := invoke(s.listProviders, admin("org_1"), http.MethodGet, "/v1/providers", "")
	var out struct {
		Data []struct {
			Name        string `json:"name"`
			PlatformKey bool   `json:"platform_key"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Data {
		if p.PlatformKey != (p.Name == "anthropic") {
			t.Errorf("%s platform_key = %v", p.Name, p.PlatformKey)
		}
	}
}

// Administrators read their own organisation's bill. Only operators see what
// the provider charges, and so the margin.
func TestTheMarginIsForOperatorsOnly(t *testing.T) {
	st, ctx := routerStore(t)
	if _, err := st.Pool().Exec(ctx, "TRUNCATE billing_lines"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_2", Name: "Another Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatal(err)
	}
	s := platformServer(ctx, t, st)
	sept := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	line := policy.BillLine{Alias: "fast", Provider: "anthropic", BackendModel: "claude-haiku-4-5",
		Currency: "USD", InputTokens: 1000, OutputTokens: 100, Micros: 1500, ProviderMicros: 1200}
	events := []store.Event{
		{TS: sept, OrgID: "org_1", Alias: "fast", Status: 200, Bills: []policy.BillLine{line, line}},
		{TS: sept, OrgID: "org_2", Alias: "fast", Status: 200, Bills: []policy.BillLine{line}},
		{TS: sept.AddDate(0, 1, 0), OrgID: "org_1", Alias: "fast", Status: 200,
			Bills: []policy.BillLine{line}},
	}
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	read := func(t *testing.T, w interface{ Result() *http.Response }) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	w := invoke(s.billing, admin("org_1"), http.MethodGet, "/v1/billing?month=2026-09", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "provider_micros") || strings.Contains(w.Body.String(), "margin") {
		t.Errorf("an administrator sees the provider cost: %s", w.Body)
	}
	out := read(t, w)
	rows := out["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want org_1's September only", rows)
	}
	if r := rows[0].(map[string]any); r["org_id"] != "org_1" || r["calls"] != 2.0 || r["micros"] != 3000.0 {
		t.Errorf("row = %v", r)
	}

	if w := invoke(s.billing, admin("org_2"), http.MethodGet, "/v1/billing?org_id=org_1", ""); w.Code != http.StatusForbidden {
		t.Errorf("another tenant read the bill: %d", w.Code)
	}
	if w := invoke(s.billing, member("org_1"), http.MethodGet, "/v1/billing", ""); w.Code != http.StatusForbidden {
		t.Errorf("a member read the bill: %d", w.Code)
	}

	w = invoke(s.billing, operator(), http.MethodGet, "/v1/billing?month=2026-09", "")
	out = read(t, w)
	if rows := out["data"].([]any); len(rows) != 2 {
		t.Fatalf("operator rows = %v, want both organisations", rows)
	}
	total := out["totals"].([]any)[0].(map[string]any)
	if total["micros"] != 4500.0 || total["provider_micros"] != 3600.0 || total["margin_micros"] != 900.0 {
		t.Errorf("total = %v", total)
	}

	w = invoke(s.billing, operator(), http.MethodGet, "/v1/billing?month=2026-09&format=csv", "")
	if !strings.Contains(w.Body.String(), "provider_cost,margin") ||
		!strings.Contains(w.Body.String(), "2026-09,org_1,Example Bank,anthropic,claude-haiku-4-5,USD,2,2000,0,0,200,0.003,0.0024,0.0006") {
		t.Errorf("csv = %s", w.Body)
	}
	if w := invoke(s.billing, operator(), http.MethodGet, "/v1/billing?month=september", ""); w.Code != http.StatusBadRequest {
		t.Errorf("a month that is none = %d", w.Code)
	}
}

func TestThePanelShowsBillingOnlyWithADeploymentKey(t *testing.T) {
	for _, tt := range []struct {
		platform catalog.Platform
		want     bool
	}{{nil, false}, {testPlatform, true}} {
		s := New(nil, nil, nil, nil, Options{OperatorKey: testOperatorKey, Platform: tt.platform},
			slog.New(slog.DiscardHandler))
		w := httptest.NewRecorder()
		s.me(w, httptest.NewRequest(http.MethodGet, "/v1/me", nil), operator())
		var me map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		if me["billing"] != tt.want {
			t.Errorf("with %d provider keys, billing = %v, want %v", len(tt.platform), me["billing"], tt.want)
		}
	}
}
