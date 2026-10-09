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
	// figure and forgets the local delta, which the recorder has by then
	// written.
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
	if _, ok := errors.AsType[*policy.ErrNoCredit](b.AllowCredit("org_new")); !ok {
		t.Error("an organisation that never paid was let in")
	}
	b.reconcileCredit([]store.CreditRow{
		{OrgID: "org_paid", BalanceMicros: 5_000_000},
		{OrgID: "org_invoiced", BalanceMicros: -20_000_000, Invoiced: true},
	})
	if err := b.AllowCredit("org_paid"); err != nil {
		t.Errorf("an organisation with CHF 5.00 was refused: %v", err)
	}
	if err := b.AllowCredit("org_invoiced"); err != nil {
		t.Errorf("an organisation billed by invoice was refused: %v", err)
	}
	// Spent locally before the next reconcile.
	b.ChargeCredit("org_paid", 5_000_000)
	err := b.AllowCredit("org_paid")
	if e, ok := errors.AsType[*policy.ErrNoCredit](err); !ok || e.BalanceMicros != 0 {
		t.Errorf("after spending it all: %v", err)
	}
}

func TestWithoutPaymentsCreditIsNotChecked(t *testing.T) {
	b := newBudgets(false)
	b.ChargeCredit("org_a", 1_000_000)
	if err := b.AllowCredit("org_a"); err != nil {
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
	err = b.AllowCredit("org_a")
	if e, ok := errors.AsType[*policy.ErrNoCredit](err); !ok || e.HeldMicros != 6_000_000 {
		t.Fatalf("with all of it held: %v", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("message = %q, want it to say why", err)
	}

	b.ReleaseCredit("org_a", first)
	if err := b.AllowCredit("org_a"); err != nil {
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
