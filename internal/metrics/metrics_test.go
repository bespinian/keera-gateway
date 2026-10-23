package metrics

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// The exposition is hand-rolled, so only these tests stand between a scrape and
// output a parser rejects. They mostly check the shape, not the numbers: every
// family declares itself before its samples, no series is written twice, and
// histograms are cumulative, which is easy to get wrong in a plausible way.

// sample is one line of the exposition, already split into the parts a scrape
// reads it as.
type sample struct {
	name   string
	labels string
	value  string
}

// exposition is Write's output, parsed the way Prometheus parses it.
type exposition struct {
	samples []sample
	help    map[string]string
	typ     map[string]string
	// declaredBefore records, per metric name, whether a TYPE line for it was
	// seen before its first sample.
	declaredBefore map[string]bool
}

func parse(t *testing.T, raw string) exposition {
	t.Helper()
	e := exposition{
		help:           map[string]string{},
		typ:            map[string]string{},
		declaredBefore: map[string]bool{},
	}
	seen := map[string]int{}
	for i, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, text, _ := strings.Cut(rest, " ")
			if text == "" {
				t.Errorf("line %d: %q has no help text", i+1, name)
			}
			e.help[name] = text
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, kind, _ := strings.Cut(rest, " ")
			switch kind {
			case "counter", "gauge", "histogram", "summary", "untyped":
			default:
				t.Errorf("line %d: %q is not a metric type", i+1, kind)
			}
			e.typ[name] = kind
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		head, value, ok := splitSample(line)
		if !ok {
			t.Fatalf("line %d: %q is not 'series value'", i+1, line)
		}
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			t.Errorf("line %d: value %q does not parse: %v", i+1, value, err)
		}
		name, labels := head, ""
		if open := strings.IndexByte(head, '{'); open >= 0 {
			if !strings.HasSuffix(head, "}") {
				t.Fatalf("line %d: label set is not closed: %q", i+1, head)
			}
			name, labels = head[:open], head[open+1:len(head)-1]
		}
		// A family declares itself once, ahead of its samples. A scrape that
		// meets a sample first has to guess the type.
		if _, declared := e.declaredBefore[name]; !declared {
			e.declaredBefore[name] = e.typ[family(name)] != ""
		}
		if n := seen[head]; n > 0 {
			t.Errorf("line %d: %q is written twice; a scrape takes the last one", i+1, head)
		}
		seen[head]++
		e.samples = append(e.samples, sample{name: name, labels: labels, value: value})
	}
	return e
}

// splitSample separates a sample line into its series and its value.
//
// It cannot cut at the first space: a quoted label value may hold spaces and
// braces, so it finds the brace that closes the label set.
func splitSample(line string) (head, value string, ok bool) {
	end := -1
	if open := strings.IndexByte(line, '{'); open >= 0 {
		quoted := false
		for i := open; i < len(line); i++ {
			switch {
			case line[i] == '\\' && quoted:
				i++ // an escaped quote is part of the value, not the end of it
			case line[i] == '"':
				quoted = !quoted
			case line[i] == '}' && !quoted:
				end = i
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return line, "", false
		}
	}
	head, value, ok = strings.Cut(line[end+1:], " ")
	return line[:end+1] + head, value, ok
}

// family strips the suffix a histogram's own series carry, so that
// keera_x_bucket is found under the TYPE line for keera_x.
func family(name string) string {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			return base
		}
	}
	return name
}

// find returns the value of one series, and whether it was there at all.
func (e exposition) find(name, labels string) (float64, bool) {
	for _, s := range e.samples {
		if s.name == name && s.labels == labels {
			v, _ := strconv.ParseFloat(s.value, 64)
			return v, true
		}
	}
	return 0, false
}

// render runs Write and parses what came out.
func render(t *testing.T, r *Registry) exposition {
	t.Helper()
	var buf bytes.Buffer
	r.Write(&buf)
	return parse(t, buf.String())
}

func TestAnEmptyRegistryIsStillAValidExposition(t *testing.T) {
	// A gateway that has served nothing is still scraped, and empty families
	// must declare themselves, or a dashboard built before the first request
	// has nothing to draw.
	e := render(t, New())

	for _, want := range []string{
		"keera_requests_total",
		"keera_tokens_total",
		"keera_request_duration_seconds",
		"keera_upstream_errors_total",
		"keera_ratelimit_fallback_total",
		"keera_gateway_overhead_seconds",
		"keera_filter_runs_total",
		"keera_filter_cost_micros_total",
		"keera_filter_duration_seconds",
		"keera_router_decisions_total",
		"keera_router_cost_micros_total",
		"keera_router_duration_seconds",
		"keera_inflight_requests",
	} {
		if e.typ[want] == "" {
			t.Errorf("%s has no TYPE line", want)
		}
		if e.help[want] == "" {
			t.Errorf("%s has no HELP line", want)
		}
	}
}

func TestEveryFamilyDeclaresItselfBeforeItsSamples(t *testing.T) {
	r := New()
	r.Observe("gpt-oss", "org_1", 200, 0.3, 120)
	r.Overhead("gpt-oss", "org_1", 0.002)
	r.FilterRun("pii", "org_1", "rewrote", false, 0.4, 1200)
	r.RouterRun("cheap-first", "org_1", "decided", "gpt-oss", 0.1, 90)
	r.UpstreamError("gpt-oss", "org_1")
	r.RateLimitFallback()
	r.InflightAdd(2)

	e := render(t, r)
	for name, ok := range e.declaredBefore {
		if !ok {
			t.Errorf("%s has samples before anything said what it is", name)
		}
	}
}

// The histograms are accumulated by hand. Buckets that are not cumulative still
// scrape, and quietly report the wrong quantile.
func TestHistogramBucketsAreCumulativeAndEndAtTheCount(t *testing.T) {
	r := New()
	// Spread over the range, with one above the top bound so the +Inf bucket is
	// not empty: a sum that forgot it would still look plausible.
	for _, seconds := range []float64{0.01, 0.2, 0.2, 3, 45, 600} {
		r.Observe("gpt-oss", "org_1", 200, seconds, 10)
	}
	for _, seconds := range []float64{0.00005, 0.003, 0.003, 2} {
		r.Overhead("gpt-oss", "org_1", seconds)
	}
	for _, seconds := range []float64{0.1, 7, 400} {
		r.FilterRun("pii", "org_1", "rewrote", true, seconds, 5)
	}
	for _, seconds := range []float64{0.02, 90} {
		r.RouterRun("cheap-first", "org_1", "decided", "gpt-oss", seconds, 5)
	}
	e := render(t, r)

	histograms := []struct {
		name    string
		labels  string
		buckets []float64
		want    int
	}{
		{
			name:    "keera_request_duration_seconds",
			labels:  `model="gpt-oss",org="org_1",status="200"`,
			buckets: latencyBuckets, want: 6,
		},
		{
			name:    "keera_gateway_overhead_seconds",
			labels:  `model="gpt-oss",org="org_1"`,
			buckets: overheadBuckets, want: 4,
		},
		{
			name:    "keera_filter_duration_seconds",
			labels:  `filter="pii",org="org_1",outcome="rewrote",shadow="true"`,
			buckets: latencyBuckets, want: 3,
		},
		{
			name:    "keera_router_duration_seconds",
			labels:  `router="cheap-first",org="org_1",outcome="decided",destination="gpt-oss"`,
			buckets: latencyBuckets, want: 2,
		},
	}
	for _, h := range histograms {
		t.Run(h.name, func(t *testing.T) {
			prev := 0.0
			for _, ub := range h.buckets {
				le := strconv.FormatFloat(ub, 'g', -1, 64)
				got, ok := e.find(h.name+"_bucket", h.labels+`,le="`+le+`"`)
				if !ok {
					t.Fatalf("no bucket le=%q", le)
				}
				if got < prev {
					t.Errorf("bucket le=%q = %v, below the one under it (%v): "+
						"buckets have to be cumulative", le, got, prev)
				}
				prev = got
			}
			inf, ok := e.find(h.name+"_bucket", h.labels+`,le="+Inf"`)
			if !ok {
				t.Fatal("no +Inf bucket")
			}
			if inf < prev {
				t.Errorf("+Inf = %v, below the last bound (%v)", inf, prev)
			}
			count, ok := e.find(h.name+"_count", h.labels)
			if !ok {
				t.Fatal("no _count")
			}
			// Everything observed is under +Inf, so the two must be equal.
			if inf != count {
				t.Errorf("+Inf = %v but _count = %v; they are the same population", inf, count)
			}
			if count != float64(h.want) {
				t.Errorf("_count = %v, want %d observations", count, h.want)
			}
		})
	}
}

func TestTheHistogramSumIsStatedInTheUnitItsNamePromises(t *testing.T) {
	// Kept in microseconds so the counter stays an integer, but written in
	// seconds, as the name says.
	r := New()
	r.Overhead("gpt-oss", "org_1", 0.002)
	r.Overhead("gpt-oss", "org_1", 0.004)

	e := render(t, r)
	got, ok := e.find("keera_gateway_overhead_seconds_sum", `model="gpt-oss",org="org_1"`)
	if !ok {
		t.Fatal("no _sum for the overhead histogram")
	}
	if got != 0.006 {
		t.Errorf("_sum = %v, want 0.006 seconds and not 6000 microseconds", got)
	}

	// Every histogram has one, so an average can be taken of any of them.
	r.Observe("gpt-oss", "org_1", 200, 1.5, 100)
	got, ok = render(t, r).find("keera_request_duration_seconds_sum",
		`model="gpt-oss",org="org_1",status="200"`)
	if !ok || got != 1.5 {
		t.Errorf("request _sum = %v (%v), want 1.5 seconds, not the 100 tokens", got, ok)
	}
}

func TestEachCounterLandsUnderItsOwnLabels(t *testing.T) {
	r := New()
	r.Observe("gpt-oss", "org_1", 200, 0.3, 120)
	r.Observe("gpt-oss", "org_1", 200, 0.3, 80)
	// A different status is a different series, not the same one: the split
	// between them is how a deployment sees refusals.
	r.Observe("gpt-oss", "org_1", 429, 0.01, 0)
	r.Observe("gpt-oss", "org_2", 200, 0.3, 5)
	r.UpstreamError("gpt-oss", "org_1")
	r.UpstreamError("gpt-oss", "org_1")
	r.RateLimitFallback()
	r.InflightAdd(3)
	r.InflightAdd(-1)

	e := render(t, r)
	tests := []struct {
		name, labels string
		want         float64
	}{
		{"keera_requests_total", `model="gpt-oss",org="org_1",status="200"`, 2},
		{"keera_requests_total", `model="gpt-oss",org="org_1",status="429"`, 1},
		{"keera_requests_total", `model="gpt-oss",org="org_2",status="200"`, 1},
		{"keera_tokens_total", `model="gpt-oss",org="org_1",status="200"`, 200},
		{"keera_tokens_total", `model="gpt-oss",org="org_2",status="200"`, 5},
		{"keera_upstream_errors_total", `model="gpt-oss",org="org_1"`, 2},
		{"keera_ratelimit_fallback_total", "", 1},
		// A gauge, so it goes down again. The number that matters is what is
		// open now, not how many were ever opened.
		{"keera_inflight_requests", "", 2},
	}
	for _, tc := range tests {
		got, ok := e.find(tc.name, tc.labels)
		if !ok {
			t.Errorf("%s{%s} is missing", tc.name, tc.labels)
			continue
		}
		if got != tc.want {
			t.Errorf("%s{%s} = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

func TestAFallbackIsCountedWhereTheRateLimiterReportsIt(t *testing.T) {
	// The Redis limiter takes a Reporter rather than this registry, so it
	// depends on nothing. This is the only thing making a degraded ceiling
	// visible.
	var r any = New()
	reporter, ok := r.(interface{ RateLimitFallback() })
	if !ok {
		t.Fatal("the registry no longer satisfies ratelimit.Reporter")
	}
	reporter.RateLimitFallback()
	reporter.RateLimitFallback()

	e := render(t, r.(*Registry))
	if got, _ := e.find("keera_ratelimit_fallback_total", ""); got != 2 {
		t.Errorf("keera_ratelimit_fallback_total = %v, want 2", got)
	}
}

func TestALabelValueCannotBreakOutOfItsQuotes(t *testing.T) {
	// Aliases and outcomes are validated elsewhere, so a quote should never get
	// here. Pinned anyway: one label written with %s instead of %q breaks the
	// whole scrape, and nothing would point here.
	r := New()
	r.Observe(`a"b`, "org\n1", 200, 0.3, 1)
	r.FilterRun("pii", "org_1", "refused: said \"no\"", false, 0.1, 0)

	e := render(t, r)
	if _, ok := e.find("keera_requests_total", `model="a\"b",org="org\n1",status="200"`); !ok {
		t.Error("a quote and a newline in a label did not come back escaped")
	}
	if _, ok := e.find("keera_filter_runs_total",
		`filter="pii",org="org_1",outcome="refused: said \"no\"",shadow="false"`); !ok {
		t.Error("a quote in a filter outcome did not come back escaped")
	}
}

func TestALabelSetIsOnlyEverOneSeries(t *testing.T) {
	// Keys are NUL-joined and split again to be written. A value holding the
	// separator would land under the wrong series and silently under-count.
	r := New()
	r.Observe("gpt\x00oss", "org_1", 200, 0.3, 1)

	e := render(t, r)
	total := 0.0
	for _, s := range e.samples {
		if s.name == "keera_requests_total" {
			v, _ := strconv.ParseFloat(s.value, 64)
			total += v
		}
	}
	if total != 1 {
		t.Errorf("one request produced %v across the requests series, want 1", total)
	}
}
