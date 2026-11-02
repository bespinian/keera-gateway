package control

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// locate moves one of org_1's models to loc.
func locate(t *testing.T, st *store.Store, alias, loc string) {
	t.Helper()
	ctx := t.Context()
	m, err := st.Model(ctx, "org_1", alias)
	if err != nil {
		t.Fatal(err)
	}
	m.Location = loc
	if err := st.UpsertModel(ctx, m, store.Credential{}); err != nil {
		t.Fatal(err)
	}
}

func TestAGuardrailAllowsModelsByLocation(t *testing.T) {
	st, ctx := routerStore(t)
	s := routerServer(ctx, t, st)
	locate(t, st, "keera-small", "onprem")
	locate(t, st, "keera-large", "usa")
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_1", OrgID: "org_1", Name: "payments"}); err != nil {
		t.Fatal(err)
	}
	put := func(scope, body string) *http.Response {
		return invoke(s.putGuardrails, operator(), http.MethodPut, "/v1/guardrails/"+scope, body).Result()
	}

	if resp := put("project/project_1", `{"allowed_locations":["Mars!"]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a made-up location: %d", resp.StatusCode)
	}
	// Written as models name theirs, once each.
	if resp := put("project/project_1", `{"allowed_locations":["CH"," onprem","ch"]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("locations: %d", resp.StatusCode)
	}
	lim, _ := st.GetGuardrail(ctx, policy.ScopeProject, "project_1")
	if !slices.Equal(lim.AllowedLocations, []string{"ch", "onprem"}) {
		t.Errorf("stored %v", lim.AllowedLocations)
	}
	w := invoke(s.effectiveGuardrails, operator(), http.MethodGet, "/v1/guardrails/project/project_1/effective", "")
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil || !slices.Equal(eff.AllowedLocations, []string{"ch", "onprem"}) {
		t.Errorf("effective = %s", w.Body)
	}

	// A filter the project runs on a model in the USA would read every
	// request there before redaction.
	if _, err := st.UpsertFilter(ctx, policy.Filter{
		OrgID: "org_1", Alias: "redact", Model: "keera-large",
		Mode: policy.FilterModeRewrite, Prompt: "take credentials out",
	}); err != nil {
		t.Fatal(err)
	}
	w = invoke(s.putGuardrails, operator(), http.MethodPut, "/v1/guardrails/project/project_1",
		`{"allowed_locations":["ch","onprem"],"filters":["redact"]}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "keera-large, in usa") {
		t.Errorf("a filter outside: %d %s", w.Code, w.Body)
	}
	// Inherited from the organisation too.
	if w := invoke(s.putGuardrails, operator(), http.MethodPut, "/v1/guardrails/org/org_1",
		`{"filters":["redact"]}`); w.Code != http.StatusOK {
		t.Fatalf("org filter: %d %s", w.Code, w.Body)
	}
	if resp := put("project/project_1", `{"allowed_locations":["ch","onprem"]}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("an inherited filter outside: %d", resp.StatusCode)
	}
	if resp := put("project/project_1", `{"allowed_locations":["ch","onprem","usa"]}`); resp.StatusCode != http.StatusOK {
		t.Errorf("with its location allowed: %d", resp.StatusCode)
	}

	// An organisation limited to on premises keeps its filters there too.
	if w := invoke(s.putGuardrails, operator(), http.MethodPut, "/v1/guardrails/org/org_1",
		`{"allowed_locations":["onprem"]}`); w.Code != http.StatusOK {
		t.Fatalf("org locations: %d %s", w.Code, w.Body)
	}
	code, out := putTo(t, s, "/v1/filters/strict", map[string]any{
		"model": "keera-large", "mode": "rewrite", "prompt": "take credentials out",
	})
	if code != http.StatusBadRequest || !strings.Contains(out, "runs in usa") {
		t.Errorf("a filter on a model outside: %d %s", code, out)
	}
}
