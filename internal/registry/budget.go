package registry

import (
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

type window struct {
	start time.Time
	// base is the last figure read from the database and delta what this
	// replica charged since. Kept apart so a reconcile can simply replace base.
	base  int64
	delta int64
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
}

func newBudgets() *Budgets { return &Budgets{w: make(map[string]*window)} }

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
		w.delta += micros
	}
}

// reconcile replaces every window with the database's figure and forgets the
// local delta, which the recorder has written by then. A charge still in the
// recorder's buffer is briefly uncounted until the next reconcile.
func (b *Budgets) reconcile(rows []store.SpendRow) {
	next := make(map[string]*window, len(rows))
	for _, r := range rows {
		next[windowKey(r.ScopeType, r.ScopeID, r.Period)] = &window{
			start: r.PeriodStart.UTC(),
			base:  r.Micros,
		}
	}
	b.mu.Lock()
	b.w = next
	b.mu.Unlock()
}

// Spent returns the current view of one window, for the control API.
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
	return w.base + w.delta
}
