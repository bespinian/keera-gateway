package registry

import (
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// window is one budget period's spend: the last figure read from the
// database, plus what this replica charged since.
type window struct {
	start  time.Time
	micros int64
}

// Budgets is the gateway's in-memory view of spend, reconciled with the
// database every few seconds so the check before each request needs no round
// trip.
//
// The price: across replicas a budget can be overshot by about one refresh
// interval of traffic. Being exact would cost a round trip per completion.
type Budgets struct {
	mu sync.RWMutex
	w  map[string]*window

	// prepaid says organisations pay for the deployment's provider keys in
	// advance, and credit is each one's balance. The same refresh keeps it,
	// with the same overshoot.
	prepaid bool
	credit  map[string]credit
	// held is what requests still running on this replica may take from
	// each organisation's credit. A reconcile leaves it alone: those
	// requests have not been charged yet.
	held map[string]int64
}

// credit is what the gateway knows of an organisation's credit account.
type credit struct {
	balance  int64
	invoiced bool
}

func newBudgets(prepaid bool) *Budgets {
	return &Budgets{w: make(map[string]*window), prepaid: prepaid, credit: map[string]credit{},
		held: map[string]int64{}}
}

func windowKey(t policy.ScopeType, id string, p policy.Period) string {
	return string(t) + "\x00" + id + "\x00" + string(p)
}

// Allow reports whether every scope on a resolved key is within its budget.
func (b *Budgets) Allow(scopes []policy.Scope, now time.Time) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sc := range scopes {
		if sc.BudgetMicros <= 0 {
			continue
		}
		if spent := b.spent(sc.Type, sc.ID, sc.Period, now); spent >= sc.BudgetMicros {
			return &policy.ErrBudgetExceeded{
				Scope: sc, Spent: spent, Budget: sc.BudgetMicros,
				ResetsAt: sc.Period.Next(now),
			}
		}
	}
	return nil
}

// Charge folds the cost of a completed request into the local view. The usage
// recorder writes the real row; this keeps the next few seconds honest.
func (b *Budgets) Charge(scopes []policy.Scope, micros int64, now time.Time) {
	if micros == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sc := range scopes {
		start := sc.Period.Start(now)
		k := windowKey(sc.Type, sc.ID, sc.Period)
		w, ok := b.w[k]
		if !ok || !w.start.Equal(start) {
			w = &window{start: start}
			b.w[k] = w
		}
		w.micros += micros
	}
}

// reconcile replaces every window with the database's figure and forgets the
// local charges, which the recorder has written by then. A charge still in the
// recorder's buffer is briefly uncounted until the next reconcile.
func (b *Budgets) reconcile(rows []store.SpendRow) {
	next := make(map[string]*window, len(rows))
	for _, r := range rows {
		next[windowKey(r.ScopeType, r.ScopeID, r.Period)] = &window{
			start:  r.PeriodStart.UTC(),
			micros: r.Micros,
		}
	}
	b.mu.Lock()
	b.w = next
	b.mu.Unlock()
}

// Spent returns the current view of one window, for the control API and the
// budget headers.
func (b *Budgets) Spent(t policy.ScopeType, id string, p policy.Period, now time.Time) int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.spent(t, id, p, now)
}

// spent is zero when nothing was spent in the open window. The caller holds
// the lock.
func (b *Budgets) spent(t policy.ScopeType, id string, p policy.Period, now time.Time) int64 {
	w, ok := b.w[windowKey(t, id, p)]
	if !ok || !w.start.Equal(p.Start(now)) {
		return 0
	}
	return w.micros
}

// AllowCredit reports whether an organisation may use the deployment's
// provider keys: it has credit left that no running request holds, or is
// billed by invoice. Without payments, every organisation may.
func (b *Budgets) AllowCredit(orgID string) error {
	_, err := b.HoldCredit(orgID, 0)
	return err
}

// HoldCredit admits a request to the deployment's keys, as AllowCredit does,
// and holds the most it may cost until ReleaseCredit. It returns what it
// held, which is what to release.
//
// Without a hold, a request is only charged when it ends, so any number of
// requests started together would all see the same credit.
func (b *Budgets) HoldCredit(orgID string, micros int64) (int64, error) {
	if !b.prepaid {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.credit[orgID]
	if c.invoiced {
		return 0, nil
	}
	held := b.held[orgID]
	if c.balance-held <= 0 {
		return 0, &policy.ErrNoCredit{BalanceMicros: c.balance, HeldMicros: held}
	}
	if micros > 0 {
		b.held[orgID] = held + micros
	}
	return max(micros, 0), nil
}

// ReleaseCredit gives back what HoldCredit held, once the request has been
// charged what it really used.
func (b *Budgets) ReleaseCredit(orgID string, micros int64) {
	if micros <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if left := b.held[orgID] - micros; left > 0 {
		b.held[orgID] = left
	} else {
		delete(b.held, orgID)
	}
}

// ChargeCredit takes what a request used from the local view of the credit,
// until the next reconcile reads what the usage writer took.
func (b *Budgets) ChargeCredit(orgID string, micros int64) {
	if !b.prepaid || micros == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.credit[orgID]
	c.balance -= micros
	b.credit[orgID] = c
}

// reconcileCredit replaces the credit with the database's.
func (b *Budgets) reconcileCredit(rows []store.CreditRow) {
	next := make(map[string]credit, len(rows))
	for _, r := range rows {
		next[r.OrgID] = credit{balance: r.BalanceMicros, invoiced: r.Invoiced}
	}
	b.mu.Lock()
	b.credit = next
	b.mu.Unlock()
}
