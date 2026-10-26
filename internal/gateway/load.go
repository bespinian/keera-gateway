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

// loads is what this process has seen of each destination, for route steps
// that order by measurement. A written order wastes equal destinations: always
// using the first of two identical vLLM deployments leaves half the GPUs idle.
// Two measured orders exist:
//
//   - In flight: requests outstanding, until the last token. Best when
//     destinations are alike, and the only signal right after a restart.
//   - Latency: a decayed average time to first token. Best when destinations
//     differ, since queue depth cannot be compared across machines.
//
// Everything is per process and in memory, since sharing it would add a round
// trip per request. These modes get less exact as the gateway scales out.
type loads struct {
	// m maps a model key, not an alias, to *destLoad, since two organisations
	// can use the same alias.
	m sync.Map
}

const (
	// loadHalfLife is short enough that a recovered destination is back within
	// a few requests, long enough that one slow request does not decide.
	loadHalfLife = 30 * time.Second
	// loadStale is when an observation is dropped, which puts the destination
	// back at the front so the step notices if it got faster.
	loadStale = 5 * time.Minute
	// loadPenalty is how long a failed destination is ranked last. Without it,
	// a dead destination stays unmeasured and idle, so both modes favour it.
	loadPenalty = 30 * time.Second
)

type destLoad struct {
	inflight atomic.Int64

	mu sync.Mutex
	// ewma is the decayed average time to first token, last updated at at.
	ewma     time.Duration
	at       time.Time
	failedAt time.Time
}

func newLoads() *loads { return &loads{} }

// get returns the record for a model key, made on first use.
func (l *loads) get(key string) *destLoad {
	if v, ok := l.m.Load(key); ok {
		return v.(*destLoad)
	}
	v, _ := l.m.LoadOrStore(key, &destLoad{})
	return v.(*destLoad)
}

// begin counts a request as in flight and returns the function that ends it.
// The count covers the whole answer, since a streaming model is still busy.
// The function must be called, or the destination looks busy forever; later
// calls do nothing.
func (l *loads) begin(key string) func() {
	d := l.get(key)
	d.inflight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { d.inflight.Add(-1) }) }
}

// observe records the time to first token, or to the whole answer without
// streaming. Only answers count, since a fast refusal would post the best
// score.
func (l *loads) observe(key string, ttft time.Duration, now time.Time) {
	if ttft <= 0 {
		return
	}
	d := l.get(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch age := now.Sub(d.at); {
	case d.at.IsZero(), age >= loadStale, age < 0:
		// Nothing to blend with, or the clock went backwards.
		d.ewma = ttft
	default:
		// Weighted by age, not count.
		w := math.Exp2(-age.Seconds() / loadHalfLife.Seconds())
		d.ewma = time.Duration(w*float64(d.ewma) + (1-w)*float64(ttft))
	}
	d.at = now
}

// fail ranks a destination last until loadPenalty has passed.
func (l *loads) fail(key string, now time.Time) {
	d := l.get(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failedAt = now
}

type reading struct {
	inflight int64
	// latency is the decayed time to first token, if known. Unknown means
	// nothing recent was seen, such as after a restart.
	latency time.Duration
	known   bool
	// failing means it failed within the last loadPenalty.
	failing bool
}

func (l *loads) read(key string, now time.Time) reading {
	d := l.get(key)
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

// order sorts a measured step's destinations in place, best first. The sort is
// stable, so the written order breaks ties. The rules, in order:
//
//  1. A destination that just failed goes last.
//  2. For latency, one with no recent measurement goes first. Otherwise the
//     first to post a good number would keep all the traffic.
//  3. Then least in flight, or lowest latency.
//
// key maps an alias to its model key.
func (l *loads) order(mode policy.RouteMode, aliases []string, key func(alias string) string) {
	if len(aliases) < 2 || !mode.Measures() {
		return
	}
	now := time.Now()
	r := make(map[string]reading, len(aliases))
	for _, a := range aliases {
		r[a] = l.read(key(a), now)
	}
	sort.SliceStable(aliases, func(i, j int) bool {
		a, b := r[aliases[i]], r[aliases[j]]
		if a.failing != b.failing {
			return !a.failing
		}
		if mode == policy.RouteModeLeastBusy {
			return a.inflight < b.inflight
		}
		if a.known != b.known {
			return !a.known
		}
		return a.latency < b.latency
	})
}

// orderByLoad sorts a route step's destinations. See loads.order.
func (s *Server) orderByLoad(rt policy.Route, aliases []string) {
	s.load.order(rt.Mode, aliases, func(alias string) string {
		return policy.ModelKey(rt.OrgID, alias)
	})
}

// describe says what a measured step currently makes of a destination, for
// the step's check. It is prose, since it is one replica's recent view.
func (l *loads) describe(mode policy.RouteMode, key string) string {
	if !mode.Measures() {
		return ""
	}
	r := l.read(key, time.Now())
	var out string
	switch {
	case mode == policy.RouteModeLeastBusy:
		out = strconv.FormatInt(r.inflight, 10) + " in flight"
	case r.known:
		out = r.latency.Round(time.Millisecond).String() + " to first token, lately"
	default:
		out = "nothing measured recently"
	}
	if r.failing {
		out += "; failed within the last " + loadPenalty.String() + ", so it is ranked last"
	}
	return out
}
