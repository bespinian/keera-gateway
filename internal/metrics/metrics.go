// Package metrics is a small Prometheus exposition, hand-rolled so that the
// gateway stays a single binary with no client-library dependency.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Buckets are the latency buckets, in seconds. They reach far up because a
// streamed completion can run for minutes.
var Buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// OverheadBuckets measure the gateway's own work, which is tens of
// microseconds when it goes well. With Buckets every request would land in the
// first bucket. The top is far above normal, because a slow request is what
// this is here to show.
var OverheadBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1,
}

// Label names of each keyed family, in the order their values are joined.
var (
	requestLabels  = []string{"model", "org", "status"}
	overheadLabels = []string{"model", "org"}
	filterLabels   = []string{"filter", "org", "outcome", "shadow"}
	routerLabels   = []string{"router", "org", "outcome", "destination"}
	// A tool's name is not a label: a client can send any name, and series are
	// never cleaned up.
	toolLabels = []string{"server", "org", "outcome"}
)

type series struct {
	count   atomic.Int64
	sum     atomic.Int64 // micro-units, for counters that carry a value
	buckets []float64
	hist    []atomic.Int64
}

func (s *series) observe(seconds float64, sum int64) {
	s.count.Add(1)
	s.sum.Add(sum)
	i, _ := slices.BinarySearch(s.buckets, seconds)
	s.hist[i].Add(1)
}

// Registry collects the gateway's counters.
type Registry struct {
	mu sync.RWMutex
	// The keyed maps hold label values joined by NUL, in the order of the
	// family's label names above.
	requests map[string]*series
	inflight atomic.Int64
	// rlFallback counts rate-limit decisions made locally because Redis was
	// unreachable. Anything but zero means the limits bind per replica.
	rlFallback   atomic.Int64
	upstreamErrs map[string]*atomic.Int64 // by model
	overhead     map[string]*series
	// A filter's cost and refusals hide inside the request's series, so it has
	// its own families to alert on.
	filters map[string]*series
	// The destination is a label because the split between destinations is
	// the point of a router.
	routers map[string]*series
	tools   map[string]*series
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		requests:     make(map[string]*series),
		upstreamErrs: make(map[string]*atomic.Int64),
		overhead:     make(map[string]*series),
		filters:      make(map[string]*series),
		routers:      make(map[string]*series),
		tools:        make(map[string]*series),
	}
}

// getOrCreate takes the write lock only the first time a label set appears.
func getOrCreate[T any](r *Registry, m map[string]*T, key string, create func() *T) *T {
	r.mu.RLock()
	v, ok := m[key]
	r.mu.RUnlock()
	if ok {
		return v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok = m[key]; ok {
		return v
	}
	v = create()
	m[key] = v
	return v
}

func (r *Registry) seriesIn(m map[string]*series, buckets []float64, values ...string) *series {
	return getOrCreate(r, m, strings.Join(values, "\x00"), func() *series {
		return &series{buckets: buckets, hist: make([]atomic.Int64, len(buckets)+1)}
	})
}

// Observe records one finished request.
func (r *Registry) Observe(model, org string, status int, seconds float64, tokens int) {
	r.seriesIn(r.requests, Buckets, model, org, strconv.Itoa(status)).
		observe(seconds, int64(tokens))
}

// Overhead records what the gateway itself added ahead of the inference plane.
// The request duration is almost all model time, so it would hide the
// gateway getting slower.
//
// The window runs from the body being read to the upstream call. Filter
// generation is subtracted, because it has its own metric and would otherwise
// make a new guardrail look like a slow gateway. Refusals are not recorded:
// they never made an upstream call.
func (r *Registry) Overhead(model, org string, seconds float64) {
	r.seriesIn(r.overhead, OverheadBuckets, model, org).
		observe(seconds, int64(seconds*1e6))
}

// FilterRun records one filter's pass over one request. shadow is a label so
// the weeks before and after a filter was enforced read side by side.
func (r *Registry) FilterRun(filter, org, outcome string, shadow bool,
	seconds float64, micros int64,
) {
	r.seriesIn(r.filters, Buckets, filter, org, outcome, strconv.FormatBool(shadow)).
		observe(seconds, micros)
}

// RouterRun records one routing decision. dest is empty when the request went
// nowhere.
func (r *Registry) RouterRun(router, org, outcome, dest string,
	seconds float64, micros int64,
) {
	r.seriesIn(r.routers, Buckets, router, org, outcome, dest).
		observe(seconds, micros)
}

// ToolCall records one MCP tool call through the gateway.
func (r *Registry) ToolCall(server, org, outcome string, seconds float64) {
	r.seriesIn(r.tools, Buckets, server, org, outcome).observe(seconds, 0)
}

// UpstreamError records a failure reaching the inference plane.
func (r *Registry) UpstreamError(model string) {
	getOrCreate(r, r.upstreamErrs, model, func() *atomic.Int64 { return new(atomic.Int64) }).Add(1)
}

// RateLimitFallback records a rate-limit decision that Redis could not answer.
func (r *Registry) RateLimitFallback() { r.rlFallback.Add(1) }

// InflightAdd tracks concurrent upstream requests, which shows whether the
// inference plane is saturated.
func (r *Registry) InflightAdd(n int64) { r.inflight.Add(n) }

// Write renders the exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	requests := keyed{w: w, labels: requestLabels, m: r.requests}
	requests.counter("keera_requests_total", "Inference requests handled by the gateway.",
		func(s *series) int64 { return s.count.Load() })
	requests.counter("keera_tokens_total", "Input plus output tokens.",
		func(s *series) int64 { return s.sum.Load() })
	requests.histogram("keera_request_duration_seconds", "Wall time of an inference request.", false)

	header(w, "keera_upstream_errors_total", "Failures reaching the inference plane.", "counter")
	for _, model := range slices.Sorted(maps.Keys(r.upstreamErrs)) {
		_, _ = fmt.Fprintf(w, "keera_upstream_errors_total{model=%q} %d\n", model, r.upstreamErrs[model].Load())
	}

	header(w, "keera_ratelimit_fallback_total",
		"Rate-limit decisions made locally because Redis was unreachable.", "counter")
	_, _ = fmt.Fprintf(w, "keera_ratelimit_fallback_total %d\n", r.rlFallback.Load())

	overhead := keyed{w: w, labels: overheadLabels, m: r.overhead}
	overhead.histogram("keera_gateway_overhead_seconds",
		"Time the gateway added ahead of the inference plane, excluding filter generation.", true)

	filters := keyed{w: w, labels: filterLabels, m: r.filters}
	filters.counter("keera_filter_runs_total", "Filter runs, by what the filter did.",
		func(s *series) int64 { return s.count.Load() })
	filters.counter("keera_filter_cost_micros_total",
		"What filter models spent, in micro-units of the billing currency.",
		func(s *series) int64 { return s.sum.Load() })
	filters.histogram("keera_filter_duration_seconds",
		"What a filter added to the request that waited for it.", false)

	routers := keyed{w: w, labels: routerLabels, m: r.routers}
	routers.counter("keera_router_decisions_total", "Routing decisions, by where they sent the request.",
		func(s *series) int64 { return s.count.Load() })
	routers.counter("keera_router_cost_micros_total",
		"What deciding cost, in micro-units of the billing currency.",
		func(s *series) int64 { return s.sum.Load() })
	routers.histogram("keera_router_duration_seconds",
		"What deciding added to the request that waited for it.", false)

	tools := keyed{w: w, labels: toolLabels, m: r.tools}
	tools.counter("keera_tool_calls_total", "MCP tool calls through the gateway, by outcome.",
		func(s *series) int64 { return s.count.Load() })
	tools.histogram("keera_tool_call_duration_seconds", "Wall time of an MCP tool call.", false)

	header(w, "keera_inflight_requests", "Requests currently open against the inference plane.", "gauge")
	_, _ = fmt.Fprintf(w, "keera_inflight_requests %d\n", r.inflight.Load())
}

func header(w io.Writer, name, help, typ string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// keyed is one map of series, written out under its label names.
type keyed struct {
	w      io.Writer
	labels []string
	m      map[string]*series
}

func (f keyed) counter(name, help string, value func(*series) int64) {
	header(f.w, name, help, "counter")
	for _, k := range slices.Sorted(maps.Keys(f.m)) {
		_, _ = fmt.Fprintf(f.w, "%s{%s} %d\n", name, f.labelSet(k), value(f.m[k]))
	}
}

// histogram writes cumulative buckets. Only the overhead histogram has a sum.
func (f keyed) histogram(name, help string, withSum bool) {
	header(f.w, name, help, "histogram")
	for _, k := range slices.Sorted(maps.Keys(f.m)) {
		s, labels := f.m[k], f.labelSet(k)
		var cum int64
		for i, ub := range s.buckets {
			cum += s.hist[i].Load()
			_, _ = fmt.Fprintf(f.w, "%s_bucket{%s,le=%q} %d\n",
				name, labels, strconv.FormatFloat(ub, 'g', -1, 64), cum)
		}
		cum += s.hist[len(s.buckets)].Load()
		_, _ = fmt.Fprintf(f.w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, labels, cum)
		_, _ = fmt.Fprintf(f.w, "%s_count{%s} %d\n", name, labels, cum)
		if withSum {
			// Kept in microseconds so the counter stays an integer; written in
			// seconds, as the metric's name promises.
			_, _ = fmt.Fprintf(f.w, "%s_sum{%s} %s\n",
				name, labels, strconv.FormatFloat(float64(s.sum.Load())/1e6, 'f', 6, 64))
		}
	}
}

// labelSet renders a NUL-joined key as name="value" pairs.
func (f keyed) labelSet(key string) string {
	values := strings.SplitN(key, "\x00", len(f.labels))
	pairs := make([]string, len(f.labels))
	for i, name := range f.labels {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		pairs[i] = name + "=" + strconv.Quote(v)
	}
	return strings.Join(pairs, ",")
}
