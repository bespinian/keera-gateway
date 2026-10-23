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
	"github.com/bespinian/keera-gateway/internal/store"
)

// What `keera doctor` and `keera guardrail effective` answer, against a real
// database, since both report over its rows. They skip without
// KEERA_TEST_DATABASE_URL; `make test-integration` provides one.

// diagnosed brings up a deployment with one organisation, one project, one key
// and a broken filter.
func diagnosed(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, ctx := testStore(t)
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_a", Name: "Example Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_a", OrgID: "org_a", Name: "Payments"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateKey(ctx, store.KeyInfo{
		ID: "key_a", OrgID: "org_a", ProjectID: "project_a", Name: "a laptop", Prefix: "sk-a",
	}, []byte("hash-of-key_a")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	return New(st, nil, nil, nil, Options{OperatorKey: testOperatorKey}, slog.New(slog.DiscardHandler)), st
}

func diagnose(t *testing.T, srv *Server, p *authn.Principal, query string) Diagnosis {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/diagnostics"+query, nil)
	srv.diagnostics(w, r, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var d Diagnosis
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return d
}

// find looks a check up by name, because the list's order is only for display.
func (d Diagnosis) find(name string) (Check, bool) {
	for _, c := range d.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestDoctorFailsADeploymentThatCanServeNothing(t *testing.T) {
	// An empty catalogue refuses every request, so it is a failure, not a
	// note. Every deployment starts like this.
	srv, _ := diagnosed(t)
	d := diagnose(t, srv, operator(), "?org_id=org_a")

	c, ok := d.find("Catalogue")
	if !ok {
		t.Fatal("no catalogue check")
	}
	if c.Verdict != VerdictFail {
		t.Errorf("catalogue verdict = %q with no models, want fail", c.Verdict)
	}
	if !strings.Contains(c.Fix, "keera model") {
		t.Errorf("fix = %q, want the command that adds one", c.Fix)
	}
	if d.Failures == 0 {
		t.Error("a deployment that can serve nothing must count as a failure")
	}
	if d.Probed {
		t.Error("nothing asked for a probe")
	}
}

func TestDoctorNamesWhatTheEnvironmentIsMissing(t *testing.T) {
	// Warnings, not failures: the deployment runs, but a customer will want
	// each of them.
	srv, _ := diagnosed(t)
	d := diagnose(t, srv, operator(), "")

	for _, name := range []string{"Public URL", "Metrics", "Identity"} {
		c, ok := d.find(name)
		if !ok {
			t.Errorf("no %q check", name)
			continue
		}
		if c.Verdict == VerdictOK {
			t.Errorf("%s = ok with nothing configured", name)
		}
		if c.Fix == "" {
			t.Errorf("%s has no fix; a diagnosis with no next step is not one", name)
		}
	}
}

func TestDoctorTellsAnAdministratorNothingAboutTheDeployment(t *testing.T) {
	// The environment and the inference plane belong to whoever runs the
	// gateway. A tenant's administrator is told only about their organisation.
	srv, _ := diagnosed(t)
	admin := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_a",
	}
	d := diagnose(t, srv, admin, "?org_id=org_a")

	for _, name := range []string{"Public URL", "Metrics", "Identity", "Database"} {
		if _, ok := d.find(name); ok {
			t.Errorf("an administrator was told about %q, which is the deployment's", name)
		}
	}
}

func TestDoctorLetsOnlyTheOrganisationsAdministratorsProbe(t *testing.T) {
	// A probe spends a generation per model, like `keera model check`, so it
	// is for the same people.
	srv, _ := diagnosed(t)
	member := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_m",
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		httpx.ControlPrefix+"/v1/diagnostics?probe=1&org_id=org_a", nil)
	srv.diagnostics(w, r, member)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a member's probe = %d, want 403", w.Code)
	}

	admin := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_a",
	}
	if d := diagnose(t, srv, admin, "?probe=1&org_id=org_a"); !d.Probed {
		t.Error("an administrator's probe did not run")
	}
}

func TestDoctorCatchesAFilterPointingAtNothing(t *testing.T) {
	// A filter fails closed, so one whose model has gone refuses every request
	// it covers. Nothing else says so until the refusals start.
	srv, st := diagnosed(t)
	ctx := t.Context()
	if _, err := st.UpsertFilter(ctx, policy.Filter{
		OrgID: "org_a", Alias: "redact", Model: "a-model-that-is-not-there",
		Mode: policy.FilterModeRewrite, Prompt: "take credentials out",
	}); err != nil {
		t.Fatalf("UpsertFilter: %v", err)
	}
	d := diagnose(t, srv, operator(), "?org_id=org_a")

	c, ok := d.find("redact")
	if !ok {
		t.Fatal("the filter was not checked")
	}
	if c.Verdict != VerdictFail {
		t.Errorf("verdict = %q for a filter that cannot run, want fail", c.Verdict)
	}
	if !strings.Contains(c.Detail, "refuses every request") {
		t.Errorf("detail = %q, want it to say what a filter that cannot run does", c.Detail)
	}
}

func TestDoctorCatchesARouterModelStepPointingAtNothing(t *testing.T) {
	// Every request that reaches the step fails, and the router itself looks
	// fine in the list.
	srv, st := diagnosed(t)
	ctx := t.Context()
	if _, err := st.UpsertRouter(ctx, policy.Router{
		OrgID: "org_a", Alias: "auto",
		Start: &policy.Step{ID: "answer", Kind: policy.StepModel, Ref: "a-model-that-is-not-there"},
	}); err != nil {
		t.Fatalf("UpsertRouter: %v", err)
	}
	d := diagnose(t, srv, operator(), "?org_id=org_a")

	c, ok := d.find("auto / answer")
	if !ok {
		t.Fatal("the model step was not checked")
	}
	if c.Verdict != VerdictFail || !strings.Contains(c.Detail, "a-model-that-is-not-there") {
		t.Errorf("check = %+v, want a failure naming the model", c)
	}
}

func TestEffectiveGuardrailsCollapseTheChain(t *testing.T) {
	// What holds this key, though it sets nothing itself.
	srv, st := diagnosed(t)
	ctx := t.Context()

	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_a", policy.Limits{
		RateLimits:    []policy.RateLimit{{Requests: 600, Period: policy.PeriodMinute}},
		Budgets:       []policy.Budget{{BudgetMicros: 500_000_000, Period: policy.PeriodMonth}},
		AllowedModels: []string{"keera-speed", "keera-frontier"},
	}); err != nil {
		t.Fatalf("PutGuardrail org: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, "project_a", policy.Limits{
		RateLimits:    []policy.RateLimit{{Requests: 120, Period: policy.PeriodMinute}},
		AllowedModels: []string{"keera-speed"},
	}); err != nil {
		t.Fatalf("PutGuardrail project: %v", err)
	}

	w := serve(ctx, srv.effectiveGuardrails, operator(), http.MethodGet,
		"/v1/guardrails/key/key_a/effective", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	// The allow-list intersects, so the project's narrower one wins.
	if len(eff.AllowedModels) != 1 || eff.AllowedModels[0] != "keera-speed" {
		t.Errorf("allowed models = %v, want the intersection", eff.AllowedModels)
	}
	// Every level is reported, so somebody can see which one to change.
	if len(eff.Levels) != 3 {
		t.Fatalf("levels = %d, want org, project and key", len(eff.Levels))
	}
	if eff.Levels[0].Name != "Example Bank" || eff.Levels[1].Name != "Payments" {
		t.Errorf("levels = %+v, want them named", eff.Levels)
	}
	// Rate limits are enforced per level rather than merged, so both are here
	// and the tightest is the project's.
	var tightest int64
	for _, s := range eff.Scopes {
		if n := int64(s.RPM); n > 0 && (tightest == 0 || n < tightest) {
			tightest = n
		}
	}
	if tightest != 120 {
		t.Errorf("tightest rpm = %d, want the project's 120", tightest)
	}
	// Budgets do not merge either: the organisation's is the one that holds,
	// and it is reported against the organisation and not against the key.
	found := false
	for _, s := range eff.Scopes {
		if s.Type == policy.ScopeOrg && s.BudgetMicros == 500_000_000 {
			found = true
		}
	}
	if !found {
		t.Errorf("scopes = %+v, want the organisation's budget on its own level", eff.Scopes)
	}
}

func TestEffectiveGuardrailsOnAProjectStopAtTheProject(t *testing.T) {
	// Asking about a project means every key in it, so the answer has no
	// trailing key level without an id.
	srv, _ := diagnosed(t)
	w := serve(t.Context(), srv.effectiveGuardrails, operator(), http.MethodGet,
		"/v1/guardrails/project/project_a/effective", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(eff.Levels) != 2 {
		t.Errorf("levels = %d, want the organisation and the project", len(eff.Levels))
	}
	for _, s := range eff.Scopes {
		if s.Type == policy.ScopeKey {
			t.Error("a project's effective guardrails named a key scope")
		}
	}
}

// A scope that does not exist is missing for an operator too, the same as on
// its effective view, rather than an empty guardrail.
func TestAGuardrailOfAScopeThatDoesNotExistIsMissing(t *testing.T) {
	srv, _ := diagnosed(t)
	w := serve(t.Context(), srv.getGuardrails, operator(), http.MethodGet,
		"/v1/guardrails/project/project_gone", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body)
	}
}
