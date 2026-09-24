package store

import (
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestRetentionDeletesPastTheWindowAndNothingInsideIt(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	now := time.Now().UTC()

	old, recent := now.AddDate(0, 0, -400), now.AddDate(0, 0, -1)
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
