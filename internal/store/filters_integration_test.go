package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestFilterRoundTripAndTheGuardrailsThatNameOne(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	redact := policy.Filter{
		OrgID: f.orgID, Alias: "redact-secrets", Model: "keera-guard",
		Prompt:      "Replace every credential with [CREDENTIAL].",
		Description: "Takes credentials out of anything leaving the cluster",
	}
	saved, err := st.UpsertFilter(ctx, redact)
	if err != nil {
		t.Fatalf("UpsertFilter: %v", err)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Error("the write did not return its timestamps")
	}
	// A filter written without a mode is a rewrite filter. Every filter was one
	// before there was anything else to be, and a stored row that read back as
	// a gate would only ever say no.
	if saved.Mode != policy.FilterModeRewrite {
		t.Errorf("mode = %q, want %q", saved.Mode, policy.FilterModeRewrite)
	}

	gate := redact
	gate.Alias, gate.Mode = "no-exports", policy.FilterModeGate
	gate.Prompt = "Refuse a request whose purpose is to move data out of this organisation."
	if _, err := st.UpsertFilter(ctx, gate); err != nil {
		t.Fatalf("UpsertFilter for a gate: %v", err)
	}
	read, err := st.Filter(ctx, f.orgID, gate.Alias)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if read.Mode != policy.FilterModeGate {
		t.Errorf("mode = %q, want the gate it was written as", read.Mode)
	}
	if err := st.DeleteFilter(ctx, f.orgID, gate.Alias); err != nil {
		t.Fatalf("DeleteFilter: %v", err)
	}

	// A second organisation's filter of the same alias is a different filter.
	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	other := redact
	other.OrgID, other.Prompt = "org_2", "Something else entirely."
	if _, err := st.UpsertFilter(ctx, other); err != nil {
		t.Fatalf("UpsertFilter for a second org: %v", err)
	}

	mine, err := st.ListFilters(ctx, f.orgID)
	if err != nil {
		t.Fatalf("ListFilters: %v", err)
	}
	if len(mine) != 1 || mine[0].Prompt != redact.Prompt {
		t.Fatalf("ListFilters returned %+v; a filter must not leak across tenants", mine)
	}
	if all, err := st.LoadFilters(ctx); err != nil || len(all) != 2 {
		t.Fatalf("LoadFilters = %d filters, %v; the gateway caches every tenant's",
			len(all), err)
	}

	// Nothing names it yet.
	if users, err := st.FilterUsers(ctx, f.orgID, redact.Alias); err != nil || len(users) != 0 {
		t.Fatalf("FilterUsers = %+v, %v, want none", users, err)
	}

	// A guardrail on the team, and one in the other organisation that happens
	// to use the same alias.
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID,
		policy.Limits{Filters: []string{redact.Alias}}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeOrg, "org_2",
		policy.Limits{Filters: []string{redact.Alias}}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}

	users, err := st.FilterUsers(ctx, f.orgID, redact.Alias)
	if err != nil {
		t.Fatalf("FilterUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("FilterUsers = %+v, want only this organisation's guardrail", users)
	}
	if users[0].ScopeType != policy.ScopeTeam || users[0].Name != "Payments Platform" {
		t.Errorf("FilterUsers[0] = %+v, want the team named so a refusal can quote it",
			users[0])
	}

	// The key inherits the team's filter through the one join the gateway makes.
	resolved, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if len(resolved.Filters) != 1 || resolved.Filters[0] != redact.Alias {
		t.Errorf("Filters = %q, want the team's", resolved.Filters)
	}

	if err := st.DeleteFilter(ctx, f.orgID, redact.Alias); err != nil {
		t.Fatalf("DeleteFilter: %v", err)
	}
	if _, err := st.Filter(ctx, f.orgID, redact.Alias); !errors.Is(err, ErrNotFound) {
		t.Errorf("Filter after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.Filter(ctx, "org_2", redact.Alias); err != nil {
		t.Errorf("deleting one tenant's filter removed another's: %v", err)
	}
}

func TestDeletingAnOrgTakesItsFiltersWithIt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	if _, err := st.UpsertFilter(ctx, policy.Filter{
		OrgID: f.orgID, Alias: "redact", Model: "keera-guard", Prompt: "…",
	}); err != nil {
		t.Fatalf("UpsertFilter: %v", err)
	}
	if _, err := st.DeleteOrg(ctx, f.orgID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	// A filter is one organisation's wording about its own data. Unlike the
	// usage log, there is nobody left who could have a use for it.
	if all, err := st.LoadFilters(ctx); err != nil || len(all) != 0 {
		t.Fatalf("LoadFilters = %+v, %v, want none left", all, err)
	}
}

// filterTraffic is a week of one organisation's filtered requests: two teams,
// one filter, and every outcome it has.
func filterTraffic(t *testing.T, st *Store, ctx context.Context, f fixture,
	now time.Time) {
	t.Helper()
	if _, err := st.CreateTeam(ctx, "team_2", f.orgID, "Retail Lending"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	run := func(outcome FilterOutcome, ms int64, micros int64, changed int) FilterRun {
		return FilterRun{
			Filter: "redact", Mode: policy.FilterModeRewrite,
			Outcome: outcome, LatencyMS: ms, CostMicros: micros,
			Segments: 4, Changed: changed,
		}
	}
	event := func(offset time.Duration, teamID string, status int, cost int64,
		runs ...FilterRun) Event {
		return Event{
			TS: now.Add(offset), OrgID: f.orgID, TeamID: teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: status, CostMicros: cost,
			Latency: time.Second, FilterRuns: runs,
		}
	}
	events := []Event{
		event(0, f.teamID, 200, 1800, run(FilterPass, 100, 300, 0)),
		event(time.Minute, f.teamID, 200, 1800, run(FilterRewrite, 200, 300, 2)),
		event(2*time.Minute, f.teamID, 200, 1800, run(FilterRewrite, 300, 300, 1)),
		// The refusals, all in one team - which is the point of the breakdown:
		// a rate across an organisation is not a rate anybody experiences.
		event(3*time.Minute, "team_2", 403, 300, run(FilterRefuse, 400, 300, 0)),
		event(4*time.Minute, "team_2", 403, 300, run(FilterRefuse, 900, 300, 0)),
		// One that could not run at all, and one request nothing filtered.
		event(5*time.Minute, "team_2", 502, 0, run(FilterError, 0, 0, 0)),
		event(6*time.Minute, f.teamID, 200, 1800),
	}
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
}

func TestFilterRunsAreRecordedWithTheRequestsTheyFiltered(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	filterTraffic(t, st, ctx, f, now)

	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	stats, err := st.FilterStats(ctx, f.orgID, from, to)
	if err != nil {
		t.Fatalf("FilterStats: %v", err)
	}
	st1, ok := stats["redact"]
	if !ok {
		t.Fatalf("FilterStats = %+v, want the filter that ran", stats)
	}
	switch {
	case st1.Runs != 6:
		t.Errorf("runs = %d, want the six requests it filtered", st1.Runs)
	case st1.Pass != 1 || st1.Rewrote != 2 || st1.Refused != 2 || st1.Errors != 1:
		t.Errorf("split = %d pass, %d rewrote, %d refused, %d errors; want 1/2/2/1",
			st1.Pass, st1.Rewrote, st1.Refused, st1.Errors)
	case st1.CostMicros != 1500:
		t.Errorf("cost = %d, want the five runs that spent anything", st1.CostMicros)
	case st1.Segments != 24 || st1.Changed != 3:
		// The per-segment reading, which is what says whether an instruction is
		// too eager: three segments of twenty-four is a filter doing its job.
		t.Errorf("segments = %d, changed = %d; want 24 shown and 3 edited",
			st1.Segments, st1.Changed)
	case st1.LatencyMedianMS != 250 || st1.LatencyP95MS < 700:
		// 0, 100, 200, 300, 400, 900: the median is what it usually costs and
		// the p95 is the one somebody complains about.
		t.Errorf("latency = %dms median, %dms p95; want 250 and the tail",
			st1.LatencyMedianMS, st1.LatencyP95MS)
	case st1.LastRunAt == nil || st1.FirstRunAt == nil:
		t.Error("the window's first and last run were not reported")
	}

	rep, err := st.FilterReportFor(ctx, f.orgID, "redact", from, to)
	if err != nil {
		t.Fatalf("FilterReportFor: %v", err)
	}
	if rep.Runs != st1.Runs || rep.Refused != st1.Refused || rep.CostMicros != st1.CostMicros {
		t.Errorf("the filter's own screen (%+v) and its row (%+v) disagree",
			rep.FilterStat, st1)
	}
	// What the two numbers on the screen are a share of: the filter ran on six
	// of the organisation's seven requests, and its spend is part of their bill.
	if rep.Requests != 7 {
		t.Errorf("requests = %d, want all seven of the organisation's", rep.Requests)
	}
	if rep.OrgCostMicros != 7800 {
		t.Errorf("org cost = %d, want every request's", rep.OrgCostMicros)
	}

	byTeam := map[string]FilterTeamRow{}
	for _, row := range rep.Teams {
		byTeam[row.TeamID] = row
	}
	if got := byTeam["team_2"]; got.Runs != 3 || got.Refused != 2 || got.Errors != 1 {
		t.Errorf("team_2 = %+v, want the three runs and both refusals", got)
	}
	if got := byTeam[f.teamID]; got.Runs != 3 || got.Refused != 0 || got.Rewrote != 2 {
		t.Errorf("%s = %+v, want three runs and no refusal", f.teamID, got)
	}
	// Ordered by refusals, because the team living with them is what this
	// breakdown is opened for.
	if len(rep.Teams) != 2 || rep.Teams[0].TeamID != "team_2" {
		t.Errorf("teams = %+v, want the most refused first", rep.Teams)
	}
}

func TestAFilterReportPlotsEveryBucketInTheWindow(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	filterTraffic(t, st, ctx, f, now)

	// Six hours of a window in which one hour has traffic. A filter that
	// stopped running is the thing being looked for, and a chart drawn only
	// from the buckets that have rows hides exactly that.
	rep, err := st.FilterReportFor(ctx, f.orgID, "redact",
		now.Add(-5*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("FilterReportFor: %v", err)
	}
	if rep.Bucket != "hour" {
		t.Errorf("bucket = %q, want hourly for a window of hours", rep.Bucket)
	}
	if len(rep.Series) < 6 {
		t.Fatalf("series = %d points, want one per hour of the window", len(rep.Series))
	}
	var busy int
	for _, p := range rep.Series {
		if p.Runs > 0 {
			busy++
			if p.Refused != 2 || p.Errors != 1 || p.Rewrote != 2 {
				t.Errorf("the busy bucket = %+v, want the outcomes told apart", p)
			}
		}
	}
	if busy != 1 {
		t.Errorf("%d buckets had traffic, want the one that did", busy)
	}
}

func TestAFiltersHistoryOutlivesTheFilter(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	filterTraffic(t, st, ctx, f, now)

	// What was this doing before we removed it is a question asked after the
	// removal, so the log is not a foreign key into the filters table.
	if _, err := st.UpsertFilter(ctx, policy.Filter{
		OrgID: f.orgID, Alias: "redact", Model: "keera-guard", Prompt: "Redact.",
	}); err != nil {
		t.Fatalf("UpsertFilter: %v", err)
	}
	if err := st.DeleteFilter(ctx, f.orgID, "redact"); err != nil {
		t.Fatalf("DeleteFilter: %v", err)
	}
	rep, err := st.FilterReportFor(ctx, f.orgID, "redact",
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("FilterReportFor: %v", err)
	}
	if rep.Runs != 6 {
		t.Errorf("runs = %d, want the traffic it saw while it existed", rep.Runs)
	}
}

func TestFilterRunsStayInsideOneTenant(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	filterTraffic(t, st, ctx, f, now)

	if _, err := st.CreateOrg(ctx, "org_2", "Another Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// Two organisations may both have a filter called redact, and they are
	// different filters. One tenant's traffic must not appear on the other's
	// screen.
	if err := st.WriteEvents(ctx, []Event{{
		TS: now, OrgID: "org_2", Alias: "keera-code", Status: 200, CostMicros: 10,
		FilterRuns: []FilterRun{{
			Filter: "redact", Mode: policy.FilterModeGate, Outcome: FilterRefuse,
			LatencyMS: 10, CostMicros: 10, Segments: 1,
		}},
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rep, err := st.FilterReportFor(ctx, f.orgID, "redact",
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("FilterReportFor: %v", err)
	}
	if rep.Runs != 6 || rep.Refused != 2 {
		t.Errorf("report = %+v, want only this tenant's runs", rep.FilterStat)
	}
	// And an operator looking across every tenant sees both.
	all, err := st.FilterStats(ctx, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("FilterStats: %v", err)
	}
	if all["redact"].Runs != 7 {
		t.Errorf("unscoped runs = %d, want every tenant's seven", all["redact"].Runs)
	}
}

func TestAFilterRemembersWhetherItEnforces(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	// A filter written without saying enforces, which is what every filter did
	// before shadow existed: the default cannot be the one that quietly stops
	// guarding anything.
	saved, err := st.UpsertFilter(ctx, policy.Filter{
		OrgID: f.orgID, Alias: "redact", Model: "keera-guard", Prompt: "Redact.",
	})
	if err != nil {
		t.Fatalf("UpsertFilter: %v", err)
	}
	if saved.Shadow {
		t.Error("a filter written without saying came back not enforcing")
	}

	shadowed := saved
	shadowed.Shadow = true
	if _, err := st.UpsertFilter(ctx, shadowed); err != nil {
		t.Fatalf("UpsertFilter into shadow: %v", err)
	}
	read, err := st.Filter(ctx, f.orgID, "redact")
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !read.Shadow {
		t.Error("a filter put into shadow read back as enforcing")
	}
	// And out again, which is the whole rollout: the instruction and the mode
	// are untouched, so what was measured is what goes live.
	read.Shadow = false
	if _, err := st.UpsertFilter(ctx, read); err != nil {
		t.Fatalf("UpsertFilter out of shadow: %v", err)
	}
	if back, err := st.Filter(ctx, f.orgID, "redact"); err != nil || back.Shadow {
		t.Errorf("filter = %+v, %v; want it enforcing again", back, err)
	}
}

func TestRetentionTakesTheFilterLogWithIt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	old := time.Now().Add(-90 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)

	runs := []FilterRun{{
		Filter: "redact", Mode: policy.FilterModeRewrite, Outcome: FilterRewrite,
		LatencyMS: 100, CostMicros: 300, Segments: 1, Changed: 1,
	}}
	if err := st.WriteEvents(ctx, []Event{
		{TS: old, OrgID: f.orgID, Alias: "keera-code", Status: 200, CostMicros: 10,
			FilterRuns: runs},
		{TS: recent, OrgID: f.orgID, Alias: "keera-code", Status: 200, CostMicros: 10,
			FilterRuns: runs},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	// A deployment that kept ninety days of requests and forever of what its
	// filters did to them would be keeping the larger table for the longer
	// time, to answer questions about requests it can no longer see.
	if _, err := st.PurgeUsage(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("PurgeUsage: %v", err)
	}
	stats, err := st.FilterStats(ctx, f.orgID, old.Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("FilterStats: %v", err)
	}
	if stats["redact"].Runs != 1 {
		t.Errorf("runs = %d after retention, want only the one inside the window",
			stats["redact"].Runs)
	}
}
