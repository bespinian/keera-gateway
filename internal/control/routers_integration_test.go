package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// routerTenants is twoTenants with what a router in org_a can name: two
// chat models and a filter.
func routerTenants(t *testing.T) tenants {
	t.Helper()
	tn := twoTenants(t)
	for _, alias := range []string{"local", "hosted"} {
		if err := tn.srv.st.UpsertModel(tn.ctx, policy.Model{OrgID: "org_a", Alias: alias,
			Kind: policy.KindChat, Backends: []string{"http://" + alias + ":8000"},
			BackendModel: alias, Location: "ch", Enabled: true}, store.Credential{}); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
	}
	if _, err := tn.srv.st.UpsertFilter(tn.ctx, policy.Filter{OrgID: "org_a", Alias: "redact",
		Model: "local", Mode: policy.FilterModeRewrite, Prompt: "Take out names."}); err != nil {
		t.Fatalf("UpsertFilter: %v", err)
	}
	return tn
}

const redactThenRoute = `{"display_name":"Safe","start":{"id":"redact","kind":"filter","ref":"redact",
	"next":{"id":"route","kind":"route","model":"local","prompt":"Choose.",
		"destinations":["local","hosted"]}}}`

func TestAnAdministratorDrawsARouter(t *testing.T) {
	tn := routerTenants(t)
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	alice := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember,
		OrgID: "org_a", UserID: "user_alice"}

	if w := tn.call(tn.srv.putRouter, alice, "PUT", "/v1/routers/safe", redactThenRoute); w.Code != http.StatusForbidden {
		t.Errorf("a member got %d, want 403", w.Code)
	}
	w := tn.call(tn.srv.putRouter, carol, "PUT", "/v1/routers/safe", redactThenRoute)
	if w.Code != http.StatusOK {
		t.Fatalf("the administrator got %d: %s", w.Code, w.Body)
	}
	var saved SavedRouter
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	// It works, but the route step decides on what the rewrite left.
	if len(saved.Warnings) != 1 || !strings.Contains(saved.Warnings[0], "after the filter 'redact'") {
		t.Errorf("warnings = %q", saved.Warnings)
	}

	// Every member may see it: they name it in their editors.
	lw := tn.call(tn.srv.listRouters, alice, "GET", "/v1/routers", "")
	var list struct {
		Data []policy.Router `json:"data"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil || len(list.Data) != 1 ||
		list.Data[0].Start.Next.Mode != policy.RouteModeInstruction {
		t.Fatalf("list = %s", lw.Body)
	}

	// A budget left without a period is stored as a month, so what is read
	// back says what it does.
	budget := `{"start":{"id":"cap","kind":"budget","budget_micros":1000000,
		"next":{"id":"out","kind":"model","ref":"hosted"}}}`
	if w := tn.call(tn.srv.putRouter, carol, "PUT", "/v1/routers/capped", budget); w.Code != http.StatusOK {
		t.Fatalf("budget router got %d: %s", w.Code, w.Body)
	}
	capped, err := tn.srv.st.Router(tn.ctx, "org_a", "capped")
	if err != nil || capped.Start.Period != policy.PeriodMonth {
		t.Errorf("stored period = %q, %v", capped.Start.Period, err)
	}

	// A key's own screens, the playground among them, offer what it may name,
	// routers included.
	callable, err := tn.srv.callable(httptest.NewRequest("GET", "/", nil).WithContext(tn.ctx), "org_a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(callable, func(m policy.Model) bool { return m.Alias == "safe" }) {
		t.Errorf("callable = %+v, which leaves out the router", callable)
	}
}

func TestARouterMustNameWhatItsOrganisationHas(t *testing.T) {
	tn := routerTenants(t)
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}

	for _, tt := range []struct{ name, body, want string }{
		{"a filter of another name", `{"start":{"id":"a","kind":"filter","ref":"nope",
			"next":{"id":"b","kind":"model","ref":"local"}}}`, "not one of this organisation's filters"},
		{"a branch that ends nowhere", `{"start":{"id":"a","kind":"rate_limit","requests":5}}`,
			"has no 'next' step"},
		{"no steps at all", `{}`, "at least one step"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := tn.call(tn.srv.putRouter, carol, "PUT", "/v1/routers/safe", tt.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tt.want) {
				t.Errorf("got %d %s, want 400 saying %q", w.Code, w.Body, tt.want)
			}
		})
	}

	// A client names models and routers in one field, so they may not share
	// an alias.
	w := tn.call(tn.srv.putRouter, carol, "PUT", "/v1/routers/local", redactThenRoute)
	if w.Code != http.StatusConflict {
		t.Errorf("a router named like a model got %d, want 409", w.Code)
	}
}

// What a router uses cannot be deleted under it: every request through
// that step would be refused.
func TestARouterKeepsWhatItUses(t *testing.T) {
	tn := routerTenants(t)
	carol := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin,
		OrgID: "org_a", UserID: "user_carol"}
	if w := tn.call(tn.srv.putRouter, carol, "PUT", "/v1/routers/safe", redactThenRoute); w.Code != http.StatusOK {
		t.Fatalf("put got %d: %s", w.Code, w.Body)
	}

	for _, tt := range []struct {
		name, path string
		h          handler
		code       string
	}{
		{"redact", "/v1/filters/redact", tn.srv.deleteFilter, "filter_in_use"},
		// A route step sends to it.
		{"hosted", "/v1/models/hosted", tn.srv.deleteModel, "model_in_use"},
	} {
		w := tn.call(tt.h, carol, "DELETE", tt.path, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), tt.code) ||
			!strings.Contains(w.Body.String(), "'safe'") {
			t.Errorf("deleting %s got %d %s, want 409 naming the router", tt.name, w.Code, w.Body)
		}
	}

	// Once the router is gone, so is the reason.
	if w := tn.call(tn.srv.deleteRouter, carol, "DELETE", "/v1/routers/safe", ""); w.Code != http.StatusOK {
		t.Fatalf("deleting the router got %d: %s", w.Code, w.Body)
	}
	if w := tn.call(tn.srv.deleteFilter, carol, "DELETE", "/v1/filters/redact", ""); w.Code != http.StatusOK {
		t.Errorf("deleting the filter afterwards got %d: %s", w.Code, w.Body)
	}
}
