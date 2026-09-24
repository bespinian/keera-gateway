package registry

import (
	"errors"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func scopes() []policy.Scope {
	return []policy.Scope{
		{Type: policy.ScopeOrg, ID: "org_1", BudgetMicros: 10_000_000, Period: policy.PeriodMonth},
		{Type: policy.ScopeTeam, ID: "team_1", BudgetMicros: 1_000_000, Period: policy.PeriodDay},
	}
}

func TestAllowUntilTheTightestScopeIsSpent(t *testing.T) {
	b := newBudgets()
	now := time.Now()

	if err := b.Allow(scopes(), now); err != nil {
		t.Fatalf("a fresh budget refused a request: %v", err)
	}

	// Spend the team's daily allowance but not the org's monthly one.
	b.Charge(scopes(), 1_000_000, now)

	err := b.Allow(scopes(), now)
	var exceeded *policy.ErrBudgetExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if exceeded.Scope.Type != policy.ScopeTeam {
		// The org still has room; the team is what ran out.
		t.Errorf("the wrong scope was blamed: %+v", exceeded.Scope)
	}
}

func TestScopesWithoutABudgetAreNeverBlocked(t *testing.T) {
	b := newBudgets()
	now := time.Now()
	free := []policy.Scope{{Type: policy.ScopeKey, ID: "key_1", Period: policy.PeriodMonth}}
	b.Charge(free, 999_999_999, now)
	if err := b.Allow(free, now); err != nil {
		t.Errorf("a scope with no budget was blocked: %v", err)
	}
}

func TestChargesLandInTheRightPeriodWindow(t *testing.T) {
	b := newBudgets()
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
	b := newBudgets()
	now := time.Now()
	b.Charge(scopes(), 500_000, now)

	b.reconcile([]store.SpendRow{{
		ScopeType:   policy.ScopeTeam,
		ScopeID:     "team_1",
		Period:      policy.PeriodDay,
		PeriodStart: policy.PeriodDay.Start(now),
		Micros:      900_000,
	}})

	if got := b.Spent(policy.ScopeTeam, "team_1", policy.PeriodDay, now); got != 900_000 {
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
	b := newBudgets()
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
