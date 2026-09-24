package gateway

import (
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// loads is what this process has seen of each destination, for the routers
// that order by measurement instead of by a written list.
//
// A fallback router's order says the first destination is preferred. That is
// wrong when destinations are equals: always using the first of two identical
// vLLM deployments would leave half the GPUs idle. So two measured orders
// exist:
//
//   - In flight: requests outstanding against a destination, from dispatch to
//     the last token. Best when destinations are alike and requests are not,
//     and the only signal right after a restart.
//   - Latency: a decayed average of time to first token. Best when
//     destinations differ (regions, providers, hardware), since queue depth
//     cannot be compared across machines of different speed.
//
// Everything is per process and in memory. Sharing it between replicas would
// add a round trip to every request for a number that is already stale, so
// these modes get less exact as the gateway scales out. A restart starts with
// nothing measured, the same as a newly added destination.
type loads struct {
	// alias -> *destLoad. A sync.Map because it is read and written on every
	// request, over keys that change only with the catalogue.
	m sync.Map
}

const (
	// loadHalfLife is how long an observation takes to lose half its weight.
	// Short enough that a recovered destination is back within a few
	// requests, long enough that one slow request does not decide.
	loadHalfLife = 30 * time.Second
	// loadStale is when an observation is discarded and the destination is
	// unknown again, which puts it back at the front of the order. That is how
	// a measured router notices a destination has got faster.
	loadStale = 5 * time.Minute
	// loadPenalty is how long a destination that failed is ranked last.
	//
	// Without it both modes favour a dead destination: nothing answers, so it
	// stays unmeasured and first, and nothing is in flight against it. It is
	// short because being wrong costs just one failed attempt.
	loadPenalty = 30 * time.Second
)

// destLoad is one destination's measurements.
type destLoad struct {
	// inflight is the count of requests outstanding against this destination.
	// Atomic because it changes twice on every request.
	inflight atomic.Int64

	mu sync.Mutex
	// ewma is the decayed average time to first token, and at is when it was
	// last updated. at is what the decay is measured from, and what makes an
	// old number count as absent.
	ewma time.Duration
	at   time.Time
	// failedAt is when this destination last failed to answer. See
	// loadPenalty.
	failedAt time.Time
}

// newLoads builds the tracker.
func newLoads() *loads { return &loads{} }

// get is the per-destination record, made on first use.
func (l *loads) get(alias string) *destLoad {
	if v, ok := l.m.Load(alias); ok {
		return v.(*destLoad)
	}
	v, _ := l.m.LoadOrStore(alias, &destLoad{})
	return v.(*destLoad)
}

// begin records that a request has been dispatched to alias, and returns the
// function that records that it is over.
//
// The count covers the whole answer, because a model streaming is still busy.
// The gateway's inflight metric is narrower: this process's concurrency.
//
// The returned function must be called exactly once (see the defer in
// answer); a leaked count leaves a destination looking busy forever.
func (l *loads) begin(alias string) func() {
	d := l.get(alias)
	d.inflight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { d.inflight.Add(-1) }) }
}

// observe records how long a destination took to start answering.
//
// The input is the request's whole time to first token, including gateway
// overhead and guardrails. That is the same for every destination of a
// router, so it does not change their order.
//
// Only answers count: a fast refusal would post the best score. Failures are
// recorded by fail, and a 4xx, being the request's fault, not at all.
func (l *loads) observe(alias string, ttft time.Duration, now time.Time) {
	if ttft <= 0 {
		return
	}
	d := l.get(alias)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch age := now.Sub(d.at); {
	case d.at.IsZero(), age >= loadStale, age < 0:
		// Nothing to blend with: the first observation, one whose predecessor
		// has expired, or a clock that has gone backwards.
		d.ewma = ttft
	default:
		// Weighted by age, not count, so ten requests in a second and ten in
		// an hour do not count the same.
		w := math.Exp2(-age.Seconds() / loadHalfLife.Seconds())
		d.ewma = time.Duration(w*float64(d.ewma) + (1-w)*float64(ttft))
	}
	d.at = now
}

// fail records that a destination did not answer, which ranks it last until
// loadPenalty has passed.
func (l *loads) fail(alias string, now time.Time) {
	d := l.get(alias)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failedAt = now
}

// reading is what one destination looks like at one moment.
type reading struct {
	// inflight is what this gateway has outstanding against it.
	inflight int64
	// latency is the decayed time to first token, and known says whether
	// there is one. Unknown is neither slow nor fast: nothing recent has been
	// seen, as after a restart, when newly added, or after five idle minutes.
	latency time.Duration
	known   bool
	// failing is a destination that failed within the last loadPenalty.
	failing bool
}

// read takes one destination's numbers.
func (l *loads) read(alias string, now time.Time) reading {
	d := l.get(alias)
	r := reading{inflight: d.inflight.Load()}
	d.mu.Lock()
	defer d.mu.Unlock()
	if age := now.Sub(d.at); !d.at.IsZero() && age >= 0 && age < loadStale {
		r.latency, r.known = d.ewma, true
	}
	if !d.failedAt.IsZero() && now.Sub(d.failedAt) < loadPenalty {
		r.failing = true
	}
	return r
}

// order sorts a measured router's destinations into the order to try them,
// best first, in place.
//
// The sort is stable, so the written order breaks every tie: a router with no
// measurements behaves like a fallback router.
//
// The rules, in order:
//
//  1. A destination that just failed goes last (see loadPenalty).
//  2. For latency, a destination with no recent measurement goes first. This
//     deliberate inefficiency keeps the order self-correcting: otherwise the
//     first destination to post a good number would keep all the traffic.
//  3. Then least in flight, or lowest latency.
func (l *loads) order(mode policy.RouterMode, aliases []string) {
	if len(aliases) < 2 || !mode.Measures() {
		return
	}
	now := time.Now()
	r := make(map[string]reading, len(aliases))
	for _, a := range aliases {
		r[a] = l.read(a, now)
	}
	sort.SliceStable(aliases, func(i, j int) bool {
		a, b := r[aliases[i]], r[aliases[j]]
		if a.failing != b.failing {
			return !a.failing
		}
		if mode == policy.RouterModeLeastBusy {
			return a.inflight < b.inflight
		}
		if a.known != b.known {
			return !a.known
		}
		return a.latency < b.latency
	})
}

// describe says in a few words what a measured router currently makes of a
// destination, for the router's check. It is prose because it is one
// replica's view of the last few minutes.
func (l *loads) describe(mode policy.RouterMode, alias string) string {
	if !mode.Measures() {
		return ""
	}
	r := l.read(alias, time.Now())
	var out string
	if mode == policy.RouterModeLeastBusy {
		out = strconv.FormatInt(r.inflight, 10) + " in flight"
	} else if r.known {
		out = r.latency.Round(time.Millisecond).String() + " to first token, lately"
	} else {
		out = "nothing measured recently"
	}
	if r.failing {
		out += "; failed within the last " + loadPenalty.String() + ", so it is ranked last"
	}
	return out
}
