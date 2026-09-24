package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// What the control plane refuses to write down.
//
// None of these is what makes the request path safe - a router whose
// destinations cannot be reached simply does not offer them, and one whose
// fallback is nonsense falls back on nothing. They are here because every one
// of them produces a router that looks like it works: the mistake is silent
// afterwards and visible now, which is the only moment it is anybody's to see.

// routerServer is a control plane over a real store, with a real registry
// behind it. The registry is not optional here: writing a router announces the
// change, and announcing it is the gateway's cache being dropped.
func routerServer(ctx context.Context, t *testing.T, st *store.Store) *Server {
	t.Helper()
	reg, err := registry.New(ctx, st, registry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	return New(st, reg, nil, nil, Options{OperatorKey: testOperatorKey, Currency: "CHF"},
		slog.New(slog.DiscardHandler))
}

func routerStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("KEERA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KEERA_TEST_DATABASE_URL to run the control-plane tests that need one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	st, err := store.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `TRUNCATE routers, guardrails, models, api_keys, teams,
		orgs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	for _, m := range []policy.Model{
		{Alias: "keera-small", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "small", Description: "fast and local", Enabled: true},
		{Alias: "keera-large", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "large", Description: "hosted and capable", Enabled: true},
		{Alias: "keera-picker", Kind: policy.KindChat, Backends: []string{"http://x/v1"},
			BackendModel: "picker", Enabled: true},
		{Alias: "keera-embed", Kind: policy.KindEmbedding, Backends: []string{"http://x/v1"},
			BackendModel: "embed", Enabled: true},
	} {
		if err := st.UpsertModel(ctx, m); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
	}
	return st, ctx
}

// putRouter posts one router and returns the status and the body.
func putRouter(t *testing.T, srv *Server, name string, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut,
		httpx.ControlPrefix+"/v1/routers/"+name+"?org_id=org_1", bytes.NewReader(raw))
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

	if code, body := putRouter(t, routerServer(ctx, t, st), "auto",
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

func TestPutRouterRefusesWhatWouldLookLikeItWorked(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		why  string
		edit func(map[string]any)
	}{
		{
			"one destination",
			"a router with nothing to choose between charges a generation per request " +
				"to reach a conclusion that was already written down",
			func(b map[string]any) { b["destinations"] = []string{"keera-small"} },
		},
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
			code, out := putRouter(t, srv, "auto", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 - %s: %s", code, tc.why, out)
			}
		})
	}
}

// A router's name shares the namespace of the catalogue's aliases, because a
// client names both in the same field. The catalogue wins on the request path,
// so a router named over an alias would take no traffic at all - which is a
// mistake worth refusing here rather than leaving to be discovered as a router
// that does nothing.
func TestPutRouterRefusesAnAliasThatIsAlreadyAModel(t *testing.T) {
	st, ctx := routerStore(t)

	code, body := putRouter(t, routerServer(ctx, t, st), "keera-small", validRouter())
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", code, body)
	}
}

func TestPutRouterRefusesANameAClientCouldNotType(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	for _, name := range []string{"Auto", "auto-", "auto.1"} {
		if code, body := putRouter(t, srv, name, validRouter()); code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400: %s", name, code, body)
		}
	}
}

// A router with no fallback refuses what it cannot place, which is a real
// choice and not an incomplete router - so an empty fallback is accepted and
// stored as one.
func TestPutRouterAcceptsNoFallbackAsADeliberateChoice(t *testing.T) {
	st, ctx := routerStore(t)

	body := validRouter()
	body["fallback"] = ""
	if code, out := putRouter(t, routerServer(ctx, t, st), "strict", body); code != http.StatusOK {
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

// Deleting a router is not what takes a client down - the panel cannot see the
// clients - but it is what takes down a scope whose allow-list was narrowed to
// it, which is the arrangement that makes a scope route.
func TestDeleteRouterRefusesOneAnAllowListNarrowsTo(t *testing.T) {
	st, ctx := routerStore(t)

	srv := routerServer(ctx, t, st)
	if code, body := putRouter(t, srv, "auto", validRouter()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	if _, err := st.CreateTeam(ctx, "team_1", "org_1", "Payments Platform"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeTeam, "team_1",
		policy.Limits{AllowedModels: []string{"auto"}}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
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

// ------------------------------------------------------------- fallback mode

func chainBody() map[string]any {
	return map[string]any{
		"mode":         "fallback",
		"destinations": []string{"keera-large", "keera-small"},
		"description":  "The hosted model, with the local one behind it",
	}
}

func TestPutRouterWritesAFallbackRouter(t *testing.T) {
	st, ctx := routerStore(t)

	if code, body := putRouter(t, routerServer(ctx, t, st), "ha",
		chainBody()); code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, body)
	}
	saved, err := st.Router(ctx, "org_1", "ha")
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if saved.Decides() {
		t.Error("the mode was not saved; this router would read every request with a " +
			"model it does not have")
	}
	// The order is the whole of what this router does, so it is the one thing
	// about it that has to survive a write and a read unchanged.
	if len(saved.Destinations) != 2 || saved.Destinations[0] != "keera-large" {
		t.Errorf("destinations = %v, want keera-large first", saved.Destinations)
	}
}

// A fallback router carrying a deciding model, an instruction or a fallback
// destination is refused rather than tidied up. Each of those is a field
// somebody would later read as the reason this router's traffic goes where it
// goes, and none of them would be.
func TestPutRouterRefusesAFallbackRouterThatDecidesSomething(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"a deciding model", func(b map[string]any) { b["model"] = "keera-picker" }},
		{"an instruction", func(b map[string]any) { b["prompt"] = "choose wisely" }},
		{"a fallback destination", func(b map[string]any) { b["fallback"] = "keera-small" }},
		{"one destination", func(b map[string]any) { b["destinations"] = []string{"keera-small"} }},
		{"a destination that cannot answer a chat request", func(b map[string]any) {
			b["destinations"] = []string{"keera-small", "keera-embed"}
		}},
		{"a mode that is neither", func(b map[string]any) { b["mode"] = "shadow" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := chainBody()
			tc.edit(body)
			if code, out := putRouter(t, srv, "ha", body); code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, out)
			}
		})
	}
}

// A router that says nothing about its mode is the router this endpoint
// accepted before there was anything else to be.
func TestPutRouterDefaultsToTheModeThatReadsTheRequest(t *testing.T) {
	st, ctx := routerStore(t)

	if code, body := putRouter(t, routerServer(ctx, t, st), "auto",
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

// ------------------------------------------------------- the measured modes

// A latency or least-busy router is written like a fallback router and refused
// the same things, because it is one - what differs is only what puts its
// destinations in order, which is not something anybody writes down here.
func TestPutRouterWritesAMeasuredRouter(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	for _, mode := range []policy.RouterMode{
		policy.RouterModeLatency, policy.RouterModeLeastBusy,
	} {
		t.Run(string(mode), func(t *testing.T) {
			body := chainBody()
			body["mode"] = string(mode)
			if code, out := putRouter(t, srv, "pool", body); code != http.StatusOK {
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
				t.Error("a measured router was saved as one that reads the request")
			}
			if !saved.Measures() {
				t.Error("the mode was saved and does not measure anything; its " +
					"destinations would be tried in the order they were written, " +
					"which is the mode nobody asked for")
			}
			// Still the tie-break, so it still has to survive the round trip.
			if len(saved.Destinations) != 2 || saved.Destinations[0] != "keera-large" {
				t.Errorf("destinations = %v, want keera-large first", saved.Destinations)
			}
		})
	}
}

func TestPutRouterRefusesAMeasuredRouterThatDecidesSomething(t *testing.T) {
	st, ctx := routerStore(t)
	srv := routerServer(ctx, t, st)

	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"a deciding model", func(b map[string]any) { b["model"] = "keera-picker" }},
		{"an instruction", func(b map[string]any) { b["prompt"] = "choose wisely" }},
		{"a fallback destination", func(b map[string]any) { b["fallback"] = "keera-small" }},
		{"one destination", func(b map[string]any) { b["destinations"] = []string{"keera-small"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := chainBody()
			body["mode"] = string(policy.RouterModeLeastBusy)
			tc.edit(body)
			if code, out := putRouter(t, srv, "pool", body); code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, out)
			}
		})
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

	if code, out := putRouter(t, routerServer(ctx, t, st), "bysize",
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
	// A size router asks no model anything, so it must not come back carrying
	// one: it is what an administrator would otherwise read off the router's
	// screen as the thing choosing its destinations.
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
		{
			"one destination",
			"it would place every request on that model whatever its size",
			func(b map[string]any) {
				b["destinations"] = []string{"keera-small"}
				b["ceilings"] = map[string]int{}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := sizeBody()
			tc.edit(body)
			code, out := putRouter(t, srv, "bysize", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 - %s: %s", code, tc.why, out)
			}
		})
	}
}

func TestPutRouterRefusesCeilingsOnAModeThatCannotReadThem(t *testing.T) {
	// A ceiling on a fallback or instruction router is a number that does
	// nothing, and one somebody would later read as the reason its traffic
	// goes where it goes.
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
			if code, out := putRouter(t, srv, "auto", body); code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", code, out)
			}
		})
	}
}

func TestPutRouterDropsACeilingOfZero(t *testing.T) {
	// Zero and no entry are the same statement - take whatever nothing else
	// will - so a router must not end up with two ways of saying it.
	st, ctx := routerStore(t)
	body := sizeBody()
	body["ceilings"] = map[string]int{"keera-small": 4000, "keera-large": 0}

	if code, out := putRouter(t, routerServer(ctx, t, st), "bysize", body); code != http.StatusOK {
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
