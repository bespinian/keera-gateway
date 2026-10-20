package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// What the control plane refuses to save. The request path would cope with
// each of these, but each makes a router that looks like it works, and saving
// is the only moment the mistake is visible.

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
	st, ctx := testStore(t, "routers", "filters", "guardrails", "models", "api_keys", "projects")
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

func validRouter() map[string]any {
	return map[string]any{
		"model":        "keera-picker",
		"prompt":       "Send short edits to the small model and everything else to the large one.",
		"destinations": []string{"keera-small", "keera-large"},
		"fallback":     "keera-small",
		"description":  "Keeps the large model for the work that needs it",
	}
}

func TestPutRouterWritesAWholeOne(t *testing.T) {
	st, ctx := routerStore(t)

	if code, body := putTo(t, routerServer(ctx, t, st), "/v1/routers/auto",
		validRouter()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	saved, err := st.Router(ctx, "org_1", "auto")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if len(saved.Destinations) != 2 || saved.Fallback != "keera-small" {
		t.Errorf("saved = %+v", saved)
	}
}

// A router with one destination is a name for that model which stays the same
// when the model behind it changes. Every mode takes one.
func TestPutRouterTakesOneDestination(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	modes := append([]policy.RouterMode{policy.RouterModeInstruction, policy.RouterModeSize},
		chainModes...)
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			body := map[string]any{"mode": string(mode), "destinations": []string{"keera-small"}}
			if mode.Decides() {
				body["model"], body["prompt"] = "keera-picker", "Choose one."
			}
			if code, out := putTo(t, srv, "/v1/routers/coding", body); code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", code, out)
			}
			saved, err := st.Router(ctx, "org_1", "coding")
			if err != nil {
				t.Fatalf("Router: %v", err)
			}
			if saved.Decides() {
				t.Errorf("a router with one destination decides: %+v", saved)
			}
		})
	}
	t.Run("no destination", func(t *testing.T) {
		body := map[string]any{"mode": "fallback", "destinations": []string{}}
		if code, out := putTo(t, srv, "/v1/routers/coding", body); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, out)
		}
	})
	t.Run("a deciding model that does not exist", func(t *testing.T) {
		body := map[string]any{"destinations": []string{"keera-small"}, "model": "keera-huge",
			"prompt": "Choose one."}
		if code, out := putTo(t, srv, "/v1/routers/coding", body); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, out)
		}
	})
}

func TestPutRouterRefusesWhatWouldLookLikeItWorked(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		why  string
		edit func(map[string]any)
	}{
		{
			"a destination that is not in the catalogue",
			"it would be dropped from the choice, so the router would silently be " +
				"choosing between fewer models than it says",
			func(b map[string]any) {
				b["destinations"] = []string{"keera-small", "keera-huge"}
			},
		},
		{
			"a destination that cannot answer a chat request",
			"a router is reached on a chat request, so everything it may choose " +
				"between has to be able to answer one",
			func(b map[string]any) {
				b["destinations"] = []string{"keera-small", "keera-embed"}
			},
		},
		{
			"a fallback that is not a destination",
			"the fallback is where a request goes when the decision could not be " +
				"made, so it has to be somewhere this router could send it anyway",
			func(b map[string]any) { b["fallback"] = "keera-embed" },
		},
		{
			"a deciding model that is not a chat model",
			"a router reads text and answers with a name",
			func(b map[string]any) { b["model"] = "keera-embed" },
		},
		{
			"no deciding model",
			"there is nothing to make the decision with",
			func(b map[string]any) { b["model"] = "" },
		},
		{
			"no instruction",
			"the model would be shown a list of destinations on every request with " +
				"nothing to choose between them by",
			func(b map[string]any) { b["prompt"] = "" },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := validRouter()
			tc.edit(body)
			code, out := putTo(t, srv, "/v1/routers/auto", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 - %s: %s", code, tc.why, out)
			}
		})
	}
}

// A client names routers and models in the same field, and the model wins on
// the request path. A router named like a model would take no traffic, so it
// is refused.
func TestPutRouterRefusesAnAliasThatIsAlreadyAModel(t *testing.T) {
	st, ctx := routerStore(t)

	code, body := putTo(t, routerServer(ctx, t, st), "/v1/routers/keera-small", validRouter())
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
}

func TestPutRouterRefusesANameAClientCouldNotType(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	for _, name := range []string{"Auto", "auto-", "auto.1"} {
		if code, body := putTo(t, srv, "/v1/routers/"+name, validRouter()); code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400: %s", name, code, body)
		}
	}
}

// A router with no fallback refuses what it cannot place. That is a real
// choice, so an empty fallback is accepted.
func TestPutRouterAcceptsNoFallbackAsADeliberateChoice(t *testing.T) {
	st, ctx := routerStore(t)

	body := validRouter()
	body["fallback"] = ""
	if code, out := putTo(t, routerServer(ctx, t, st), "/v1/routers/strict", body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	saved, err := st.Router(ctx, "org_1", "strict")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if !saved.Refuses() {
		t.Error("a router saved with no fallback does not refuse")
	}
}

// Deleting a router would leave a scope whose allow-list is narrowed to it
// reaching nothing.
func TestDeleteRouterRefusesOneAnAllowListNarrowsTo(t *testing.T) {
	st, ctx := routerStore(t)

	srv := routerServer(ctx, t, st)
	if code, body := putTo(t, srv, "/v1/routers/auto", validRouter()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_1", OrgID: "org_1", Name: "Payments Platform"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, "project_1",
		policy.Limits{AllowedModels: []string{"auto"}}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, httpx.ControlPrefix+"/v1/routers/auto?org_id=org_1", nil)
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	// And it says which scope, because that is what has to change first.
	if !bytes.Contains(w.Body.Bytes(), []byte("Payments Platform")) {
		t.Errorf("the refusal does not name the scope to change: %s", w.Body.String())
	}
	if _, err := st.Router(ctx, "org_1", "auto"); err != nil {
		t.Errorf("the router was deleted anyway: %v", err)
	}
}

// --------------------------------------------------- the modes that try in turn

func chainBody() map[string]any {
	return map[string]any{
		"mode":         "fallback",
		"destinations": []string{"keera-large", "keera-small"},
		"description":  "The hosted model, with the local one behind it",
	}
}

// The modes that try their destinations in turn: fallback in the written
// order, latency and least-busy in the order the gateway measures. They are
// saved and refused alike.
var chainModes = []policy.RouterMode{
	policy.RouterModeFallback, policy.RouterModeLatency, policy.RouterModeLeastBusy,
}

func TestPutRouterWritesARouterThatTriesItsDestinationsInTurn(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	for _, mode := range chainModes {
		t.Run(string(mode), func(t *testing.T) {
			body := chainBody()
			body["mode"] = string(mode)
			if code, out := putTo(t, srv, "/v1/routers/pool", body); code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", code, out)
			}
			saved, err := st.Router(ctx, "org_1", "pool")
			if err != nil {
				t.Fatalf("Router: %v", err)
			}
			if saved.Mode != mode {
				t.Errorf("mode = %q, want %q", saved.Mode, mode)
			}
			if saved.Decides() {
				t.Error("the mode was not saved; this router would read every request with a " +
					"model it does not have")
			}
			if saved.Measures() != (mode != policy.RouterModeFallback) {
				t.Errorf("Measures() = %v for %s; the destinations would be tried in an "+
					"order nobody asked for", saved.Measures(), mode)
			}
			// The order is a fallback router's whole job and a measured one's
			// tie-break, so it must survive the round trip.
			if len(saved.Destinations) != 2 || saved.Destinations[0] != "keera-large" {
				t.Errorf("destinations = %v, want keera-large first", saved.Destinations)
			}
		})
	}
}

// A router of these modes with a deciding model, an instruction or a fallback
// is refused, not tidied up: somebody would read those fields as what decides
// its traffic.
func TestPutRouterRefusesARouterThatTriesInTurnAndDecidesSomething(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"a deciding model", func(b map[string]any) { b["model"] = "keera-picker" }},
		{"an instruction", func(b map[string]any) { b["prompt"] = "choose wisely" }},
		{"a fallback destination", func(b map[string]any) { b["fallback"] = "keera-small" }},
		{"a destination that cannot answer a chat request", func(b map[string]any) {
			b["destinations"] = []string{"keera-small", "keera-embed"}
		}},
	}
	for _, mode := range chainModes {
		for _, tc := range tests {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				body := chainBody()
				body["mode"] = string(mode)
				tc.edit(body)
				if code, out := putTo(t, srv, "/v1/routers/pool", body); code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", code, out)
				}
			})
		}
	}
	t.Run("a mode that is none of them", func(t *testing.T) {
		body := chainBody()
		body["mode"] = "shadow"
		if code, out := putTo(t, srv, "/v1/routers/pool", body); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, out)
		}
	})
}

// A router without a mode gets the original one.
func TestPutRouterDefaultsToTheModeThatReadsTheRequest(t *testing.T) {
	st, ctx := routerStore(t)

	if code, body := putTo(t, routerServer(ctx, t, st), "/v1/routers/auto",
		validRouter()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	saved, err := st.Router(ctx, "org_1", "auto")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if saved.Mode != policy.RouterModeInstruction {
		t.Errorf("mode = %q, want %q", saved.Mode, policy.RouterModeInstruction)
	}
}

/* --------------------------------------------------------- the size router */

// sizeBody is a whole size router: two destinations, the small one bounded and
// the large one taking what is above it.
func sizeBody() map[string]any {
	return map[string]any{
		"mode":         string(policy.RouterModeSize),
		"destinations": []string{"keera-small", "keera-large"},
		"ceilings":     map[string]int{"keera-small": 4000},
		"description":  "Keeps the large model for the long conversations",
	}
}

func TestPutRouterWritesASizeRouter(t *testing.T) {
	st, ctx := routerStore(t)

	if code, out := putTo(t, routerServer(ctx, t, st), "/v1/routers/bysize",
		sizeBody()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	saved, err := st.Router(ctx, "org_1", "bysize")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if !saved.Sizes() {
		t.Errorf("mode = %q, want a router that places by size", saved.Mode)
	}
	if n, bounded := saved.Ceiling("keera-small"); !bounded || n != 4000 {
		t.Errorf("ceiling = %d, %v; want 4000, true - the ceilings are the router", n, bounded)
	}
	if got := saved.Unbounded(); len(got) != 1 || got[0] != "keera-large" {
		t.Errorf("Unbounded() = %v, want the large model taking what is above every ceiling",
			got)
	}
	// A size router asks no model, so it must not come back with one that an
	// administrator would read as choosing its destinations.
	if saved.Model != "" || saved.Prompt != "" || saved.Fallback != "" {
		t.Errorf("saved = %+v, want nothing that would decide for it", saved)
	}
}

func TestPutRouterRefusesASizeRouterThatCouldNotPlaceEveryRequest(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		why  string
		edit func(map[string]any)
	}{
		{
			"every destination has a ceiling",
			"a request larger than all of them would have nowhere this router meant to " +
				"send it, and that is found out by the first person who sends one",
			func(b map[string]any) {
				b["ceilings"] = map[string]int{"keera-small": 4000, "keera-large": 32000}
			},
		},
		{
			"no destination has a ceiling",
			"the first one would answer everything and the second would never be the " +
				"first choice for anything",
			func(b map[string]any) { b["ceilings"] = map[string]int{} },
		},
		{
			"a ceiling on a model it cannot send to",
			"the likeliest reason is a typo in the alias, which would otherwise read as " +
				"a second destination with no ceiling",
			func(b map[string]any) {
				b["ceilings"] = map[string]int{"keera-smal": 4000, "keera-small": 4000}
			},
		},
		{
			"a deciding model",
			"it reads how long the request is and asks no model anything",
			func(b map[string]any) { b["model"] = "keera-picker" },
		},
		{
			"an instruction",
			"nothing here reads prose, so there is nothing to give an instruction to",
			func(b map[string]any) { b["prompt"] = "choose wisely" },
		},
		{
			"a fallback destination",
			"the next destination up is the fallback, as it is on every mode that tries",
			func(b map[string]any) { b["fallback"] = "keera-small" },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := sizeBody()
			tc.edit(body)
			code, out := putTo(t, srv, "/v1/routers/bysize", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 - %s: %s", code, tc.why, out)
			}
		})
	}
}

func TestPutRouterRefusesCeilingsOnAModeThatCannotReadThem(t *testing.T) {
	// A ceiling on a fallback or instruction router does nothing, but would be
	// read as what decides its traffic.
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	for _, mode := range []policy.RouterMode{
		policy.RouterModeInstruction, policy.RouterModeFallback, policy.RouterModeLeastBusy,
	} {
		t.Run(string(mode), func(t *testing.T) {
			body := validRouter()
			if mode != policy.RouterModeInstruction {
				body = chainBody()
			}
			body["mode"] = string(mode)
			body["ceilings"] = map[string]int{"keera-small": 4000}
			if code, out := putTo(t, srv, "/v1/routers/auto", body); code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, out)
			}
		})
	}
}

func TestPutRouterDropsACeilingOfZero(t *testing.T) {
	// Zero and no entry both mean "take whatever nothing else will", so only
	// one of them is allowed.
	st, ctx := routerStore(t)
	body := sizeBody()
	body["ceilings"] = map[string]int{"keera-small": 4000, "keera-large": 0}

	if code, out := putTo(t, routerServer(ctx, t, st), "/v1/routers/bysize", body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	saved, err := st.Router(ctx, "org_1", "bysize")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if _, bounded := saved.Ceiling("keera-large"); bounded {
		t.Error("a ceiling of zero was stored as a ceiling")
	}
}

func TestADecidingRouterStaysOnTheAllowList(t *testing.T) {
	// The deciding model reads each request before anything is redacted, so
	// one outside the allow-list would receive every request in full.
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	putOrgAllowList := func(models []string) (int, string) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"allowed_models": models})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut,
			httpx.ControlPrefix+"/v1/guardrails/org/org_1", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+testOperatorKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	if code, out := putTo(t, srv, "/v1/routers/auto", validRouter()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	code, out := putOrgAllowList([]string{"keera-small", "keera-large"})
	if code != http.StatusConflict || !strings.Contains(out, "router_model_not_allowed") {
		t.Fatalf("narrowing the list past the deciding model: status = %d, want 409: %s", code, out)
	}

	if code, out := putOrgAllowList([]string{"keera-small", "keera-picker"}); code != http.StatusOK {
		t.Fatalf("a list that keeps the deciding model: status = %d, want 200: %s", code, out)
	}
	body := validRouter()
	body["model"] = "keera-large"
	code, out = putTo(t, srv, "/v1/routers/auto", body)
	if code != http.StatusBadRequest || !strings.Contains(out, "model_not_allowed") {
		t.Fatalf("a deciding model outside the list: status = %d, want 400: %s", code, out)
	}
}

// A router with one destination keeps its deciding model, and decides as soon
// as it is given a second, so the list cannot be narrowed past that model
// either.
func TestAOneDestinationRouterStaysOnTheAllowList(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	body := validRouter()
	body["destinations"] = []string{"keera-small"}
	if code, out := putTo(t, srv, "/v1/routers/solo", body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	code, out := putTo(t, srv, "/v1/guardrails/org/org_1", map[string]any{
		"allowed_models": []string{"keera-small", "keera-large"},
	})
	if code != http.StatusConflict || !strings.Contains(out, "router_model_not_allowed") {
		t.Fatalf("narrowing the list past the deciding model: status = %d, want 409: %s", code, out)
	}
}

// A new router joins the organisation's allow-list as a new model does, or no
// key could call it. But only when the list allows every destination, since
// allowing a router allows them all.
func TestANewRouterJoinsAnAllowListThatCoversItsDestinations(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedModels: []string{"keera-small", "keera-picker"},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	allowed := func() []string {
		t.Helper()
		lim, err := st.GetGuardrail(ctx, policy.ScopeOrg, "org_1")
		if err != nil {
			t.Fatalf("GetGuardrail: %v", err)
		}
		return lim.AllowedModels
	}

	code, out := putTo(t, srv, "/v1/routers/small", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small"},
	})
	if code != http.StatusOK || strings.Contains(out, "warning") {
		t.Fatalf("status = %d, want 200 and no warning: %s", code, out)
	}
	if !slices.Contains(allowed(), "small") {
		t.Errorf("allow-list = %v, want the router on it", allowed())
	}

	code, out = putTo(t, srv, "/v1/routers/both", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small", "keera-large"},
	})
	if code != http.StatusOK || !strings.Contains(out, `"warning":"`) ||
		!strings.Contains(out, "keera-large") {
		t.Fatalf("status = %d, want 200 and a warning naming keera-large: %s", code, out)
	}
	if slices.Contains(allowed(), "both") {
		t.Errorf("allow-list = %v; the router opens keera-large, which it leaves out", allowed())
	}

	// An alias the list names before anything is added under it is allowed
	// already, so the warning says the router opens the model.
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedModels: []string{"keera-small", "keera-picker", "early"},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	code, out = putTo(t, srv, "/v1/routers/early", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small", "keera-large"},
	})
	if code != http.StatusOK || !strings.Contains(out, "can now reach 'keera-large'") {
		t.Fatalf("status = %d, want 200 and a warning that keera-large is open: %s", code, out)
	}

	// Saving a router again leaves the list alone, whatever an administrator
	// did to it in between.
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedModels: []string{"keera-small", "keera-picker"},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if code, out := putTo(t, srv, "/v1/routers/small", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small"},
	}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	if slices.Contains(allowed(), "small") {
		t.Errorf("allow-list = %v; saving a router again put it back", allowed())
	}
}

// A router on the organisation's allow-list opens every destination, so one
// that is changed to send to a model the list leaves out is saved with a
// warning.
func TestARouterOnTheAllowListWarnsWhenItOpensAModel(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedModels: []string{"keera-small", "keera-picker"},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if code, out := putTo(t, srv, "/v1/routers/small", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small"},
	}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}

	code, out := putTo(t, srv, "/v1/routers/small", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small", "keera-large"},
	})
	if code != http.StatusOK || !strings.Contains(out, `"warning":"`) ||
		!strings.Contains(out, "keera-large") {
		t.Fatalf("status = %d, want 200 and a warning naming keera-large: %s", code, out)
	}
}

// Adding a router puts it on the organisation's allow-list, so deleting it
// takes it off again. Only a list that names nothing else keeps it.
func TestDeleteRouterTakesItOffTheOrganisationsList(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	del := func(alias string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete,
			httpx.ControlPrefix+"/v1/routers/"+alias+"?org_id=org_1", nil)
		req.Header.Set("Authorization", "Bearer "+testOperatorKey)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedModels: []string{"keera-small", "keera-picker"},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if code, out := putTo(t, srv, "/v1/routers/small", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small"},
	}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}

	if w := del("small"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	lim, err := st.GetGuardrail(ctx, policy.ScopeOrg, "org_1")
	if err != nil {
		t.Fatalf("GetGuardrail: %v", err)
	}
	if slices.Contains(lim.AllowedModels, "small") {
		t.Errorf("allow-list = %v, want the router gone from it", lim.AllowedModels)
	}

	// A list narrowed to the router would be left reaching nothing.
	if code, out := putTo(t, srv, "/v1/routers/small", map[string]any{
		"mode": "fallback", "destinations": []string{"keera-small"},
	}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, out)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedModels: []string{"small"},
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if w := del("small"); w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestAGuardrailRefusesANegativeLimit(t *testing.T) {
	// A negative budget would be spent before the first request, and a
	// negative rate could never be met, so both are refused rather than stored.
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)
	for _, body := range []string{`{"budget_micros":-5000000}`, `{"rpm":-1}`} {
		req := httptest.NewRequest(http.MethodPut,
			httpx.ControlPrefix+"/v1/guardrails/org/org_1", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testOperatorKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot be negative") {
			t.Errorf("%s: status = %d, want 400: %s", body, w.Code, w.Body.String())
		}
	}
}
