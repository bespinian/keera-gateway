package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestRouterRoundTripAndTheAllowListsThatNameOne(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	safe := policy.Router{
		OrgID: f.orgID, Alias: "safe", DisplayName: "Safe", Description: "Redacts what leaves",
		Start: &policy.Step{
			ID: "cap", Kind: policy.StepBudget, BudgetMicros: 50_000_000, Period: policy.PeriodMonth,
			Exceeded: &policy.Step{ID: "spent", Kind: policy.StepModel, Ref: "keera-small"},
			Next: &policy.Step{ID: "route", Kind: policy.StepRoute, Mode: policy.RouteModeFallback,
				Destinations: []string{"keera-small", "keera-large"},
				Branches: map[string]*policy.Step{
					"keera-large": {ID: "redact", Kind: policy.StepFilter, Ref: "redact",
						Next: &policy.Step{ID: "out", Kind: policy.StepModel, Ref: "keera-large"}},
				}},
		},
	}
	saved, err := st.UpsertRouter(ctx, safe)
	if err != nil {
		t.Fatalf("UpsertRouter: %v", err)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Error("the write did not return its timestamps")
	}

	read, err := st.Router(ctx, f.orgID, safe.Alias)
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if read.DisplayName != "Safe" || read.Description != safe.Description {
		t.Errorf("read %q / %q", read.DisplayName, read.Description)
	}
	// The tree comes back whole: every step, in the same places.
	var ids []string
	for s := range read.Steps() {
		ids = append(ids, s.ID)
	}
	if want := []string{"cap", "route", "redact", "out", "spent"}; !slices.Equal(ids, want) {
		t.Errorf("steps = %v, want %v", ids, want)
	}
	if read.Start.BudgetMicros != 50_000_000 || read.Start.Exceeded.Ref != "keera-small" {
		t.Errorf("the budget step came back as %+v", read.Start)
	}
	if route := read.Start.Next; route.Mode != policy.RouteModeFallback || len(route.Destinations) != 2 {
		t.Errorf("the route step came back as %+v", route)
	}

	// A second write replaces the tree.
	safe.Start = &policy.Step{ID: "only", Kind: policy.StepModel, Ref: "keera-small"}
	if _, err := st.UpsertRouter(ctx, safe); err != nil {
		t.Fatalf("UpsertRouter again: %v", err)
	}
	if read, _ = st.Router(ctx, f.orgID, safe.Alias); read.Start.ID != "only" || read.Start.Next != nil {
		t.Errorf("after a second write the tree is %+v", read.Start)
	}

	if all, err := st.LoadRouters(ctx); err != nil || len(all) != 1 {
		t.Fatalf("LoadRouters = %d, %v", len(all), err)
	}
	if setup, err := st.SetupState(ctx, f.orgID); err != nil || setup.Routers != 1 {
		t.Errorf("SetupState routers = %d, %v, want 1", setup.Routers, err)
	}

	// Named by a project's allow-list, it is a user; the organisation's own
	// list, which names more, is not.
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID,
		policy.Limits{AllowedModels: []string{safe.Alias}}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID,
		policy.Limits{AllowedModels: []string{safe.Alias, "keera-small"}}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	users, err := st.RouterUsers(ctx, f.orgID, safe.Alias)
	if err != nil || len(users) != 1 || users[0].ScopeType != policy.ScopeProject {
		t.Fatalf("RouterUsers = %+v, %v, want the project", users, err)
	}

	if err := st.DeleteRouter(ctx, f.orgID, safe.Alias); err != nil {
		t.Fatalf("DeleteRouter: %v", err)
	}
	if _, err := st.Router(ctx, f.orgID, safe.Alias); !errors.Is(err, ErrNotFound) {
		t.Errorf("Router after delete = %v, want ErrNotFound", err)
	}
	// Deleting it takes it off the allow-lists, so a router added later under
	// the same alias is not allowed by accident.
	org, err := st.GetGuardrail(ctx, policy.ScopeOrg, f.orgID)
	if err != nil {
		t.Fatalf("GetGuardrail: %v", err)
	}
	if slices.Contains(org.AllowedModels, safe.Alias) {
		t.Errorf("the organisation's allow-list still names the deleted router: %v", org.AllowedModels)
	}
}

// A router's report is read from the usage rows, which carry its alias, and
// its limit steps' spend is kept like any other scope's.
func TestRouterReportAndSpend(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	step := policy.Scope{Type: policy.ScopeRouter, ID: f.orgID + "/safe/cap", Period: policy.PeriodMonth}

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-large",
			Status: 200, CostMicros: 900, Router: "safe", RouterOutcome: RouterChose, RouterMS: 40,
			Scopes: []policy.Scope{step}},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small",
			Status: 200, CostMicros: 100, Router: "safe", RouterOutcome: RouterFellBack},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small",
			Status: 200, CostMicros: 100},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rep, err := st.RouterReportFor(ctx, f.orgID, "safe", now.Add(-24*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RouterReportFor: %v", err)
	}
	if rep.Total.Requests != 2 || rep.Total.CostMicros != 1000 {
		t.Errorf("total = %+v, want the two requests that named the router", rep.Total)
	}
	if len(rep.Destinations) != 2 || rep.Destinations[0].Alias != "keera-large" {
		t.Errorf("destinations = %+v, want both models, the dearer first", rep.Destinations)
	}
	if rep.Chose != 1 || rep.FellBack != 1 || rep.Errored != 0 || rep.DecisionMedianMS != 40 {
		t.Errorf("decisions = %+v, want one chosen and one fallen back", rep)
	}

	stats, err := st.RouterStats(ctx, f.orgID, now.Add(-24*time.Hour), now.Add(time.Minute))
	if err != nil || stats["safe"].Requests != 2 || stats["safe"].FellBack != 1 {
		t.Errorf("RouterStats = %+v, %v", stats, err)
	}

	// A daily limit counts requests and tokens in the same table, under its
	// own ids.
	requests := policy.Scope{Type: policy.ScopeRouter, ID: f.orgID + "/safe/daily|requests",
		Period: policy.PeriodDay, Meter: policy.MeterRequests, Quota: 100}
	tokens := policy.Scope{Type: policy.ScopeRouter, ID: f.orgID + "/safe/daily|tokens",
		Period: policy.PeriodDay, Meter: policy.MeterTokens, Quota: 100_000}
	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small", Status: 200,
			InputTokens: 30, OutputTokens: 12, Router: "safe", Scopes: []policy.Scope{requests, tokens}},
		// Refused before an answer: a request all the same, and no tokens.
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small", Status: 403,
			Router: "safe", Scopes: []policy.Scope{requests, tokens}},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	spend, err := st.LoadSpend(ctx, now)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	if !slices.ContainsFunc(spend, func(r SpendRow) bool {
		return r.ScopeType == policy.ScopeRouter && r.ScopeID == step.ID && r.Micros == 900
	}) {
		t.Errorf("spend = %+v, want 900 against the router's budget step", spend)
	}
	for id, want := range map[string]int64{requests.ID: 2, tokens.ID: 42} {
		if !slices.ContainsFunc(spend, func(r SpendRow) bool { return r.ScopeID == id && r.Micros == want }) {
			t.Errorf("spend = %+v, want %d counted under %s", spend, want, id)
		}
	}
}

// A request a limit step refused before any step placed it has no model, so
// it counts in the total and is no destination.
func TestRouterReportLeavesOutWhatNoStepPlaced(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small", Status: 200,
			Router: "safe", RouterOutcome: RouterChose},
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Status: 429, Router: "safe"},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	rep, err := st.RouterReportFor(ctx, f.orgID, "safe", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RouterReportFor: %v", err)
	}
	if rep.Total.Requests != 2 {
		t.Errorf("total = %+v, want both requests", rep.Total)
	}
	if len(rep.Destinations) != 1 || rep.Destinations[0].Alias != "keera-small" {
		t.Errorf("destinations = %+v, want only the model a step placed one on", rep.Destinations)
	}
}

// The CLI promises that a new router of a deleted one's alias starts from
// zero, so the delete takes the old limits' counts with it, and only those.
func TestDeletingARouterForgetsWhatItsLimitsCounted(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	for _, alias := range []string{"safe", "safer"} {
		if _, err := st.UpsertRouter(ctx, policy.Router{OrgID: f.orgID, Alias: alias,
			Start: &policy.Step{ID: "out", Kind: policy.StepModel, Ref: "keera-small"}}); err != nil {
			t.Fatalf("UpsertRouter: %v", err)
		}
	}
	scope := func(alias string) policy.Scope {
		return policy.Scope{Type: policy.ScopeRouter, ID: f.orgID + "/" + alias + "/cap",
			Period: policy.PeriodMonth}
	}
	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small", Status: 200,
			CostMicros: 500, Router: "safe", Scopes: []policy.Scope{scope("safe")}},
		{TS: now, OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small", Status: 200,
			CostMicros: 700, Router: "safer", Scopes: []policy.Scope{scope("safer")}},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	if err := st.DeleteRouter(ctx, f.orgID, "safe"); err != nil {
		t.Fatalf("DeleteRouter: %v", err)
	}
	spend, err := st.LoadSpend(ctx, now)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	if slices.ContainsFunc(spend, func(r SpendRow) bool { return r.ScopeID == scope("safe").ID }) {
		t.Errorf("spend = %+v, want the deleted router's counts gone", spend)
	}
	if !slices.ContainsFunc(spend, func(r SpendRow) bool { return r.ScopeID == scope("safer").ID }) {
		t.Errorf("spend = %+v, want the other router's counts kept", spend)
	}
}
