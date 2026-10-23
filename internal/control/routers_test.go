package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// routerServer is a control plane over a real store and a real registry,
// which writing a router has to notify.
func routerServer(ctx context.Context, t *testing.T, st *store.Store) *Server {
	t.Helper()
	reg, err := registry.New(ctx, st, registry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	return New(st, reg, nil, nil, Options{OperatorKey: testOperatorKey},
		slog.New(slog.DiscardHandler))
}

func routerStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	st, ctx := testStore(t)
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	for _, m := range []policy.Model{
		{OrgID: "org_1", Alias: "keera-small", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "small", Description: "fast and local", Enabled: true},
		{OrgID: "org_1", Alias: "keera-large", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "large", Description: "hosted and capable", Enabled: true},
		{OrgID: "org_1", Alias: "keera-picker", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "picker", Enabled: true},
		{OrgID: "org_1", Alias: "keera-embed", Kind: policy.KindEmbedding, Backends: []string{"http://x/v1"},
			BackendModel: "embed", Enabled: true},
	} {
		if err := st.UpsertModel(ctx, m, store.Credential{}); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
	}
	return st, ctx
}

// putTo puts one router or filter as the operator, into org_1, and returns the
// status and the body.
func putTo(t *testing.T, srv *Server, path string, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut,
		httpx.ControlPrefix+path+"?org_id=org_1", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// validRoute is an instruction step between the small and the large model.
func validRoute() map[string]any {
	return map[string]any{
		"id": "route", "kind": "route",
		"model":        "keera-picker",
		"prompt":       "Send short edits to the small model and everything else to the large one.",
		"destinations": []string{"keera-small", "keera-large"},
		"fallback":     "keera-small",
	}
}

// validRouter is the simplest router: validRoute alone.
func validRouter() map[string]any {
	return map[string]any{"description": "Keeps the large model for the work that needs it",
		"start": validRoute()}
}

func TestPutRouterWritesARouteStep(t *testing.T) {
	st, ctx := routerStore(t)
	body := validRouter()
	// Left out, the mode is written as the default it means.
	delete(body["start"].(map[string]any), "mode")
	if code, out := putTo(t, routerServer(ctx, t, st), "/v1/routers/auto", body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	saved, err := st.Router(ctx, "org_1", "auto")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	routes := saved.Routes()
	if len(routes) != 1 || routes[0].Mode != policy.RouteModeInstruction ||
		len(routes[0].Destinations) != 2 || routes[0].Fallback != "keera-small" {
		t.Errorf("saved = %+v", routes)
	}
}

// Each of these makes a route step that looks like it works, and saving is
// the only moment the mistake is visible. The step's other problems are
// policy's TestRouteStepProblems; the deciding model is checked here.
func TestPutRouterRefusesARouteStepThatWouldLookLikeItWorked(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	for _, tc := range []struct {
		name, want string
		change     func(map[string]any)
	}{
		{"a deciding model that is not a chat model", "reads text and answers with an alias",
			func(s map[string]any) { s["model"] = "keera-embed" }},
		{"a deciding model that does not exist", "model_not_found",
			func(s map[string]any) { s["model"] = "keera-huge" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := validRouter()
			tc.change(body["start"].(map[string]any))
			code, out := putTo(t, srv, "/v1/routers/auto", body)
			if code != http.StatusBadRequest || !strings.Contains(out, tc.want) {
				t.Errorf("status = %d, want 400 saying %q: %s", code, tc.want, out)
			}
		})
	}
}

// A client names a router and a model in the same field, so they cannot
// share an alias.
func TestPutRouterRefusesAnAliasThatIsAlreadyAModel(t *testing.T) {
	st, ctx := routerStore(t)
	code, body := putTo(t, routerServer(ctx, t, st), "/v1/routers/keera-small", validRouter())
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
}

// Every request through the step would be refused, so a model a route step
// uses stays until the step is changed.
func TestDeleteModelARouteStepUsesIsRefused(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	if code, out := putTo(t, srv, "/v1/routers/auto", validRouter()); code != http.StatusOK {
		t.Fatalf("putRouter: %d %s", code, out)
	}
	for _, alias := range []string{"keera-picker", "keera-large"} {
		req := httptest.NewRequest(http.MethodDelete,
			httpx.ControlPrefix+"/v1/models/"+alias+"?org_id=org_1", nil)
		req.Header.Set("Authorization", "Bearer "+testOperatorKey)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "model_in_use") {
			t.Errorf("%s: status = %d, want 409 model_in_use: %s", alias, w.Code, w.Body)
		}
	}
}
