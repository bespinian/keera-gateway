package registry

import (
	"slices"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// window is one budget period's spend, or count of requests or tokens: the
// database's last figure plus what this replica charged since.
type window struct {
	start time.Time
	n     int64
}

// Budgets is the gateway's in-memory view of spend, reconciled with the
// database every few seconds, so the check before each request needs no round
// trip. Across replicas a budget can be overshot by about one refresh
// interval of traffic.
type Budgets struct {
	mu sync.RWMutex
	w  map[string]*window

	// prepaid says organisations pay for the deployment's provider keys in
	// advance, and credit is each one's balance.
	prepaid bool
	credit  map[string]credit
	// held is what running requests on this replica may take from each
	// organisation's credit. A reconcile leaves it alone, as they are not
	// charged yet.
	held map[string]int64
	// running is the requests running on this replica, per window of a scope
	// that counts requests. A reconcile leaves these alone too, as they have
	// no row yet.
	running map[string]*window
}

type credit struct {
	balance  int64
	invoiced bool
}

func newBudgets(prepaid bool) *Budgets {
	return &Budgets{
		w: make(map[string]*window), prepaid: prepaid, credit: map[string]credit{},
		held: map[string]int64{}, running: map[string]*window{},
	}
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

// Charge adds the cost of a completed request to the local view until the
// next reconcile. A scope that counts requests or tokens is left alone; see
// Tally.
func (b *Budgets) Charge(scopes []policy.Scope, micros int64, now time.Time) {
	b.Tally(scopes, policy.MeterCost, micros, now)
}

// Tally adds n to the windows of the scopes that count meter, as Charge does
// for money. Requests are counted with StartRequest instead.
func (b *Budgets) Tally(scopes []policy.Scope, meter policy.Meter, n int64, now time.Time) {
	if n != 0 {
		b.addEach(&b.w, scopes, meter, n, now)
	}
}

// StartRequest counts an admitted request against the scopes that count
// requests, until EndRequest, so requests arriving together cannot all slip
// under the limit.
func (b *Budgets) StartRequest(scopes []policy.Scope, now time.Time) {
	b.addEach(&b.running, scopes, policy.MeterRequests, 1, now)
}

// addEach adds n to the windows of the scopes that count meter. windows is
// read under the lock, as a reconcile replaces b.w.
func (b *Budgets) addEach(windows *map[string]*window, scopes []policy.Scope,
	meter policy.Meter, n int64, now time.Time,
) {
	if !counts(scopes, meter) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sc := range scopes {
		if sc.Meter == meter {
			add(*windows, windowKey(sc.Type, sc.ID, sc.Period), sc.Period.Start(now), n)
		}
	}
}

// EndRequest moves a request StartRequest counted into its windows, where it
// stays until a reconcile reads the row the recorder wrote. now is the time
// StartRequest was given, so the request ends in the window it started in.
func (b *Budgets) EndRequest(scopes []policy.Scope, now time.Time) {
	if !counts(scopes, policy.MeterRequests) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sc := range scopes {
		if sc.Meter != policy.MeterRequests {
			continue
		}
		k, start := windowKey(sc.Type, sc.ID, sc.Period), sc.Period.Start(now)
		if r, ok := b.running[k]; ok && r.start.Equal(start) {
			if r.n--; r.n <= 0 {
				delete(b.running, k)
			}
		}
		add(b.w, k, start, 1)
	}
}

// counts reports whether any scope counts meter, so the shared lock is taken
// only when there is something to count.
func counts(scopes []policy.Scope, meter policy.Meter) bool {
	return slices.ContainsFunc(scopes, func(sc policy.Scope) bool { return sc.Meter == meter })
}

// add adds n to the window at k that starts at start. An older window is
// replaced; n for a window already over, such as a stream's that ends after
// midnight, is dropped. The caller holds the lock.
func add(windows map[string]*window, k string, start time.Time, n int64) {
	w, ok := windows[k]
	switch {
	case ok && w.start.After(start):
		return
	case !ok || w.start.Before(start):
		w = &window{start: start}
		windows[k] = w
	}
	w.n += n
}

// reconcile replaces every window with the database's figure and forgets the
// local charges. A charge still in the recorder's buffer is briefly
// uncounted. Running requests stay counted.
func (b *Budgets) reconcile(rows []store.SpendRow) {
	next := make(map[string]*window, len(rows))
	for _, r := range rows {
		next[windowKey(r.ScopeType, r.ScopeID, r.Period)] = &window{
			start: r.PeriodStart.UTC(),
			n:     r.Micros,
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

// spent is zero when nothing was spent in the open window. It includes
// running requests. The caller holds the lock.
func (b *Budgets) spent(t policy.ScopeType, id string, p policy.Period, now time.Time) int64 {
	k, start := windowKey(t, id, p), p.Start(now)
	var n int64
	for _, windows := range []map[string]*window{b.w, b.running} {
		if w, ok := windows[k]; ok && w.start.Equal(start) {
			n += w.n
		}
	}
	return n
}

// HoldCredit admits a request to the deployment's keys, and holds the most it
// may cost until ReleaseCredit. It returns what to release. An organisation is
// admitted when it has unheld credit left or is billed by invoice. Without
// payments, every organisation is.
//
// Without a hold, requests started together would all see the same credit.
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

func (b *Budgets) reconcileCredit(rows []store.CreditRow) {
	next := make(map[string]credit, len(rows))
	for _, r := range rows {
		next[r.OrgID] = credit{balance: r.BalanceMicros, invoiced: r.Invoiced}
	}
	b.mu.Lock()
	b.credit = next
	b.mu.Unlock()
}
