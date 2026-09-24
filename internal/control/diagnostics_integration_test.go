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
// database. Both are reports over rows in three tables, so there is nothing
// left of either of them without one.
//
// These skip without KEERA_TEST_DATABASE_URL, as the rest of this package's
// integration tests do; `make test-integration` provides one.

// diagnosed brings up a deployment with one organisation, one team, one key
// and a broken filter in it, and returns the server.
func diagnosed(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, ctx := streamStore(t)
	if _, err := st.Pool().Exec(ctx,
		"TRUNCATE filters, routers, guardrails RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("emptying the tables: %v", err)
	}
	if _, err := st.CreateOrg(ctx, "org_a", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateTeam(ctx, "team_a", "org_a", "Payments"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := st.CreateKey(ctx, store.KeyInfo{
		ID: "key_a", OrgID: "org_a", TeamID: "team_a", Alias: "a laptop", Prefix: "sk-a",
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

// find returns the check with this name, so a test names what it is about
// rather than indexing into a list whose order is a rendering decision.
func (d Diagnosis) find(name string) (Check, bool) {
	for _, c := range d.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestDoctorFailsADeploymentThatCanServeNothing(t *testing.T) {
	// An empty catalogue is the whole deployment refused, so it is a failure
	// and not something to mention. This is the state every deployment starts
	// in, and the first thing somebody runs doctor to be told.
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
	// The three that are warnings rather than failures: the deployment runs,
	// and a customer will want each of them.
	srv, _ := diagnosed(t)
	d := diagnose(t, srv, operator(), "")

	for _, name := range []string{"Public URL", "Secret key", "Metrics", "Identity"} {
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
	// The environment variables and the inference plane belong to whoever runs
	// the gateway. An administrator of one tenant is told about their own
	// organisation and not about the host it is on.
	srv, _ := diagnosed(t)
	admin := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_a",
	}
	d := diagnose(t, srv, admin, "?org_id=org_a")

	for _, name := range []string{"Public URL", "Secret key", "Metrics", "Identity", "Database"} {
		if _, ok := d.find(name); ok {
			t.Errorf("an administrator was told about %q, which is the deployment's", name)
		}
	}
}

func TestDoctorRefusesAProbeToAnyoneButAnOperator(t *testing.T) {
	// A probe reaches the inference plane and spends a generation per model.
	srv, _ := diagnosed(t)
	admin := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_a",
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		httpx.ControlPrefix+"/v1/diagnostics?probe=1&org_id=org_a", nil)
	srv.diagnostics(w, r, admin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestDoctorCatchesAFilterPointingAtNothing(t *testing.T) {
	// A filter fails closed, so a filter whose model has gone is not a
	// degraded filter: it is every request it covers, refused. Nothing else
	// in the product says so until the refusals start.
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

func TestEffectiveGuardrailsCollapseTheChain(t *testing.T) {
	// The question 'guardrail get' could never answer: what actually holds
	// this key, given that it sets nothing itself.
	srv, st := diagnosed(t)
	ctx := t.Context()

	rpm, teamRPM := 600, 120
	budget := int64(500_000_000)
	month := policy.PeriodMonth
	if err := st.PutPolicy(ctx, policy.ScopeOrg, "org_a", policy.Limits{
		RPM: &rpm, BudgetMicros: &budget, BudgetPeriod: &month,
		AllowedModels: []string{"keera-speed", "keera-frontier"},
	}); err != nil {
		t.Fatalf("PutPolicy org: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeTeam, "team_a", policy.Limits{
		RPM: &teamRPM, AllowedModels: []string{"keera-speed"},
	}); err != nil {
		t.Fatalf("PutPolicy team: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		httpx.ControlPrefix+"/v1/guardrails/key/key_a/effective", nil).WithContext(ctx)
	r.SetPathValue("scope", "key")
	r.SetPathValue("id", "key_a")
	srv.effectiveGuardrails(w, r, operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	// The allow-list intersects, so the team's narrower one wins.
	if len(eff.AllowedModels) != 1 || eff.AllowedModels[0] != "keera-speed" {
		t.Errorf("allowed models = %v, want the intersection", eff.AllowedModels)
	}
	// Every level is reported, because which level set a number is the thing
	// somebody needs in order to change the right one.
	if len(eff.Levels) != 3 {
		t.Fatalf("levels = %d, want org, team and key", len(eff.Levels))
	}
	if eff.Levels[0].Name != "Example Bank" || eff.Levels[1].Name != "Payments" {
		t.Errorf("levels = %+v, want them named", eff.Levels)
	}
	// Rate limits are enforced per level rather than merged, so both are here
	// and the tightest is the team's.
	var tightest int
	for _, s := range eff.Scopes {
		if s.RPM > 0 && (tightest == 0 || s.RPM < tightest) {
			tightest = s.RPM
		}
	}
	if tightest != teamRPM {
		t.Errorf("tightest rpm = %d, want the team's %d", tightest, teamRPM)
	}
	// Budgets do not merge either: the organisation's is the one that holds,
	// and it is reported against the organisation and not against the key.
	found := false
	for _, s := range eff.Scopes {
		if s.Type == policy.ScopeOrg && s.BudgetMicros == budget {
			found = true
		}
	}
	if !found {
		t.Errorf("scopes = %+v, want the organisation's budget on its own level", eff.Scopes)
	}
}

func TestEffectiveGuardrailsOnATeamStopAtTheTeam(t *testing.T) {
	// Asking about a team is asking what holds every key in it, and a key that
	// does not exist yet holds nothing - so there is no trailing level with no
	// id in the answer.
	srv, _ := diagnosed(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		httpx.ControlPrefix+"/v1/guardrails/team/team_a/effective", nil).WithContext(t.Context())
	r.SetPathValue("scope", "team")
	r.SetPathValue("id", "team_a")
	srv.effectiveGuardrails(w, r, operator())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var eff Effective
	if err := json.Unmarshal(w.Body.Bytes(), &eff); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(eff.Levels) != 2 {
		t.Errorf("levels = %d, want the organisation and the team", len(eff.Levels))
	}
	for _, s := range eff.Scopes {
		if s.Type == policy.ScopeKey {
			t.Error("a team's effective guardrails named a key scope")
		}
	}
}
