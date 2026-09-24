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

	auto := policy.Router{
		OrgID: f.orgID, Alias: "auto", Model: "keera-picker",
		Prompt:       "Send short edits to the small model and everything else to the large one.",
		Destinations: []string{"keera-small", "keera-large"},
		Fallback:     "keera-small",
		Description:  "Keeps the large model for the work that needs it",
	}
	saved, err := st.UpsertRouter(ctx, auto)
	if err != nil {
		t.Fatalf("UpsertRouter: %v", err)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Error("the write did not return its timestamps")
	}

	read, err := st.Router(ctx, f.orgID, auto.Alias)
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if !slices.Equal(read.Destinations, auto.Destinations) {
		t.Errorf("destinations = %q, want %q", read.Destinations, auto.Destinations)
	}
	if read.Fallback != "keera-small" || read.Refuses() {
		t.Errorf("fallback = %q, Refuses = %v; want the stored fallback",
			read.Fallback, read.Refuses())
	}

	// A router with no fallback refuses what it cannot place, and the column is
	// null rather than empty so that the two are one field rather than two.
	strict := auto
	strict.Alias, strict.Fallback = "strict", ""
	if _, err := st.UpsertRouter(ctx, strict); err != nil {
		t.Fatalf("UpsertRouter with no fallback: %v", err)
	}
	stored, err := st.Router(ctx, f.orgID, strict.Alias)
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if !stored.Refuses() {
		t.Errorf("Refuses = false, want a router with no fallback to refuse")
	}

	// A second organisation's router of the same name is a different router.
	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	other := auto
	other.OrgID, other.Destinations = "org_2", []string{"keera-large", "keera-huge"}
	if _, err := st.UpsertRouter(ctx, other); err != nil {
		t.Fatalf("UpsertRouter for a second org: %v", err)
	}
	mine, err := st.ListRouters(ctx, f.orgID)
	if err != nil {
		t.Fatalf("ListRouters: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("ListRouters returned %+v; a router must not leak across tenants", mine)
	}
	if all, err := st.LoadRouters(ctx); err != nil || len(all) != 3 {
		t.Fatalf("LoadRouters = %d routers, %v; the gateway caches every tenant's",
			len(all), err)
	}

	// Nothing names it yet.
	if users, err := st.RouterUsers(ctx, f.orgID, auto.Alias); err != nil || len(users) != 0 {
		t.Fatalf("RouterUsers = %+v, %v, want none", users, err)
	}
	// An allow-list narrowed to the router, which is how a scope is made to
	// route whatever it asks for - and so the one arrangement a deletion would
	// leave able to reach nothing at all.
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID,
		policy.Limits{AllowedModels: []string{auto.Alias}}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeOrg, "org_2",
		policy.Limits{AllowedModels: []string{auto.Alias}}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	users, err := st.RouterUsers(ctx, f.orgID, auto.Alias)
	if err != nil {
		t.Fatalf("RouterUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("RouterUsers = %+v, want only this organisation's allow-list", users)
	}
	if users[0].ScopeType != policy.ScopeTeam || users[0].Name != "Payments Platform" {
		t.Errorf("RouterUsers[0] = %+v, want the team named so a refusal can quote it",
			users[0])
	}

	if err := st.DeleteRouter(ctx, f.orgID, auto.Alias); err != nil {
		t.Fatalf("DeleteRouter: %v", err)
	}
	if _, err := st.Router(ctx, f.orgID, auto.Alias); !errors.Is(err, ErrNotFound) {
		t.Errorf("Router after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.Router(ctx, "org_2", auto.Alias); err != nil {
		t.Errorf("deleting one tenant's router removed another's: %v", err)
	}
}

// A router's report is a reading of the usage log rather than a log of its own,
// so what it can say is bounded by what the request rows carry.
func TestRouterReportSplitsTrafficByDestination(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-3 * time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small",
			Status: 200, CostMicros: 100, InputTokens: 10, OutputTokens: 5, TTFT: time.Second,
			Router: "auto", RouterOutcome: RouterChose, RouterMS: 40},
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small",
			Status: 200, CostMicros: 100, InputTokens: 10, OutputTokens: 5, TTFT: time.Second,
			Router: "auto", RouterOutcome: RouterChose, RouterMS: 60},
		{TS: now.Add(-time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-large",
			Status: 200, CostMicros: 900, InputTokens: 20, OutputTokens: 10, TTFT: 2 * time.Second,
			Router: "auto", RouterOutcome: RouterChose, RouterMS: 50},
		// A request placed without a decision, and one placed nowhere at all.
		{TS: now.Add(-50 * time.Minute), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-small",
			Status: 200, CostMicros: 100, Router: "auto", RouterOutcome: RouterFellBack,
			RouterMS: 20},
		{TS: now.Add(-40 * time.Minute), OrgID: f.orgID, KeyID: f.keyID, Alias: "strict",
			Status: 503, Router: "strict", RouterOutcome: RouterError, RouterMS: 30},
		// And a request that named a model directly, which is no router's.
		{TS: now.Add(-30 * time.Minute), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-large",
			Status: 200, CostMicros: 900},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rep, err := st.RouterReportFor(ctx, f.orgID, "auto", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("RouterReportFor: %v", err)
	}
	if rep.Total.Requests != 4 {
		t.Errorf("requests = %d, want the 4 this router placed", rep.Total.Requests)
	}
	if rep.Chose != 3 || rep.FellBack != 1 || rep.Errored != 0 {
		t.Errorf("chose/fellBack/errored = %d/%d/%d, want 3/1/0",
			rep.Chose, rep.FellBack, rep.Errored)
	}
	// The split is the reason anybody reads this screen.
	if len(rep.Destinations) != 2 {
		t.Fatalf("destinations = %+v, want one row per model it placed on", rep.Destinations)
	}
	if rep.Destinations[0].Alias != "keera-small" || rep.Destinations[0].Requests != 3 {
		t.Errorf("destinations[0] = %+v, want keera-small with 3", rep.Destinations[0])
	}
	if rep.DecisionMedianMS == 0 {
		t.Error("the decision latency was not measured")
	}

	// A router with no fallback: its refusals are counted, and they are not a
	// destination row, because those requests reached no model.
	strict, err := st.RouterReportFor(ctx, f.orgID, "strict", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("RouterReportFor: %v", err)
	}
	if strict.Errored != 1 || len(strict.Destinations) != 0 {
		t.Errorf("strict = %d errored, %d destinations; want 1 and none",
			strict.Errored, len(strict.Destinations))
	}

	stats, err := st.RouterStats(ctx, f.orgID, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("RouterStats: %v", err)
	}
	if stats["auto"].Requests != 4 || stats["auto"].FellBack != 1 {
		t.Errorf("stats[auto] = %+v, want 4 requests with 1 fallback", stats["auto"])
	}
	if _, ok := stats["keera-large"]; ok {
		t.Error("a request that named a model directly was counted as a router's")
	}
}

func TestDeletingAnOrgTakesItsRoutersWithIt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if _, err := st.UpsertRouter(ctx, policy.Router{
		OrgID: f.orgID, Alias: "auto", Model: "keera-picker", Prompt: "…",
		Destinations: []string{"keera-small", "keera-large"},
	}); err != nil {
		t.Fatalf("UpsertRouter: %v", err)
	}
	if _, err := st.DeleteOrg(ctx, f.orgID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if all, err := st.LoadRouters(ctx); err != nil || len(all) != 0 {
		t.Fatalf("LoadRouters = %+v, %v, want none left", all, err)
	}
}
