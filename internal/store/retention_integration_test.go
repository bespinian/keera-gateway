package store

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestRetentionDeletesPastTheWindowAndNothingInsideIt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC()

	// recent is now, not a day ago: on the 1st, a day ago is last month, and
	// its spend window is not the open one this test checks survives.
	old, recent := now.AddDate(0, 0, -400), now
	for _, ts := range []time.Time{old, recent} {
		if err := st.WriteEvents(ctx, []Event{{TS: ts, OrgID: f.orgID, Alias: "keera-code",
			CostMicros: 100, Status: 200,
			Scopes: []policy.Scope{{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodMonth}},
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
		if err := st.Audit(ctx, "operator key", f.orgID, "key.create", "key", "key_1", nil); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}
	// The audit rows both defaulted to now(), so one is backdated by hand.
	if _, err := st.pool.Exec(ctx,
		"UPDATE audit_log SET ts = $1 WHERE id = (SELECT min(id) FROM audit_log)", old); err != nil {
		t.Fatal(err)
	}

	cutoff := now.AddDate(0, 0, -365)
	deleted, err := st.PurgeUsage(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeUsage: %v", err)
	}
	// One event, and the closed spend window that went with it.
	if deleted != 2 {
		t.Errorf("PurgeUsage deleted %d rows, want 2 (one event and its spend window)", deleted)
	}
	var events int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("%d events left, want the one inside the window", events)
	}
	// The open window is what budgets are checked against, so it must survive.
	rows, err := st.LoadSpend(ctx, now)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	if len(rows) != 1 || rows[0].Micros != 100 {
		t.Errorf("LoadSpend = %+v, want the open window intact", rows)
	}

	n, err := st.PurgeAudit(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeAudit: %v", err)
	}
	if n != 1 {
		t.Errorf("PurgeAudit deleted %d, want 1", n)
	}
	entries, err := st.ListAudit(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%d audit entries left, want the one inside the window", len(entries))
	}

	// Running it again deletes nothing rather than failing.
	if n, err := st.PurgeUsage(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("a second PurgeUsage = %d, %v; want 0, nil", n, err)
	}
	if n, err := st.PurgeAudit(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("a second PurgeAudit = %d, %v; want 0, nil", n, err)
	}
}

func TestRetentionWorksInMoreThanOneBatch(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	old := time.Now().UTC().AddDate(0, 0, -400)

	// More than one batch, so the loop that keeps a DELETE short is exercised
	// rather than assumed.
	const rows = purgeBatch + 17
	batch := make([]Event, 0, rows)
	for range rows {
		batch = append(batch, Event{TS: old, OrgID: f.orgID, Alias: "keera-code", Status: 200})
	}
	if err := st.WriteEvents(ctx, batch); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	deleted, err := st.PurgeUsage(ctx, time.Now().UTC().AddDate(0, 0, -365))
	if err != nil {
		t.Fatalf("PurgeUsage: %v", err)
	}
	if deleted != rows {
		t.Errorf("PurgeUsage deleted %d rows, want all %d", deleted, rows)
	}
	var left int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d events survived, want 0", left)
	}
}

// A short retention must not take the open month with it: on the 20th with
// seven days kept, the month began before the cutoff but has not ended.
func TestRetentionKeepsTheOpenMonthWhenItBeganBeforeTheCutoff(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	for _, ts := range []time.Time{
		time.Date(2026, 2, 27, 9, 0, 0, 0, time.UTC), // last month: closed
		time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),  // this month, before the cutoff
		now,
	} {
		if err := st.WriteEvents(ctx, []Event{{TS: ts, OrgID: f.orgID, Alias: "keera-code",
			CostMicros: 100, Status: 200,
			Scopes: []policy.Scope{
				{Type: policy.ScopeOrg, ID: f.orgID, Period: policy.PeriodMonth},
				{Type: policy.ScopeProject, ID: "project_1", Period: policy.PeriodDay},
			},
		}}); err != nil {
			t.Fatalf("WriteEvents: %v", err)
		}
	}

	if _, err := st.PurgeUsage(ctx, now.AddDate(0, 0, -7)); err != nil {
		t.Fatalf("PurgeUsage: %v", err)
	}
	rows, err := st.LoadSpend(ctx, now)
	if err != nil {
		t.Fatalf("LoadSpend: %v", err)
	}
	var month, day int64
	for _, r := range rows {
		switch r.Period {
		case policy.PeriodMonth:
			month = r.Micros
		case policy.PeriodDay:
			day = r.Micros
		}
	}
	// Both of March's events count, although one is past the cutoff.
	if month != 200 || day != 100 {
		t.Errorf("open windows = month %d, day %d; want 200 and 100", month, day)
	}
	var windows int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM spend").Scan(&windows); err != nil {
		t.Fatal(err)
	}
	// March's month and today's day survive; February and the 2nd go.
	if windows != 2 {
		t.Errorf("%d spend windows left, want 2", windows)
	}
}

func TestEndedSandboxesAndKeysGoOnceTheirUsageHasGone(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	newSandboxClass(t, st, ctx, f.orgID, "standard")
	now := time.Now().UTC()
	old, cutoff := now.AddDate(0, 0, -400), now.AddDate(0, 0, -365)

	key := func(id, ended string) {
		t.Helper()
		if _, err := st.CreateKey(ctx, KeyInfo{ID: id, OrgID: f.orgID, ProjectID: f.projectID,
			Name: id, Prefix: id}, []byte("hash-of-"+id)); err != nil {
			t.Fatalf("CreateKey %s: %v", id, err)
		}
		if ended == "" {
			return
		}
		if _, err := st.pool.Exec(ctx, "UPDATE api_keys SET "+ended+" = $2 WHERE id = $1",
			id, old); err != nil {
			t.Fatal(err)
		}
	}
	key("key_revoked", "revoked_at")
	key("key_expired", "expires_at")
	key("key_of_suspended", "expires_at") // an old sandbox's, which may yet be revived
	key("key_of_expired", "expires_at")
	key("key_of_failed_agent", "revoked_at")
	key("key_of_failed_engineer", "revoked_at")
	key("key_live", "")
	// A guardrail and a spend row on a key name it by id alone.
	if _, err := st.pool.Exec(ctx, `INSERT INTO guardrails (scope_type, scope_id) VALUES ('key', 'key_revoked');
		INSERT INTO spend (scope_type, scope_id, period, period_start) VALUES ('key', 'key_revoked', 'month', now())`,
	); err != nil {
		t.Fatal(err)
	}

	// sandbox makes one created at, and for a terminated one also ended at, at.
	sandbox := func(name, keyID string, state policy.SandboxState, purpose policy.Purpose, at time.Time) {
		t.Helper()
		sb := newSandbox(t, st, ctx, f, name)
		if _, err := st.pool.Exec(ctx, `UPDATE sandboxes SET key_id = $2, state = $3, purpose = $4,
			created_at = $5, terminated_at = CASE WHEN $3 = 'terminated' THEN $5::timestamptz END
			WHERE id = $1`, sb.ID, keyID, string(state), string(purpose), at); err != nil {
			t.Fatal(err)
		}
	}
	engineer, agent := policy.PurposeEngineer, policy.PurposeAgent
	sandbox("gone", "key_expired", policy.SandboxTerminated, engineer, old)
	sandbox("recent", "key_live", policy.SandboxTerminated, engineer, now)
	sandbox("resting", "key_of_suspended", policy.SandboxSuspended, engineer, old)
	// An expired one can be resumed, and a failed engineer one keeps its
	// volume until it is terminated, so both stay however old they are.
	sandbox("lapsed", "key_of_expired", policy.SandboxExpired, engineer, old)
	sandbox("broken-dev", "key_of_failed_engineer", policy.SandboxFailed, engineer, old)
	// A failed agent sandbox has ended. It has no end time, so its age counts.
	sandbox("broken-task", "key_of_failed_agent", policy.SandboxFailed, agent, old)
	sandbox("broken-today", "key_live", policy.SandboxFailed, agent, now)

	n, err := st.PurgeEnded(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeEnded: %v", err)
	}
	if n != 5 {
		t.Errorf("PurgeEnded deleted %d rows, want 5: two sandboxes and three keys", n)
	}

	left := func(query string) []string {
		t.Helper()
		rows, err := st.pool.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	wantSandboxes := []string{"broken-dev", "broken-today", "lapsed", "recent", "resting"}
	if got := left("SELECT name FROM sandboxes ORDER BY name"); !slices.Equal(got, wantSandboxes) {
		t.Errorf("sandboxes left: %v, want %v", got, wantSandboxes)
	}
	want := []string{f.keyID, "key_live", "key_of_expired", "key_of_failed_engineer", "key_of_suspended"}
	if got := left("SELECT id FROM api_keys ORDER BY id"); !slices.Equal(got, want) {
		t.Errorf("keys left: %v, want %v", got, want)
	}
	if got := left("SELECT scope_id FROM guardrails WHERE scope_type = 'key' UNION ALL " +
		"SELECT scope_id FROM spend WHERE scope_type = 'key'"); len(got) != 0 {
		t.Errorf("rows still name a deleted key: %v", got)
	}
}
