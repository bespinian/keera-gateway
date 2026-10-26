package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// dailyTraffic is two UTC days of one organisation's requests, written in two
// batches so the second adds to the rows of the first.
func dailyTraffic(t *testing.T, st *Store, f fixture, day time.Time) {
	t.Helper()
	ctx := t.Context()
	event := func(ts time.Time, alias, client string, status int, cost int64) Event {
		return Event{
			TS: ts, OrgID: f.orgID, ProjectID: f.projectID, KeyID: f.keyID,
			Alias: alias, Client: client, Status: status,
			InputTokens: 100, OutputTokens: 20, CachedInputTokens: 40, CacheWriteTokens: 10,
			CostMicros: cost, ListCostMicros: cost / 2,
		}
	}
	batches := [][]Event{{
		event(day.Add(time.Hour), "keera-code", "claude-code", 200, 1000),
		event(day.Add(2*time.Hour), "keera-code", "claude-code", 429, 0),
		event(day.Add(3*time.Hour), "keera-large", "", 502, 50),
		// A tool call and a check's bills are not requests.
		{
			TS: day.Add(time.Hour), OrgID: f.orgID, KeyID: f.keyID, Alias: "keera-code",
			Status: 200, Tool: &ToolCall{Server: "git", Tool: "log", Outcome: ToolOK},
		},
		{
			TS: day.Add(time.Hour), OrgID: f.orgID, Alias: "keera-code", BillsOnly: true,
			CostMicros: 7,
		},
	}, {
		event(day.Add(4*time.Hour), "keera-code", "claude-code", 200, 3000),
		event(day.Add(26*time.Hour), "keera-code", "opencode", 200, 500),
	}}
	for _, b := range batches {
		if err := st.WriteEvents(ctx, b); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}
}

func TestDailyTotalsAgreeWithTheRequestLog(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	dailyTraffic(t, st, f, day)

	// Three whole days read usage_daily; each single day reads the log. Every
	// grouping must add up the same.
	for _, groupBy := range GroupBys() {
		whole, err := st.Usage(ctx, UsageQuery{
			OrgID: f.orgID, From: day, To: day.AddDate(0, 0, 3),
			GroupBy: groupBy,
		})
		if err != nil {
			t.Fatalf("Usage by %s: %v", groupBy, err)
		}
		sums := map[string]UsageBucket{}
		for d := range 3 {
			from := day.AddDate(0, 0, d)
			part, err := st.Usage(ctx, UsageQuery{
				OrgID: f.orgID, From: from, To: from.AddDate(0, 0, 1),
				GroupBy: groupBy,
			})
			if err != nil {
				t.Fatalf("Usage by %s on day %d: %v", groupBy, d, err)
			}
			for _, b := range part {
				s := sums[b.Group]
				s.Group, s.OrgID = b.Group, b.OrgID
				s.Requests += b.Requests
				s.InputTokens += b.InputTokens
				s.OutputTokens += b.OutputTokens
				s.CostMicros += b.CostMicros
				s.SubscriptionMicros += b.SubscriptionMicros
				sums[b.Group] = s
			}
		}
		if len(whole) != len(sums) {
			t.Errorf("by %s: %d groups from the daily totals, %d from the log: %+v vs %+v",
				groupBy, len(whole), len(sums), whole, sums)
			continue
		}
		for _, b := range whole {
			if b != sums[b.Group] {
				t.Errorf("by %s: daily %+v, log %+v", groupBy, b, sums[b.Group])
			}
		}
	}

	o, err := st.Overview(ctx, f.orgID, day, day.AddDate(0, 0, 3), ReportScope{})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if o.Requests != 5 || o.Refused != 1 || o.Failed != 1 || o.CostMicros != 4550 {
		t.Errorf("Overview = %d requests, %d refused, %d failed, %d micros; "+
			"want 5, 1, 1 and 4550", o.Requests, o.Refused, o.Failed, o.CostMicros)
	}
	if len(o.Series) != 3 || o.Series[0].Requests != 4 || o.Series[1].Requests != 1 ||
		o.Series[0].Failed != 1 {
		t.Errorf("Series = %+v, want three days: 4 requests with 1 error, then 1", o.Series)
	}
}

func TestTheUpgradeFillsTheDailyTotalsAsTheGatewayWould(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	dailyTraffic(t, st, f, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC))

	read := func() []string {
		rows, err := queryAll(ctx, st.pool, func(r row) (string, error) {
			var s string
			return s, r.Scan(&s)
		}, `SELECT u::text FROM usage_daily u
			ORDER BY day, org_id, key_id, alias, client, project_id, user_id`)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	written := read()

	body, err := migrations.ReadFile("migrations/0007_v0.10.0.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	start := strings.Index(sql, "INSERT INTO usage_daily")
	backfill := sql[start : start+strings.Index(sql[start:], ";")]
	if _, err := st.pool.Exec(ctx, "TRUNCATE usage_daily"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if filled := read(); !slices.Equal(filled, written) {
		t.Errorf("the upgrade filled\n%v\nthe gateway wrote\n%v", filled, written)
	}
}

func TestReportsOutliveTheRequestLog(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -10)
	if err := st.WriteEvents(ctx, []Event{{
		TS: old, OrgID: f.orgID, ProjectID: f.projectID,
		KeyID: f.keyID, Alias: "keera-code", Status: 200, CostMicros: 100,
	}}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	if _, err := st.PurgeUsage(ctx, now.AddDate(0, 0, -2)); err != nil {
		t.Fatalf("PurgeUsage: %v", err)
	}

	month := now.AddDate(0, 0, -30)
	buckets, err := st.Usage(ctx, UsageQuery{OrgID: f.orgID, From: month, To: now})
	if err != nil || len(buckets) != 1 || buckets[0].Requests != 1 {
		t.Errorf("Usage over 30 days = %+v, %v; want the request retention removed", buckets, err)
	}
	o, err := st.Overview(ctx, f.orgID, month, now, ReportScope{})
	if err != nil || o.Requests != 1 || o.CostMicros != 100 {
		t.Errorf("Overview over 30 days = %d requests, %d micros, %v; want 1 and 100",
			o.Requests, o.CostMicros, err)
	}
	c, err := st.UsageChart(ctx, UsageQuery{OrgID: f.orgID, From: month, To: now})
	if err != nil || len(c.Lines) != 1 || c.Lines[0].Total != 1 {
		t.Errorf("UsageChart over 30 days = %+v, %v; want one line of 1", c, err)
	}
	keys, err := st.KeySummaries(ctx, KeyQuery{OrgID: f.orgID, Since: month})
	if err != nil || len(keys) != 1 || keys[0].Requests != 1 || keys[0].LastUsedAt == nil ||
		!keys[0].LastUsedAt.Equal(old.Truncate(time.Microsecond)) {
		t.Errorf("KeySummaries = %+v, %v; want 1 request, last used 10 days ago", keys, err)
	}
	if used, err := st.AnyUsed(ctx, f.orgID, []string{f.keyID}); err != nil || !used {
		t.Errorf("AnyUsed = %v, %v; want the key used", used, err)
	}
	if setup, err := st.SetupState(ctx, f.orgID); err != nil || setup.Requests != 1 {
		t.Errorf("SetupState = %+v, %v; want something served", setup, err)
	}

	// Two days or less reads the request log, which no longer has it.
	short, err := st.Usage(ctx, UsageQuery{
		OrgID: f.orgID, From: old.Add(-time.Hour),
		To: old.Add(time.Hour),
	})
	if err != nil || len(short) != 0 {
		t.Errorf("Usage over two hours = %+v, %v; want nothing", short, err)
	}
}
