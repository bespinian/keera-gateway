package store

import (
	"context"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The filter log records every filter run. A filter that rewrites or refuses
// too much does not show in the request log, so its runs are kept here.

// FilterOutcome is what one filter run did.
type FilterOutcome string

const (
	// FilterPass is a request let through unchanged: a gate's allow, or a
	// rewrite that changed no segment.
	FilterPass FilterOutcome = "pass"
	// FilterRewrite is a rewrite filter that changed at least one segment.
	FilterRewrite FilterOutcome = "rewrite"
	// FilterRefuse is the filter saying no. In shadow mode the request went on.
	FilterRefuse FilterOutcome = "refuse"
	// FilterError is a filter that could not run, such as when its model is
	// unreachable. When enforcing, the request was refused; in shadow, it went on.
	FilterError FilterOutcome = "error"
)

// FilterRun is one filter's pass over one request. It records what the filter
// did, never what it read: Segments and Changed are counts.
type FilterRun struct {
	Filter  string            `json:"filter"`
	Mode    policy.FilterMode `json:"mode"`
	Shadow  bool              `json:"shadow,omitempty"`
	Outcome FilterOutcome     `json:"outcome"`
	// LatencyMS is how long the request waited for the filter.
	LatencyMS  int64 `json:"latency_ms"`
	CostMicros int64 `json:"cost_micros"`
	Segments   int   `json:"segments"`
	Changed    int   `json:"changed"`
}

// FilterStat is one filter's traffic over a window.
type FilterStat struct {
	Filter string `json:"filter"`
	Runs   int64  `json:"runs"`
	// The outcomes sum to Runs. Counts, not rates, show how small the sample is.
	Passed  int64 `json:"passed"`
	Rewrote int64 `json:"rewrote"`
	Refused int64 `json:"refused"`
	Errored int64 `json:"errored"`
	// Shadowed counts the runs in shadow mode. A count, not a flag, because a
	// filter can leave shadow mode inside the window.
	Shadowed int64 `json:"shadowed"`
	// LatencyMedianMS and LatencyP95MS are what this filter adds to a request.
	LatencyMedianMS int64 `json:"latency_median_ms"`
	LatencyP95MS    int64 `json:"latency_p95_ms"`
	CostMicros      int64 `json:"cost_micros"`
	// Segments and Changed are summed over the runs, for a per-segment rewrite
	// rate.
	Segments   int64      `json:"segments"`
	Changed    int64      `json:"changed"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	FirstRunAt *time.Time `json:"first_run_at,omitempty"`
}

// filterStatColumns is the select list for one filter's totals and the list,
// so the two are always computed the same way.
var filterStatColumns = `count(*),
	` + countOf("outcome", FilterPass) + `,
	` + countOf("outcome", FilterRewrite) + `,
	` + countOf("outcome", FilterRefuse) + `,
	` + countOf("outcome", FilterError) + `,
	count(*) FILTER (WHERE shadow),
	COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms)), 0)::bigint,
	COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)), 0)::bigint,
	COALESCE(sum(cost_micros), 0), COALESCE(sum(segments), 0), COALESCE(sum(changed), 0),
	min(ts), max(ts)`

// filterStatTargets points at the fields filterStatColumns fills, in its order.
func filterStatTargets(s *FilterStat) []any {
	return []any{
		&s.Runs, &s.Passed, &s.Rewrote, &s.Refused, &s.Errored, &s.Shadowed,
		&s.LatencyMedianMS, &s.LatencyP95MS, &s.CostMicros, &s.Segments, &s.Changed,
		&s.FirstRunAt, &s.LastRunAt,
	}
}

// FilterStats summarises how each of aliases ran inside a window, keyed by
// alias. Naming the filters lets it use the index that leads with them. A
// filter with no traffic is absent rather than zeroed.
func (s *Store) FilterStats(ctx context.Context, orgID string, aliases []string,
	from, to time.Time,
) (map[string]FilterStat, error) {
	stats, err := queryAll(ctx, s.pool, scanFilterStat, `SELECT filter, `+filterStatColumns+`
		FROM filter_runs
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND filter = ANY($4)
		GROUP BY filter`, from, to, orgID, aliases)
	if err != nil {
		return nil, err
	}
	out := make(map[string]FilterStat, len(stats))
	for _, st := range stats {
		out[st.Filter] = st
	}
	return out, nil
}

func scanFilterStat(r row) (FilterStat, error) {
	var st FilterStat
	err := r.Scan(append([]any{&st.Filter}, filterStatTargets(&st)...)...)
	return st, err
}

// FilterPoint is one bucket of a filter's chart. Refusals and errors are
// apart: one is the filter working, the other is it broken.
type FilterPoint struct {
	At         time.Time `json:"at"`
	Runs       int64     `json:"runs"`
	Refused    int64     `json:"refused"`
	Errored    int64     `json:"errored"`
	Rewrote    int64     `json:"rewrote"`
	CostMicros int64     `json:"cost_micros"`
}

// FilterProjectRow is one project's runs of a filter. A small refusal rate
// across the organisation can still be most of one project's traffic.
type FilterProjectRow struct {
	ProjectID  string `json:"project_id"`
	Runs       int64  `json:"runs"`
	Refused    int64  `json:"refused"`
	Rewrote    int64  `json:"rewrote"`
	Errored    int64  `json:"errored"`
	CostMicros int64  `json:"cost_micros"`
}

// FilterReport is one filter's own screen.
type FilterReport struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Bucket string    `json:"bucket"`
	FilterStat
	Series   []FilterPoint      `json:"series"`
	Projects []FilterProjectRow `json:"projects"`
	// Requests and OrgCostMicros are the organisation's whole traffic and cost
	// in the window, so the filter's numbers can be read as shares.
	Requests      int64 `json:"requests"`
	OrgCostMicros int64 `json:"org_cost_micros"`
}

// FilterReportFor reads one filter's traffic: its totals, its shape over the
// window, and which projects it affects.
func (s *Store) FilterReportFor(ctx context.Context, orgID, alias string,
	from, to time.Time,
) (FilterReport, error) {
	bucket := chartBucket(from, to)
	rep := FilterReport{
		From: from, To: to, Bucket: bucket,
		Filter: alias,
	}

	err := s.pool.QueryRow(ctx, `SELECT `+filterStatColumns+`
		FROM filter_runs
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND filter = $4`,
		from, to, orgID, alias).Scan(filterStatTargets(&rep.FilterStat)...)
	if err != nil {
		return rep, err
	}
	if rep.Series, err = s.filterSeries(ctx, orgID, alias, from, to, bucket); err != nil {
		return rep, err
	}
	if rep.Projects, err = s.filterProjects(ctx, orgID, alias, from, to); err != nil {
		return rep, err
	}
	err = s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(cost_micros), 0)
		FROM usage_events WHERE ts >= $1 AND ts < $2 AND org_id = $3`,
		from, to, orgID).Scan(&rep.Requests, &rep.OrgCostMicros)
	return rep, err
}

// filterSeries is one filter's chart. Every bucket in the window is generated,
// so a filter that stopped running shows as a gap.
func (s *Store) filterSeries(ctx context.Context, orgID, alias string,
	from, to time.Time, bucket string,
) ([]FilterPoint, error) {
	return queryAll(ctx, s.pool, scanFilterPoint, `
		WITH buckets AS (
		    SELECT generate_series(
		        date_trunc($5, $1::timestamptz),
		        date_trunc($5, $2::timestamptz),
		        ('1 ' || $5)::interval) AS at
		)
		SELECT b.at, count(f.id),
		       `+countOf("f.outcome", FilterRefuse)+`,
		       `+countOf("f.outcome", FilterError)+`,
		       `+countOf("f.outcome", FilterRewrite)+`,
		       COALESCE(sum(f.cost_micros), 0)
		FROM buckets b
		LEFT JOIN filter_runs f
		     ON date_trunc($5, f.ts) = b.at
		    AND f.ts >= $1 AND f.ts < $2
		    AND f.org_id = $3 AND f.filter = $4
		GROUP BY b.at ORDER BY b.at`, from, to, orgID, alias, bucket)
}

func scanFilterPoint(r row) (FilterPoint, error) {
	var p FilterPoint
	err := r.Scan(&p.At, &p.Runs, &p.Refused, &p.Errored, &p.Rewrote, &p.CostMicros)
	return p, err
}

// filterProjects is one filter's runs split by project, most refused first.
func (s *Store) filterProjects(ctx context.Context, orgID, alias string,
	from, to time.Time,
) ([]FilterProjectRow, error) {
	return queryAll(ctx, s.pool, scanFilterProjectRow, `
		SELECT COALESCE(project_id, ''), count(*),
		       `+countOf("outcome", FilterRefuse)+`,
		       `+countOf("outcome", FilterRewrite)+`,
		       `+countOf("outcome", FilterError)+`,
		       COALESCE(sum(cost_micros), 0)
		FROM filter_runs
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND filter = $4
		GROUP BY 1
		ORDER BY 3 DESC, 2 DESC`, from, to, orgID, alias)
}

func scanFilterProjectRow(r row) (FilterProjectRow, error) {
	var t FilterProjectRow
	err := r.Scan(&t.ProjectID, &t.Runs, &t.Refused, &t.Rewrote, &t.Errored, &t.CostMicros)
	return t, err
}
