package registry

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func scopes() []policy.Scope {
	return []policy.Scope{
		{Type: policy.ScopeOrg, ID: "org_1", BudgetMicros: 10_000_000, Period: policy.PeriodMonth},
		{Type: policy.ScopeProject, ID: "project_1", BudgetMicros: 1_000_000, Period: policy.PeriodDay},
	}
}

func TestAllowUntilTheTightestScopeIsSpent(t *testing.T) {
	b := newBudgets(false)
	now := time.Now()

	if err := b.Allow(scopes(), now); err != nil {
		t.Fatalf("a fresh budget refused a request: %v", err)
	}

	// Spend the project's daily allowance but not the org's monthly one.
	b.Charge(scopes(), 1_000_000, now)

	err := b.Allow(scopes(), now)
	var exceeded *policy.ErrBudgetExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if exceeded.Scope.Type != policy.ScopeProject {
		// The org still has room; the project is what ran out.
		t.Errorf("the wrong scope was blamed: %+v", exceeded.Scope)
	}
}

func TestScopesWithoutABudgetAreNeverBlocked(t *testing.T) {
	b := newBudgets(false)
	now := time.Now()
	free := []policy.Scope{{Type: policy.ScopeKey, ID: "key_1", Period: policy.PeriodMonth}}
	b.Charge(free, 999_999_999, now)
	if err := b.Allow(free, now); err != nil {
		t.Errorf("a scope with no budget was blocked: %v", err)
	}
}

func TestChargesLandInTheRightPeriodWindow(t *testing.T) {
	b := newBudgets(false)
	sc := scopes()

	sep := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)

	b.Charge(sc, 9_000_000, sep)
	if got := b.Spent(policy.ScopeOrg, "org_1", policy.PeriodMonth, sep); got != 9_000_000 {
		t.Errorf("September spend = %d, want 9000000", got)
	}
	// Crossing into October must reset the monthly window rather than carry it.
	if got := b.Spent(policy.ScopeOrg, "org_1", policy.PeriodMonth, oct); got != 0 {
		t.Errorf("October opened with %d already spent", got)
	}
	if err := b.Allow(sc, oct); err != nil {
		t.Errorf("the new period is still blocked by the old one: %v", err)
	}
}

func TestReconcileReplacesLocalSpendWithTheDatabase(t *testing.T) {
	// Another replica has been spending too. Reconciling adopts the shared
	// figure and forgets the local delta, which the recorder has written.
	b := newBudgets(false)
	now := time.Now()
	b.Charge(scopes(), 500_000, now)

	b.reconcile([]store.SpendRow{{
		ScopeType:   policy.ScopeProject,
		ScopeID:     "project_1",
		Period:      policy.PeriodDay,
		PeriodStart: policy.PeriodDay.Start(now),
		Micros:      900_000,
	}})

	if got := b.Spent(policy.ScopeProject, "project_1", policy.PeriodDay, now); got != 900_000 {
		t.Errorf("spend after reconcile = %d, want the database's 900000", got)
	}
	// The org row was not in the database, so its window is gone rather than
	// stale.
	if got := b.Spent(policy.ScopeOrg, "org_1", policy.PeriodMonth, now); got != 0 {
		t.Errorf("org spend = %d, want 0", got)
	}
}

func TestConcurrentChargeAndAllow(_ *testing.T) {
	// Run with -race: both are on the hot path.
	b := newBudgets(false)
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 500 {
				now := time.Now()
				_ = b.Allow(scopes(), now)
				b.Charge(scopes(), 1, now)
			}
		}()
	}
	for range 8 {
		<-done
	}
}

// With payments, an organisation without credit cannot use the deployment's
// keys until it pays, unless it is billed by invoice.
func TestAnOrganisationWithoutCreditIsRefused(t *testing.T) {
	b := newBudgets(true)
	_, err := b.HoldCredit("org_new", 0)
	if _, ok := errors.AsType[*policy.ErrNoCredit](err); !ok {
		t.Error("an organisation that never paid was let in")
	}
	b.reconcileCredit([]store.CreditRow{
		{OrgID: "org_paid", BalanceMicros: 5_000_000},
		{OrgID: "org_invoiced", BalanceMicros: -20_000_000, Invoiced: true},
	})
	if _, err := b.HoldCredit("org_paid", 0); err != nil {
		t.Errorf("an organisation with CHF 5.00 was refused: %v", err)
	}
	if _, err := b.HoldCredit("org_invoiced", 0); err != nil {
		t.Errorf("an organisation billed by invoice was refused: %v", err)
	}
	// Spent locally before the next reconcile.
	b.ChargeCredit("org_paid", 5_000_000)
	_, err = b.HoldCredit("org_paid", 0)
	if e, ok := errors.AsType[*policy.ErrNoCredit](err); !ok || e.BalanceMicros != 0 {
		t.Errorf("after spending it all: %v", err)
	}
}

func TestWithoutPaymentsCreditIsNotChecked(t *testing.T) {
	b := newBudgets(false)
	b.ChargeCredit("org_a", 1_000_000)
	if _, err := b.HoldCredit("org_a", 0); err != nil {
		t.Errorf("refused without payments: %v", err)
	}
}

// Requests started together each hold what they may cost, so they cannot all
// spend the same credit. A reconcile does not drop the holds: those requests
// have not been charged yet.
func TestRunningRequestsHoldTheCredit(t *testing.T) {
	b := newBudgets(true)
	b.reconcileCredit([]store.CreditRow{{OrgID: "org_a", BalanceMicros: 5_000_000}})

	first, err := b.HoldCredit("org_a", 3_000_000)
	if err != nil || first != 3_000_000 {
		t.Fatalf("first hold = %d, %v", first, err)
	}
	// Some credit is still free, so the second request starts, and may go over.
	second, err := b.HoldCredit("org_a", 3_000_000)
	if err != nil {
		t.Fatalf("second hold: %v", err)
	}
	b.reconcileCredit([]store.CreditRow{{OrgID: "org_a", BalanceMicros: 5_000_000}})
	_, err = b.HoldCredit("org_a", 0)
	if e, ok := errors.AsType[*policy.ErrNoCredit](err); !ok || e.HeldMicros != 6_000_000 {
		t.Fatalf("with all of it held: %v", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("message = %q, want it to say why", err)
	}

	b.ReleaseCredit("org_a", first)
	if _, err := b.HoldCredit("org_a", 0); err != nil {
		t.Errorf("after one request ended: %v", err)
	}
	b.ReleaseCredit("org_a", second)
	if len(b.held) != 0 {
		t.Errorf("held = %v, want nothing left", b.held)
	}
}

func TestAnInvoicedOrganisationHoldsNothing(t *testing.T) {
	b := newBudgets(true)
	b.reconcileCredit([]store.CreditRow{{OrgID: "org_a", Invoiced: true}})
	if held, err := b.HoldCredit("org_a", 3_000_000); err != nil || held != 0 {
		t.Errorf("hold = %d, %v; want nothing held", held, err)
	}
	if held, _ := newBudgets(false).HoldCredit("org_a", 3_000_000); held != 0 {
		t.Errorf("without payments, hold = %d", held)
	}
}

// A router's daily limit counts requests or tokens, not money, so a charge
// of money leaves it alone and a tally of requests leaves the budgets alone.
func TestTallyCountsOnlyItsOwnMeter(t *testing.T) {
	b := newBudgets(false)
	now := time.Now()
	budget := policy.Scope{Type: policy.ScopeRouter, ID: "org_1/safe/cap", Period: policy.PeriodDay}
	requests := policy.Scope{Type: policy.ScopeRouter, ID: "org_1/safe/daily|requests",
		Period: policy.PeriodDay, Meter: policy.MeterRequests, Quota: 10}
	both := []policy.Scope{budget, requests}

	b.Charge(both, 5_000, now)
	b.Tally(both, policy.MeterRequests, 1, now)
	b.Tally(both, policy.MeterRequests, 1, now)

	if got := b.Spent(budget.Type, budget.ID, budget.Period, now); got != 5_000 {
		t.Errorf("the budget spent %d, want only the money", got)
	}
	if got := b.Spent(requests.Type, requests.ID, requests.Period, now); got != 2 {
		t.Errorf("the limit counted %d requests, want 2", got)
	}
}

// The database counts a request only once its row is written, so a reconcile
// while a request runs must not forget it, or long streams would let a daily
// limit be passed again every refresh.
func TestReconcileKeepsRequestsStillRunning(t *testing.T) {
	b := newBudgets(false)
	now := time.Now()
	limit := []policy.Scope{{Type: policy.ScopeKey, ID: "key_1|requests",
		Period: policy.PeriodDay, Meter: policy.MeterRequests, Quota: 10}}
	spent := func() int64 { return b.Spent(policy.ScopeKey, "key_1|requests", policy.PeriodDay, now) }

	b.StartRequest(limit, now)
	b.StartRequest(limit, now)
	b.reconcile(nil)
	if got := spent(); got != 2 {
		t.Fatalf("after a reconcile, %d requests counted, want the 2 still running", got)
	}

	// One ends; until its row is read back, the local view counts it.
	b.EndRequest(limit, now)
	if got := spent(); got != 2 {
		t.Errorf("after one ended, %d requests counted, want 2", got)
	}
	// The database now has its row, and the other is still running.
	b.reconcile([]store.SpendRow{{ScopeType: policy.ScopeKey, ScopeID: "key_1|requests",
		Period: policy.PeriodDay, PeriodStart: policy.PeriodDay.Start(now), Micros: 1}})
	if got := spent(); got != 2 {
		t.Errorf("after the row was read, %d requests counted, want 2", got)
	}
	b.EndRequest(limit, now)
	b.reconcile([]store.SpendRow{{ScopeType: policy.ScopeKey, ScopeID: "key_1|requests",
		Period: policy.PeriodDay, PeriodStart: policy.PeriodDay.Start(now), Micros: 2}})
	if got := spent(); got != 2 {
		t.Errorf("once both rows were read, %d requests counted, want 2", got)
	}
}

// A stream that started before midnight ends in yesterday's window, and must
// not wipe out today's.
func TestARequestEndingInANewWindowLeavesItAlone(t *testing.T) {
	b := newBudgets(false)
	yesterday := time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC)
	today := yesterday.Add(2 * time.Minute)
	limit := []policy.Scope{{Type: policy.ScopeKey, ID: "key_1|requests",
		Period: policy.PeriodDay, Meter: policy.MeterRequests, Quota: 10}}

	b.StartRequest(limit, yesterday)
	b.StartRequest(limit, today)
	b.EndRequest(limit, today)
	b.EndRequest(limit, yesterday)

	if got := b.Spent(policy.ScopeKey, "key_1|requests", policy.PeriodDay, today); got != 1 {
		t.Errorf("today counts %d requests, want 1", got)
	}
}
