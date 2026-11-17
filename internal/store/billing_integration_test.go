package store

import (
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// Keera Agents sends a line again when it did not hear back. The line is
// recorded and taken from the credit once.
func TestServiceLinesAreRecordedAndChargedOnce(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	lines := []ServiceLine{
		{
			ID: "agents/org_1/h/cpu", OrgID: f.orgID, TS: hour, Item: "CPU", Unit: "CPU-hours",
			QuantityMicros: 2_000_000, Micros: 100_000,
		},
		{
			ID: "agents/org_1/h/memory", OrgID: f.orgID, TS: hour, Item: "Memory", Unit: "GiB-hours",
			QuantityMicros: 4_000_000, Micros: 40_000,
		},
		// An organisation deleted since is still billed.
		{
			ID: "agents/org_gone/h/cpu", OrgID: "org_gone", TS: hour, Item: "CPU", Unit: "CPU-hours",
			QuantityMicros: 1_000_000, Micros: 50_000,
		},
	}
	added, err := st.AddServiceLines(ctx, ServiceAgents, lines, true)
	if err != nil || added != 3 {
		t.Fatalf("AddServiceLines = %d, %v; want 3 added", added, err)
	}
	// Sent again, with one new line for the next hour.
	next := ServiceLine{
		ID: "agents/org_1/h2/cpu", OrgID: f.orgID, TS: hour.Add(time.Hour), Item: "CPU",
		Unit: "CPU-hours", QuantityMicros: 1_000_000, Micros: 50_000,
	}
	added, err = st.AddServiceLines(ctx, ServiceAgents, append(lines, next), true)
	if err != nil || added != 1 {
		t.Fatalf("sent again: %d, %v; want only the new line added", added, err)
	}
	if got := balance(t, st, f.orgID); got != -190_000 {
		t.Errorf("balance = %d, want CHF -0.19, taken once", got)
	}
	if got := balance(t, st, "org_gone"); got != -50_000 {
		t.Errorf("deleted organisation's balance = %d, want CHF -0.05", got)
	}

	rows, err := st.Billing(ctx, f.orgID, hour.Add(-time.Hour), hour.Add(3*time.Hour))
	if err != nil || len(rows) != 2 {
		t.Fatalf("Billing = %+v, %v; want a row per item", rows, err)
	}
	cpu := rows[0]
	if cpu.Service != ServiceAgents || cpu.Item != "CPU" || cpu.Unit != "CPU-hours" ||
		cpu.QuantityMicros != 3_000_000 || cpu.BilledMicros != 150_000 ||
		cpu.CreditMicros != 150_000 || cpu.Calls != 0 || cpu.OrgName != "Example Bank" {
		t.Errorf("CPU row = %+v", cpu)
	}
	if rows[1].Item != "Memory" || rows[1].QuantityMicros != 4_000_000 {
		t.Errorf("memory row = %+v", rows[1])
	}
}

// Without payments the lines are only on the bill, for an invoice: nothing is
// taken from credit.
func TestServiceLinesWithoutPaymentsLeaveTheCredit(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	added, err := st.AddServiceLines(ctx, ServiceAgents, []ServiceLine{{
		ID: "agents/org_1/h/disk", OrgID: f.orgID, TS: time.Now().Add(-time.Hour), Item: "Disk",
		Unit: "GiB-months", QuantityMicros: 7_000, Micros: 700,
	}}, false)
	if err != nil || added != 1 {
		t.Fatalf("AddServiceLines = %d, %v", added, err)
	}
	if got := balance(t, st, f.orgID); got != 0 {
		t.Errorf("balance = %d, want it untouched", got)
	}
	rows, err := st.Billing(ctx, f.orgID, time.Now().Add(-2*time.Hour), time.Now())
	if err != nil || len(rows) != 1 || rows[0].BilledMicros != 700 || rows[0].CreditMicros != 0 {
		t.Errorf("Billing = %+v, %v; want it billed, not from credit", rows, err)
	}
}

func TestCreditRowOfOneOrganisation(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	c, err := st.CreditRowOf(ctx, f.orgID)
	if err != nil || c.OrgID != f.orgID || c.BalanceMicros != 0 || !c.ChargeDueAt.IsZero() {
		t.Fatalf("no account: %+v, %v; want a zero row", c, err)
	}
	if _, err := st.Grant(ctx, Payment{
		ID: id.New("pay"), OrgID: f.orgID, AmountMicros: 2_000_000, Actor: "operator",
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	today := time.Now().UTC().Format(time.DateOnly)
	if _, err := st.CreateCharge(ctx, RecurringCharge{
		ID: id.New("chg"), OrgID: f.orgID, Name: "Dedicated model", AmountMicros: 1_000_000,
		Every: "month", StartsOn: today, Actor: "operator",
	}); err != nil {
		t.Fatalf("CreateCharge: %v", err)
	}
	c, err = st.CreditRowOf(ctx, f.orgID)
	if err != nil || c.BalanceMicros != 2_000_000 || c.ChargeDueAt.IsZero() {
		t.Errorf("with credit and a charge no card pays: %+v, %v", c, err)
	}
}
