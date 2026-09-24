package store

import (
	"context"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The filter log: what each filter did to the traffic it guarded.
//
// A filter can go wrong quietly in two ways: it rewrites too much, or it
// refuses too much. Neither shows in the request log, so every run is recorded
// here: how often it fires, what it does, how long it adds, what it costs, and
// which team it affects.

// FilterOutcome is what one filter run did.
//
// There are four, not two, because a rewrite filter that changes nothing has
// worked but done nothing, and that is worth telling apart.
type FilterOutcome string

const (
	// FilterPass is a request let through unchanged: a gate's allow, or a
	// rewrite that changed no segment.
	FilterPass FilterOutcome = "pass"
	// FilterRewrite is a rewrite filter that changed at least one segment.
	FilterRewrite FilterOutcome = "rewrite"
	// FilterRefuse is the filter saying no. When enforcing, the request was
	// dropped; in shadow, it is a refusal that did not happen.
	FilterRefuse FilterOutcome = "refuse"
	// FilterError is a filter that could not run: its model is gone, disabled
	// or unreachable, its answer could not be read, or the conversation was too
	// large. When enforcing, the request was refused; in shadow, it went on.
	FilterError FilterOutcome = "error"
)

// FilterRun is one filter's pass over one request.
//
// It records what the filter did and nothing about what it read. Segments and
// Changed are counts, so rewrite rates can be measured without keeping any
// prompt text.
type FilterRun struct {
	Filter  string            `json:"filter"`
	Mode    policy.FilterMode `json:"mode"`
	Shadow  bool              `json:"shadow,omitempty"`
	Outcome FilterOutcome     `json:"outcome"`
	// LatencyMS is the filter's own generation, which the request waited for
	// before it was forwarded.
	LatencyMS  int64 `json:"latency_ms"`
	CostMicros int64 `json:"cost_micros"`
	Segments   int   `json:"segments"`
	Changed    int   `json:"changed"`
}

// FilterStat is one filter's traffic over a window: its row on the Filters list
// and the totals on its own screen.
type FilterStat struct {
	Filter string `json:"filter"`
	Runs   int64  `json:"runs"`
	// The outcomes sum to Runs. They are counts, not rates, so the reader can
	// see how small the sample is.
	Pass    int64 `json:"pass"`
	Rewrote int64 `json:"rewrote"`
	Refused int64 `json:"refused"`
	Errors  int64 `json:"errors"`
	// Shadowed is how many of these runs enforced nothing. A count, not a flag,
	// because a filter can leave shadow mode inside the window.
	Shadowed int64 `json:"shadowed"`
	// LatencyMedianMS and LatencyP95MS are what this filter adds to a request.
	LatencyMedianMS int64 `json:"latency_median_ms"`
	LatencyP95MS    int64 `json:"latency_p95_ms"`
	CostMicros      int64 `json:"cost_micros"`
	// Segments and Changed are summed over the runs, so the rewrite rate can
	// be read per segment as well as per request.
	Segments   int64      `json:"segments"`
	Changed    int64      `json:"changed"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	FirstRunAt *time.Time `json:"first_run_at,omitempty"`
}

// filterStatColumns is the select list both readings of this table share: the
// totals for one filter, and the same per filter for the list. Sharing it
// means the two can never be computed differently.
const filterStatColumns = `count(*),
	count(*) FILTER (WHERE outcome = 'pass'),
	count(*) FILTER (WHERE outcome = 'rewrite'),
	count(*) FILTER (WHERE outcome = 'refuse'),
	count(*) FILTER (WHERE outcome = 'error'),
	count(*) FILTER (WHERE shadow),
	COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms)), 0)::bigint,
	COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)), 0)::bigint,
	COALESCE(sum(cost_micros), 0), COALESCE(sum(segments), 0), COALESCE(sum(changed), 0),
	min(ts), max(ts)`

// filterStatTargets points at the fields filterStatColumns fills, in its order.
func filterStatTargets(s *FilterStat) []any {
	return []any{&s.Runs, &s.Pass, &s.Rewrote, &s.Refused, &s.Errors, &s.Shadowed,
		&s.LatencyMedianMS, &s.LatencyP95MS, &s.CostMicros, &s.Segments, &s.Changed,
		&s.FirstRunAt, &s.LastRunAt}
}

// FilterStats summarises every filter that ran inside a window, keyed by alias.
//
// A filter with no traffic is absent rather than zeroed. The caller knows which
// filters exist, and a zero row would not prove the filter never ran.
func (s *Store) FilterStats(ctx context.Context, orgID string, from, to time.Time) (
	map[string]FilterStat, error) {
	rows, err := s.pool.Query(ctx, `SELECT filter, `+filterStatColumns+`
		FROM filter_runs
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3)
		GROUP BY filter`, from, to, orgID)
	if err != nil {
		return nil, err
	}
	stats, err := collect(rows, func(r row) (FilterStat, error) {
		var st FilterStat
		err := r.Scan(append([]any{&st.Filter}, filterStatTargets(&st)...)...)
		return st, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]FilterStat, len(stats))
	for _, st := range stats {
		out[st.Filter] = st
	}
	return out, nil
}

// FilterPoint is one bucket of a filter's chart.
//
// Refusals and errors are separate series: one is the guardrail working and
// the other is the guardrail broken.
type FilterPoint struct {
	At         time.Time `json:"at"`
	Runs       int64     `json:"runs"`
	Refused    int64     `json:"refused"`
	Errors     int64     `json:"errors"`
	Rewrote    int64     `json:"rewrote"`
	CostMicros int64     `json:"cost_micros"`
}

// FilterTeamRow is one team's experience of a filter. A small refusal rate
// across the organisation can still be most of one team's traffic.
type FilterTeamRow struct {
	TeamID     string `json:"team_id"`
	Runs       int64  `json:"runs"`
	Refused    int64  `json:"refused"`
	Rewrote    int64  `json:"rewrote"`
	Errors     int64  `json:"errors"`
	CostMicros int64  `json:"cost_micros"`
}

// FilterReport is one filter's own screen.
type FilterReport struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Bucket string    `json:"bucket"`
	FilterStat
	Series []FilterPoint   `json:"series"`
	Teams  []FilterTeamRow `json:"teams"`
	// Requests and OrgCostMicros are the organisation's whole traffic and cost
	// in the same window, so the filter's numbers can be read as shares.
	Requests      int64 `json:"requests"`
	OrgCostMicros int64 `json:"org_cost_micros"`
}

// FilterReportFor reads one filter's traffic: its totals, its shape over the
// window, and which teams it affects.
func (s *Store) FilterReportFor(ctx context.Context, orgID, alias string,
	from, to time.Time) (FilterReport, error) {
	bucket := chartBucket(from, to)
	rep := FilterReport{From: from, To: to, Bucket: bucket}
	rep.Filter = alias

	err := s.pool.QueryRow(ctx, `SELECT `+filterStatColumns+`
		FROM filter_runs
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3) AND filter = $4`,
		from, to, orgID, alias).Scan(filterStatTargets(&rep.FilterStat)...)
	if err != nil {
		return rep, err
	}
	if rep.Series, err = s.filterSeries(ctx, orgID, alias, from, to, bucket); err != nil {
		return rep, err
	}
	if rep.Teams, err = s.filterTeams(ctx, orgID, alias, from, to); err != nil {
		return rep, err
	}
	err = s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(cost_micros), 0)
		FROM usage_events WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3)`,
		from, to, orgID).Scan(&rep.Requests, &rep.OrgCostMicros)
	return rep, err
}

// filterSeries is one filter's chart. Every bucket in the window is generated,
// quiet ones included, so a filter that stopped running shows as a gap.
func (s *Store) filterSeries(ctx context.Context, orgID, alias string,
	from, to time.Time, bucket string) ([]FilterPoint, error) {
	rows, err := s.pool.Query(ctx, `
		WITH buckets AS (
		    SELECT generate_series(
		        date_trunc($5, $1::timestamptz),
		        date_trunc($5, $2::timestamptz),
		        ('1 ' || $5)::interval) AS at
		)
		SELECT b.at, count(f.id),
		       count(f.id) FILTER (WHERE f.outcome = 'refuse'),
		       count(f.id) FILTER (WHERE f.outcome = 'error'),
		       count(f.id) FILTER (WHERE f.outcome = 'rewrite'),
		       COALESCE(sum(f.cost_micros), 0)
		FROM buckets b
		LEFT JOIN filter_runs f
		     ON date_trunc($5, f.ts) = b.at
		    AND f.ts >= $1 AND f.ts < $2
		    AND ($3 = '' OR f.org_id = $3) AND f.filter = $4
		GROUP BY b.at ORDER BY b.at`, from, to, orgID, alias, bucket)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (FilterPoint, error) {
		var p FilterPoint
		err := r.Scan(&p.At, &p.Runs, &p.Refused, &p.Errors, &p.Rewrote, &p.CostMicros)
		return p, err
	})
}

// filterTeams is one filter's runs split by team, most refused first.
func (s *Store) filterTeams(ctx context.Context, orgID, alias string,
	from, to time.Time) ([]FilterTeamRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(team_id, ''), count(*),
		       count(*) FILTER (WHERE outcome = 'refuse'),
		       count(*) FILTER (WHERE outcome = 'rewrite'),
		       count(*) FILTER (WHERE outcome = 'error'),
		       COALESCE(sum(cost_micros), 0)
		FROM filter_runs
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3) AND filter = $4
		GROUP BY 1
		ORDER BY 3 DESC, 2 DESC`, from, to, orgID, alias)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (FilterTeamRow, error) {
		var t FilterTeamRow
		err := r.Scan(&t.TeamID, &t.Runs, &t.Refused, &t.Rewrote, &t.Errors, &t.CostMicros)
		return t, err
	})
}
