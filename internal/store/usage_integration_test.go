package store

import (
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestWriteEventsRollsUpSpendPerScope(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 13, 4, 0, 0, time.UTC)

	scopes := []policy.Scope{
		{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodMonth},
		{Type: policy.ScopeTeam, ID: f.teamID, Period: policy.PeriodDay},
		{Type: policy.ScopeKey, ID: f.keyID, Period: policy.PeriodMonth},
	}
	events := []Event{
		{TS: now, OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID, Alias: "keera-code",
			InputTokens: 1000, OutputTokens: 200, CostMicros: 1800, Status: 200,
			Latency: 900 * time.Millisecond, TTFT: 120 * time.Millisecond,
			Stream: true, Scopes: scopes},
		{TS: now.Add(time.Minute), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", InputTokens: 500, OutputTokens: 100, CostMicros: 900,
			Status: 200, Latency: time.Second, Scopes: scopes},
		// A refusal: recorded, because it is the event a developer asks about,
		// and charged nothing.
		{TS: now.Add(2 * time.Minute), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", Status: 402, Latency: time.Millisecond, Scopes: scopes},
		// A refusal by a filter, which paid for the filter's own call. The
		// budget charges it, so the report must show it too.
		{TS: now.Add(3 * time.Minute), OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: "keera-code", InputTokens: 50, OutputTokens: 5, CostMicros: 300,
			Status: 403, Latency: time.Millisecond, Scopes: scopes},
	}
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rows, err := st.LoadSpend(ctx, now)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[string(r.ScopeType)+":"+r.ScopeID+":"+string(r.Period)] = r.Micros
	}
	want := map[string]int64{
		"org:" + f.orgID + ":month": 3000,
		"team:" + f.teamID + ":day": 3000,
		"key:" + f.keyID + ":month": 3000,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("spend[%s] = %d, want %d (all of %v)", k, got[k], v, got)
		}
	}

	// The report counts as requests only what was served, but its cost is what
	// the budget charged, refusals included.
	buckets, err := st.Usage(ctx, UsageQuery{OrgID: f.orgID, From: now.Add(-time.Hour),
		To: now.Add(time.Hour), GroupBy: "model"})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("Usage = %+v, want one model", buckets)
	}
	b := buckets[0]
	if b.Group != "keera-code" || b.Requests != 2 || b.InputTokens != 1550 ||
		b.OutputTokens != 305 || b.CostMicros != 3000 {
		t.Errorf("Usage bucket = %+v, want two served requests and every row's cost", b)
	}

	// The refusals are still there to be read, which is the whole reason they
	// were written.
	refusals, err := st.Refusals(ctx, f.orgID, []string{f.keyID}, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("Refusals: %v", err)
	}
	if len(refusals) != 2 || refusals[0].Status != 403 || refusals[1].Status != 402 {
		t.Errorf("Refusals = %+v, want the 403 and the 402, newest first", refusals)
	}
}

// One batch can hold events from either side of midnight - the recorder flushes
// on a timer and does not know about the clock - and a day-period scope has a
// separate window on each side of it. The roll-up is summed per window before it
// is written, so this is what says the window is part of what it is summed by
// and not just the scope: charge both days into one row and a budget would be
// refusing traffic against yesterday's spend.
func TestWriteEventsKeepsSpendWindowsApartWithinOneBatch(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	lastNight := time.Date(2026, 9, 7, 23, 58, 0, 0, time.UTC)
	thisMorning := time.Date(2026, 9, 8, 0, 3, 0, 0, time.UTC)

	scopes := []policy.Scope{{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodDay}}
	if err := st.WriteEvents(ctx, []Event{
		{TS: lastNight, OrgID: f.orgID, Alias: "keera-code", CostMicros: 400,
			Status: 200, Scopes: scopes},
		{TS: thisMorning, OrgID: f.orgID, Alias: "keera-code", CostMicros: 700,
			Status: 200, Scopes: scopes},
		{TS: thisMorning.Add(time.Minute), OrgID: f.orgID, Alias: "keera-code",
			CostMicros: 50, Status: 200, Scopes: scopes},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	// Read as of the morning: the day window is the new one, and holds only
	// what was spent inside it.
	rows, err := st.LoadSpend(ctx, thisMorning)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadSpend = %+v, want only the open day", rows)
	}
	if rows[0].Micros != 750 {
		t.Errorf("Micros = %d, want 750: last night's 400 belongs to the day before",
			rows[0].Micros)
	}

	// And the night before is still its own row, with its own total.
	rows, err = st.LoadSpend(ctx, lastNight)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	if len(rows) != 1 || rows[0].Micros != 400 {
		t.Errorf("LoadSpend = %+v, want the previous day holding 400", rows)
	}
}

func TestLoadSpendReadsOnlyTheOpenWindows(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)

	// One event in the open month, one in the month before it.
	for _, ts := range []time.Time{now, now.AddDate(0, -1, 0)} {
		if err := st.WriteEvents(ctx, []Event{{
			TS: ts, OrgID: f.orgID, Alias: "keera-code", CostMicros: 1000, Status: 200,
			Scopes: []policy.Scope{{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodMonth}},
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}

	rows, err := st.LoadSpend(ctx, now)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadSpend = %+v, want only the open month", rows)
	}
	if rows[0].Micros != 1000 {
		t.Errorf("Micros = %d, want 1000: last month must not count against this month",
			rows[0].Micros)
	}
	if !rows[0].PeriodStart.UTC().Equal(policy.PeriodMonth.Start(now)) {
		t.Errorf("PeriodStart = %s, want %s", rows[0].PeriodStart, policy.PeriodMonth.Start(now))
	}
}

func TestUsageGroupsByEveryDimensionThePanelOffers(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	user, err := st.AddUser(ctx, "user_1", f.orgID, "dev@example.ch", "", "member")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	day := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)

	if err := st.WriteEvents(ctx, []Event{
		{TS: day, OrgID: f.orgID, TeamID: f.teamID, UserID: user.ID, KeyID: f.keyID,
			Alias: "keera-code", CostMicros: 100, Status: 200},
		{TS: day.AddDate(0, 0, 1), OrgID: f.orgID, TeamID: f.teamID, UserID: user.ID,
			KeyID: f.keyID, Alias: "keera-speed", CostMicros: 50, Status: 200},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	from, to := day.Add(-time.Hour), day.AddDate(0, 0, 2)
	for _, tt := range []struct {
		groupBy string
		want    map[string]int64 // group -> cost
	}{
		{"model", map[string]int64{"keera-code": 100, "keera-speed": 50}},
		{"team", map[string]int64{f.teamID: 150}},
		{"key", map[string]int64{f.keyID: 150}},
		{"user", map[string]int64{user.ID: 150}},
		{"org", map[string]int64{f.orgID: 150}},
		{"day", map[string]int64{"2026-09-07": 100, "2026-09-08": 50}},
		// Anything the API does not know groups by model rather than failing.
		{"nonsense", map[string]int64{"keera-code": 100, "keera-speed": 50}},
	} {
		t.Run(tt.groupBy, func(t *testing.T) {
			buckets, err := st.Usage(ctx, UsageQuery{OrgID: f.orgID, From: from, To: to,
				GroupBy: tt.groupBy})
			if err != nil {
				t.Fatalf("Usage: %v", err)
			}
			got := map[string]int64{}
			for _, b := range buckets {
				got[b.Group] = b.CostMicros
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Usage = %+v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("group %q = %d, want %d", k, got[k], v)
				}
			}
		})
	}

	// Another tenant's traffic is never in the answer.
	if _, err := st.CreateOrg(ctx, Org{ID: "org_2", Name: "Another Bank"}, OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if err := st.WriteEvents(ctx, []Event{{TS: day, OrgID: "org_2", Alias: "keera-code",
		CostMicros: 9999, Status: 200}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	buckets, err := st.Usage(ctx, UsageQuery{OrgID: f.orgID, From: from, To: to, GroupBy: "org"})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Group != f.orgID {
		t.Errorf("Usage = %+v, want only the requested organisation", buckets)
	}

	// Across every organisation, the two "keera-code" models are two rows.
	// Added up, an operator would read one model's traffic as twice what it is.
	buckets, err = st.Usage(ctx, UsageQuery{From: from, To: to, GroupBy: "model"})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	got := map[string]int64{}
	for _, b := range buckets {
		got[b.OrgID+"/"+b.Group] = b.CostMicros
	}
	want := map[string]int64{
		f.orgID + "/keera-code": 100, f.orgID + "/keera-speed": 50, "org_2/keera-code": 9999,
	}
	if len(got) != len(want) {
		t.Fatalf("Usage across orgs = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("group %q = %d, want %d", k, got[k], v)
		}
	}
}

func TestOverviewPlotsEveryBucketInTheWindow(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, Alias: "keera-code", InputTokens: 10, OutputTokens: 5,
			CostMicros: 100, Status: 200, TTFT: 100 * time.Millisecond},
		{TS: now, OrgID: f.orgID, Alias: "keera-code", InputTokens: 10, OutputTokens: 5,
			CostMicros: 100, Status: 200, TTFT: 300 * time.Millisecond},
		{TS: now, OrgID: f.orgID, Alias: "keera-code", Status: 429},
		// A 413 is a refusal too, as the request log counts it.
		{TS: now, OrgID: f.orgID, Alias: "keera-code", Status: 413},
		{TS: now, OrgID: f.orgID, Alias: "keera-code", Status: 502},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	// Six hours, so hourly buckets.
	o, err := st.Overview(ctx, f.orgID, now.Add(-3*time.Hour), now.Add(3*time.Hour), Scope{})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if o.Bucket != "hour" {
		t.Errorf("Bucket = %q, want hour for a six-hour window", o.Bucket)
	}
	if o.Requests != 5 {
		t.Errorf("Requests = %d, want 5 - every attempt, served or not", o.Requests)
	}
	if o.Refused != 2 {
		t.Errorf("Refused = %d, want 2", o.Refused)
	}
	if o.Failed != 1 {
		t.Errorf("Failed = %d, want 1", o.Failed)
	}
	if o.CostMicros != 200 {
		t.Errorf("CostMicros = %d, want 200", o.CostMicros)
	}
	if o.TTFTMedianMS < 100 || o.TTFTMedianMS > 300 {
		t.Errorf("TTFTMedianMS = %d, want between the two samples", o.TTFTMedianMS)
	}
	// A quiet hour is a zero in the chart, not a gap: plotting only the hours
	// with rows turns a quiet week into a flat line across the whole window.
	if len(o.Series) < 6 {
		t.Errorf("%d points for a six-hour window, want one per hour: %+v",
			len(o.Series), o.Series)
	}
	var plotted int64
	for _, p := range o.Series {
		plotted += p.Requests
	}
	if plotted != 5 {
		t.Errorf("the series holds %d requests, want 5", plotted)
	}

	// Four days, so daily buckets.
	daily, err := st.Overview(ctx, f.orgID, now.AddDate(0, 0, -2), now.AddDate(0, 0, 2), Scope{})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if daily.Bucket != "day" {
		t.Errorf("Bucket = %q, want day for a four-day window", daily.Bucket)
	}
}

func TestModelStatsReadsEachModelsFirstToken(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := st.WriteEvents(ctx, []Event{
		{TS: now, OrgID: f.orgID, Alias: "keera-code", Status: 200, TTFT: 100 * time.Millisecond},
		{TS: now, OrgID: f.orgID, Alias: "keera-code", Status: 200, TTFT: 300 * time.Millisecond},
		// A refusal waited for no token, so it is not in the median.
		{TS: now, OrgID: f.orgID, Alias: "keera-code", Status: 429},
		{TS: now, OrgID: f.orgID, Alias: "keera-large", Status: 200, TTFT: time.Second},
		{TS: now, OrgID: f.orgID, Alias: "keera-idle", Status: 502},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	stats, err := st.ModelStats(ctx, f.orgID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if got := stats["keera-code"].TTFTMedianMS; got != 200 {
		t.Errorf("keera-code median = %d, want 200", got)
	}
	if got := stats["keera-large"]; got.TTFTMedianMS != 1000 || got.TTFTP95MS != 1000 {
		t.Errorf("keera-large = %+v, want 1000 for both", got)
	}
	if _, ok := stats["keera-idle"]; ok {
		t.Error("a model with no first token has a row")
	}
}

func TestKeySummariesReportsLastUseAndWhatWasServed(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)

	if err := st.PutPolicy(ctx, policy.ScopeKey, f.keyID,
		policy.Limits{RPM: new(30)}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	if err := st.WriteEvents(ctx, []Event{
		{TS: now.Add(-2 * time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
			CostMicros: 700, Status: 200},
		// A refusal is the most recent use of the key, and "when was this last
		// used" is a question about being presented at all.
		{TS: now.Add(-time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
			Status: 429},
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	keys, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID, Since: now.AddDate(0, 0, -1)})
	if err != nil {
		t.Fatalf("KeySummaries: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("%d keys, want 1", len(keys))
	}
	k := keys[0]
	if k.Requests != 1 {
		t.Errorf("Requests = %d, want 1: the count explains the cost beside it", k.Requests)
	}
	if k.SpendMicros != 700 {
		t.Errorf("SpendMicros = %d, want 700", k.SpendMicros)
	}
	if k.LastUsedAt == nil || k.LastUsedAt.UTC().Sub(now.Add(-time.Hour)).Abs() > time.Second {
		t.Errorf("LastUsedAt = %v, want the refusal an hour ago", k.LastUsedAt)
	}
	if k.Limits.RPM == nil || *k.Limits.RPM != 30 {
		t.Errorf("Limits.RPM = %v, want the guardrail set on the key", k.Limits.RPM)
	}

	// A window that excludes the traffic reports a key nothing has used.
	fresh, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID, Since: now.Add(-time.Minute)})
	if err != nil {
		t.Fatalf("KeySummaries: %v", err)
	}
	if fresh[0].LastUsedAt != nil || fresh[0].Requests != 0 {
		t.Errorf("summary = %+v, want nothing inside the window", fresh[0])
	}
}

func TestTeamSummariesReadsTheOpenBudgetWindow(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID, policy.Limits{
		BudgetMicros: new(int64(500_000_000)), BudgetPeriod: new(policy.PeriodDay),
	}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	// Today's spend, and yesterday's, which must not be counted.
	for _, ts := range []time.Time{now, now.AddDate(0, 0, -1)} {
		if err := st.WriteEvents(ctx, []Event{{TS: ts, OrgID: f.orgID, TeamID: f.teamID,
			Alias: "keera-code", CostMicros: 4_000_000, Status: 200,
			Scopes: []policy.Scope{{Type: policy.ScopeTeam, ID: f.teamID, Period: policy.PeriodDay}},
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}

	teams, err := st.TeamSummaries(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("TeamSummaries: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("%d teams, want 1", len(teams))
	}
	if teams[0].SpendMicros != 4_000_000 {
		t.Errorf("SpendMicros = %d, want only today's 4000000", teams[0].SpendMicros)
	}
	if teams[0].BudgetMicros != 500_000_000 {
		t.Errorf("BudgetMicros = %d, want the guardrail", teams[0].BudgetMicros)
	}
	if teams[0].ActiveKeys != 1 {
		t.Errorf("ActiveKeys = %d, want 1", teams[0].ActiveKeys)
	}
	if err := st.RevokeKey(ctx, f.keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	teams, err = st.TeamSummaries(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("TeamSummaries: %v", err)
	}
	if teams[0].ActiveKeys != 0 {
		t.Errorf("ActiveKeys = %d after a revocation, want 0", teams[0].ActiveKeys)
	}
}

// A summary carries the scope's system prompt, because both screens that read
// one show it to somebody who is subject to it: a member reading what their
// team puts ahead of every request they make, and an administrator seeing at a
// glance which scopes say something at all. Leaving it out of the query made
// every scope look as though it said nothing.
func TestSummariesCarryTheSystemPrompt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	const (
		teamPrompt = "Answer in British English."
		keyPrompt  = "Prefer the standard library."
	)
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID, policy.Limits{
		SystemPrompt: new(teamPrompt),
	}); err != nil {
		t.Fatalf("PutPolicy(team): %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeKey, f.keyID, policy.Limits{
		SystemPrompt: new(keyPrompt),
	}); err != nil {
		t.Fatalf("PutPolicy(key): %v", err)
	}

	teams, err := st.TeamSummaries(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("TeamSummaries: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("%d teams, want 1", len(teams))
	}
	if got := teams[0].Limits.SystemPrompt; got == nil || *got != teamPrompt {
		t.Errorf("team SystemPrompt = %v, want %q", got, teamPrompt)
	}

	keys, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID, Since: now.AddDate(0, 0, -1)})
	if err != nil {
		t.Fatalf("KeySummaries: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("%d keys, want 1", len(keys))
	}
	if got := keys[0].Limits.SystemPrompt; got == nil || *got != keyPrompt {
		t.Errorf("key SystemPrompt = %v, want %q", got, keyPrompt)
	}

	// A scope that sets no prompt must come back as one that says nothing, not
	// as an empty string a screen would render as a blank instruction.
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID, policy.Limits{}); err != nil {
		t.Fatalf("PutPolicy(team, cleared): %v", err)
	}
	teams, err = st.TeamSummaries(ctx, f.orgID, now)
	if err != nil {
		t.Fatalf("TeamSummaries: %v", err)
	}
	if teams[0].Limits.SystemPrompt != nil {
		t.Errorf("SystemPrompt = %q after clearing, want nil", *teams[0].Limits.SystemPrompt)
	}
}

// The map's four readings of one window have to be four readings of one window:
// what is on the boxes, what is on the arrows and what is under the whole
// drawing must add up, or the picture is drawing an argument with itself.
func TestFlowsCountsOneWindowFourWays(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	day := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)

	ev := func(client, alias string, status int, cost int64, ttft time.Duration) Event {
		return Event{
			TS: day, OrgID: f.orgID, TeamID: f.teamID, KeyID: f.keyID,
			Alias: alias, Client: client, Status: status, CostMicros: cost,
			InputTokens: 10, OutputTokens: 5, TTFT: ttft,
		}
	}
	if err := st.WriteEvents(ctx, []Event{
		ev("claude-code", "keera-code", 200, 100, 200*time.Millisecond),
		ev("claude-code", "keera-code", 200, 100, 400*time.Millisecond),
		ev("claude-code", "keera-frontier", 200, 900, time.Second),
		ev("opencode", "keera-code", 200, 100, 300*time.Millisecond),
		// A refusal and a failure, which the map draws differently from each
		// other and from everything that worked.
		ev("opencode", "keera-code", 429, 0, 0),
		ev("", "keera-code", 502, 0, 0),
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	rep, err := st.Flows(ctx, f.orgID, day.Add(-time.Hour), day.Add(time.Hour))
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}

	if rep.Total.Requests != 6 {
		t.Errorf("total requests = %d, want every recorded request", rep.Total.Requests)
	}
	if rep.Total.Refused != 1 || rep.Total.Failed != 1 {
		t.Errorf("refused/failed = %d/%d, want 1/1", rep.Total.Refused, rep.Total.Failed)
	}
	if rep.Total.CostMicros != 1200 {
		t.Errorf("cost = %d, want 1200", rep.Total.CostMicros)
	}

	// A client that named itself in no way at all is a client, not a total.
	if got := rep.Clients[""].Requests; got != 1 {
		t.Errorf("the unnamed client made %d requests, want 1", got)
	}
	if got := rep.Clients["claude-code"].Requests; got != 3 {
		t.Errorf("claude-code made %d requests, want 3", got)
	}
	if got := rep.Models["keera-code"].Requests; got != 5 {
		t.Errorf("keera-code served %d requests, want 5", got)
	}

	var summed int64
	for _, c := range rep.Clients {
		summed += c.Requests
	}
	if summed != rep.Total.Requests {
		t.Errorf("the clients add up to %d and the total says %d", summed, rep.Total.Requests)
	}

	edges := map[string]int64{}
	for _, fl := range rep.Flows {
		edges[fl.Client+"->"+fl.Alias] = fl.Requests
	}
	if edges["claude-code->keera-code"] != 2 || edges["claude-code->keera-frontier"] != 1 {
		t.Errorf("edges = %v, want the pairs the traffic actually ran between", edges)
	}

	// The median is the one number that cannot be added up from the others,
	// which is why it is asked of the database at every level rather than
	// summed here. keera-code's four timed requests are 0, 200, 300 and 400ms,
	// and the zero is not a wait anybody had - it is a request that never
	// streamed - so the median of what is left is 300.
	if got := rep.Models["keera-code"].TTFTMedianMS; got != 300 {
		t.Errorf("keera-code's median first token = %dms, want 300", got)
	}
}

// Every window has a shape, including the empty one - and a map that cannot
// draw an empty deployment is a map nobody sees on their first morning.
func TestFlowsOverAnEmptyWindow(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)

	rep, err := st.Flows(ctx, f.orgID, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if rep.Total.Requests != 0 || len(rep.Flows) != 0 {
		t.Errorf("an empty window reported %+v", rep)
	}
	if rep.Clients == nil || rep.Models == nil {
		t.Error("the lookups must exist even when empty; the panel indexes into them")
	}
}

func TestASubscriptionKeyKeepsItsKindAndItsPlanUsage(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC().Truncate(time.Second)
	hash := []byte("hash-of-keera_sk_plan-32-bytes!!!")
	if _, err := st.CreateKey(ctx, KeyInfo{ID: "key_sub", OrgID: f.orgID, Alias: "claude-code",
		Prefix: "keera_sk_pla", Kind: policy.KeySubscription}, hash); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	res, err := st.LookupKey(ctx, hash)
	if err != nil || !res.Key.Subscription() {
		t.Fatalf("LookupKey = %+v, %v; want a subscription key", res, err)
	}

	plan := func(used float64, at time.Time) *policy.PlanUsage {
		return &policy.PlanUsage{FiveHour: &used, Status: "allowed", UpdatedAt: at}
	}
	// The newer reading arrives first, as it can from another replica, and the
	// older one must not replace it.
	for _, batch := range [][]Event{
		{{TS: now, OrgID: f.orgID, KeyID: "key_sub", Alias: "claude", Status: 200,
			ListCostMicros: 390, Plan: plan(0.4, now)}},
		{{TS: now.Add(-time.Minute), OrgID: f.orgID, KeyID: "key_sub", Alias: "claude",
			Status: 200, ListCostMicros: 10, Plan: plan(0.7, now.Add(-time.Minute))}},
	} {
		if err := st.WriteEvents(ctx, batch); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}

	keys, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID, Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("KeySummaries: %v", err)
	}
	var sub KeySummary
	for _, k := range keys {
		if k.ID == "key_sub" {
			sub = k
		}
	}
	switch {
	case sub.Kind != policy.KeySubscription:
		t.Errorf("kind = %q, want subscription", sub.Kind)
	case sub.SpendMicros != 0 || sub.SubscriptionMicros != 400:
		t.Errorf("spend %d, on the plan %d; want 0 and 400", sub.SpendMicros, sub.SubscriptionMicros)
	case sub.Plan == nil || sub.Plan.FiveHour == nil || *sub.Plan.FiveHour != 0.4:
		t.Errorf("plan = %+v, want the newer reading, 0.4", sub.Plan)
	}

	o, err := st.Overview(ctx, f.orgID, now.Add(-time.Hour), now.Add(time.Hour), Scope{})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if o.CostMicros != 0 || o.SubscriptionMicros != 400 {
		t.Errorf("overview spend %d, on plans %d; want 0 and 400", o.CostMicros, o.SubscriptionMicros)
	}

	// A rotated key is still one a copied settings file cannot spend money with.
	next, err := st.RotateKey(ctx, "key_sub", KeyInfo{ID: "key_sub2", Prefix: "keera_sk_pl2"},
		[]byte("hash-of-keera_sk_pl2-32-bytes!!!!"))
	if err != nil || next.Kind != policy.KeySubscription {
		t.Errorf("RotateKey = %+v, %v; want a subscription key", next, err)
	}
}
