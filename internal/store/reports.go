package store

import (
	"context"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// ProjectSummary is a project with what the panel shows next to it: its
// guardrails, its spend against them, and how many keys are active.
type ProjectSummary struct {
	Project
	Limits     policy.Limits `json:"limits"`
	ActiveKeys int           `json:"active_keys"`
	// BudgetMicros, Period and SpendMicros are the budget nearest its end,
	// when the project has a budget per day and one per month: the one that
	// will stop its keys first.
	SpendMicros  int64         `json:"spend_micros"`
	BudgetMicros int64         `json:"budget_micros"`
	Period       policy.Period `json:"period"`
}

// ProjectSummaries lists an organisation's projects with their guardrails and
// spend, in one query to avoid N+1. An empty orgID means every organisation.
func (s *Store) ProjectSummaries(ctx context.Context, orgID string, now time.Time) ([]ProjectSummary, error) {
	return queryAll(ctx, s.pool, scanProjectSummary, `
		SELECT pr.id, pr.org_id, pr.name, pr.description, pr.created_at, `+limitColumns+`,
		       (SELECT count(*) FROM api_keys k
		         WHERE k.project_id = pr.id AND k.revoked_at IS NULL AND k.key_hash IS NOT NULL
		           AND (k.expires_at IS NULL OR k.expires_at > now())),
		       COALESCE(day.micros, 0), COALESCE(month.micros, 0)
		FROM projects pr
		LEFT JOIN guardrails p ON p.scope_type = 'project' AND p.scope_id = pr.id
		LEFT JOIN spend day ON day.scope_type = 'project' AND day.scope_id = pr.id
		     AND day.period = 'day' AND day.period_start = $2::date
		LEFT JOIN spend month ON month.scope_type = 'project' AND month.scope_id = pr.id
		     AND month.period = 'month' AND month.period_start = $3::date
		WHERE ($1 = '' OR pr.org_id = $1)
		ORDER BY pr.name`,
		orgID, policy.PeriodDay.Start(now), policy.PeriodMonth.Start(now))
}

func scanProjectSummary(r row) (ProjectSummary, error) {
	var (
		p                    ProjectSummary
		keys                 int64
		daySpent, monthSpent int64
	)
	dest := append([]any{&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CreatedAt},
		limitTargets(&p.Limits)...)
	if err := r.Scan(append(dest, &keys, &daySpent, &monthSpent)...); err != nil {
		return ProjectSummary{}, err
	}
	p.ActiveKeys = int(keys)
	// Without a budget, the month's spend is still shown.
	p.Period, p.SpendMicros = policy.PeriodMonth, monthSpent
	used := -1.0
	for _, b := range p.Limits.Budgets {
		if b.BudgetMicros <= 0 {
			continue
		}
		spent := monthSpent
		if b.Per() == policy.PeriodDay {
			spent = daySpent
		}
		if f := float64(spent) / float64(b.BudgetMicros); f > used {
			used = f
			p.BudgetMicros, p.Period, p.SpendMicros = b.BudgetMicros, b.Per(), spent
		}
	}
	return p, nil
}

// SeriesPoint is one bucket of the dashboard's chart.
type SeriesPoint struct {
	At         time.Time `json:"at"`
	Requests   int64     `json:"requests"`
	Tokens     int64     `json:"tokens"`
	CostMicros int64     `json:"cost_micros"`
	Failed     int64     `json:"failed"`
}

// Overview is everything the dashboard needs, in one round trip. A project's,
// key's or model's own screen uses it too, so the screens never disagree.
type Overview struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Bucket       string    `json:"bucket"`
	Requests     int64     `json:"requests"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CostMicros   int64     `json:"cost_micros"`
	// SubscriptionMicros is what Claude subscriptions paid for, at API prices.
	// It is not part of CostMicros.
	SubscriptionMicros int64         `json:"subscription_micros"`
	Refused            int64         `json:"refused"`
	Failed             int64         `json:"failed"`
	TTFTMedianMS       int64         `json:"ttft_median_ms"`
	TTFTP95MS          int64         `json:"ttft_p95_ms"`
	Series             []SeriesPoint `json:"series"`
	TopProjects        []UsageBucket `json:"top_projects"`
	TopModels          []UsageBucket `json:"top_models"`
	// TopKeys is only filled in for a scoped overview, where "which key is
	// doing this" is the next question.
	TopKeys []UsageBucket `json:"top_keys,omitempty"`
}

// chartBucket is hourly for a window of up to two days and daily beyond, so a
// chart never has hundreds of points.
func chartBucket(from, to time.Time) string {
	if to.Sub(from) <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

// Overview aggregates the usage log for the dashboard, or for one entity's own
// screen when sc narrows it.
//
// Over more than two days the totals come from usage_daily, and From and To
// are widened to whole days. The first-token times always come from the
// request log, which retention may have cut shorter.
func (s *Store) Overview(ctx context.Context, orgID string, from, to time.Time,
	sc ReportScope) (Overview, error) {
	src, from, to := sourceFor(from, to)
	o := Overview{From: from, To: to, Bucket: chartBucket(from, to)}

	err := s.pool.QueryRow(ctx, `
		SELECT `+src.all+`,
		       COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
		       COALESCE(sum(cost_micros), 0), COALESCE(sum(list_cost_micros), 0),
		       `+src.refused+`, `+src.failed+`
		FROM `+src.table+`
		WHERE `+src.window+` AND ($3 = '' OR org_id = $3)`+sc.narrow("", 3),
		append([]any{from, to, orgID}, sc.args()...)...,
	).Scan(&o.Requests, &o.InputTokens, &o.OutputTokens, &o.CostMicros, &o.SubscriptionMicros,
		&o.Refused, &o.Failed)
	if err != nil {
		return o, err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms)
		                      FILTER (WHERE ttft_ms > 0)), 0)::bigint,
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
		                      FILTER (WHERE ttft_ms > 0)), 0)::bigint
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3)`+sc.narrow("", 3),
		append([]any{from, to, orgID}, sc.args()...)...,
	).Scan(&o.TTFTMedianMS, &o.TTFTP95MS)
	if err != nil {
		return o, err
	}
	if o.Series, err = s.series(ctx, src, orgID, from, to, o.Bucket, sc); err != nil {
		return o, err
	}

	q := UsageQuery{OrgID: orgID, ReportScope: sc, From: from, To: to}
	if o.TopProjects, err = s.topUsage(ctx, q, "project"); err != nil {
		return o, err
	}
	if o.TopModels, err = s.topUsage(ctx, q, "model"); err != nil {
		return o, err
	}
	// Across a whole organisation the key list is long and says little. Inside
	// one project or model it names who is responsible.
	if !sc.Empty() {
		if o.TopKeys, err = s.topUsage(ctx, q, "key"); err != nil {
			return o, err
		}
	}
	return o, nil
}

// series is the dashboard's chart. Every bucket in the window is generated, not
// only those with traffic, so a quiet stretch shows as a dip, not a flat line.
func (s *Store) series(ctx context.Context, src usageSource, orgID string, from, to time.Time,
	bucket string, sc ReportScope) ([]SeriesPoint, error) {
	// A LEFT JOIN leaves one row of NULLs for an empty bucket, so the raw log
	// counts ids, not rows.
	requests, failed := "count(e.id)", "count(e.id) FILTER (WHERE e.status >= 500)"
	window := "e.ts >= $1 AND e.ts < $2"
	if src.daily {
		requests, failed = "COALESCE(sum(e.requests), 0)", "COALESCE(sum(e.failed), 0)"
		window = "e.day >= $1::date AND e.day < $2::date"
	}
	return queryAll(ctx, s.pool, scanSeriesPoint, `
		WITH buckets AS (
		    SELECT generate_series(
		        date_trunc($4, $1::timestamptz),
		        date_trunc($4, $5::timestamptz),
		        ('1 ' || $4)::interval) AS at
		)
		SELECT b.at, `+requests+`,
		       COALESCE(sum(e.input_tokens + e.output_tokens), 0),
		       COALESCE(sum(e.cost_micros), 0),
		       `+failed+`
		FROM buckets b
		LEFT JOIN `+src.table+` e
		     ON date_trunc($4, e.`+src.at+`) = b.at
		    AND `+window+`
		    AND ($3 = '' OR e.org_id = $3)`+sc.narrow("e.", 5)+`
		GROUP BY b.at ORDER BY b.at`,
		append([]any{from, to, orgID, bucket, src.lastBucket(to)}, sc.args()...)...)
}

func scanSeriesPoint(r row) (SeriesPoint, error) {
	var p SeriesPoint
	err := r.Scan(&p.At, &p.Requests, &p.Tokens, &p.CostMicros, &p.Failed)
	return p, err
}

// topUsage is the first six buckets of q grouped by groupBy.
func (s *Store) topUsage(ctx context.Context, q UsageQuery, groupBy string) ([]UsageBucket, error) {
	q.GroupBy = groupBy
	b, err := s.Usage(ctx, q)
	if err != nil {
		return nil, err
	}
	return b[:min(len(b), 6)], nil
}

// ProjectOrg returns which organisation a project belongs to, so a request
// naming a project can be checked against the caller's own tenant first.
func (s *Store) ProjectOrg(ctx context.Context, projectID string) (string, error) {
	var orgID string
	err := s.pool.QueryRow(ctx, "SELECT org_id FROM projects WHERE id = $1", projectID).Scan(&orgID)
	if err != nil {
		return "", notFound(err)
	}
	return orgID, nil
}

// KeyOwner is where a key sits: its organisation, and its project and the
// person it is for, each empty for none.
type KeyOwner struct {
	OrgID, ProjectID, UserID string
	Kind                     policy.KeyKind
}

// KeyOwnerOf reads where a key sits. Members may revoke only their own keys,
// so all of it is read in one row: a second query could see a key that was
// reassigned in between.
func (s *Store) KeyOwnerOf(ctx context.Context, keyID string) (KeyOwner, error) {
	var o KeyOwner
	err := s.pool.QueryRow(ctx,
		"SELECT org_id, COALESCE(project_id,''), COALESCE(user_id,''), kind FROM api_keys WHERE id = $1",
		keyID,
	).Scan(&o.OrgID, &o.ProjectID, &o.UserID, &o.Kind)
	return o, notFound(err)
}

// ScopeName is what one org, project or key is called, for a report that names
// the level a limit came from.
func (s *Store) ScopeName(ctx context.Context, scope policy.ScopeType, id string) (string, error) {
	var query string
	switch scope {
	case policy.ScopeOrg:
		query = "SELECT name FROM orgs WHERE id = $1"
	case policy.ScopeProject:
		query = "SELECT name FROM projects WHERE id = $1"
	case policy.ScopeKey:
		query = "SELECT name FROM api_keys WHERE id = $1"
	default:
		return "", ErrNotFound
	}
	var name string
	if err := s.pool.QueryRow(ctx, query, id).Scan(&name); err != nil {
		return "", notFound(err)
	}
	return name, nil
}

// ProjectNames maps project ids to names, for a report grouped by project.
func (s *Store) ProjectNames(ctx context.Context, orgID string) (map[string]string, error) {
	return s.names(ctx, "SELECT id, name FROM projects WHERE $1 = '' OR org_id = $1", orgID)
}

// KeyNames maps key ids to names, for a report grouped by key. Revoked keys
// are included, because their history stays.
func (s *Store) KeyNames(ctx context.Context, orgID string) (map[string]string, error) {
	return s.names(ctx, "SELECT id, name FROM api_keys WHERE $1 = '' OR org_id = $1", orgID)
}

// UserEmails maps user ids to addresses, and to names for those who have one,
// for a report grouped by user.
func (s *Store) UserEmails(ctx context.Context, orgID string) (emails, names map[string]string, err error) {
	type row struct{ email, name string }
	rows, err := queryMap(ctx, s.pool, func(r *row) []any { return []any{&r.email, &r.name} },
		"SELECT id, email, name FROM users WHERE $1 = '' OR org_id = $1", orgID)
	if err != nil {
		return nil, nil, err
	}
	emails = make(map[string]string, len(rows))
	names = map[string]string{}
	for id, r := range rows {
		emails[id] = r.email
		if r.name != "" {
			names[id] = r.name
		}
	}
	return emails, names, nil
}

// names runs an (id, name) query and returns it as a map.
func (s *Store) names(ctx context.Context, sql string, args ...any) (map[string]string, error) {
	return queryMap(ctx, s.pool, func(name *string) []any { return []any{name} }, sql, args...)
}

// KeySummary is an issued key with what the keys screen shows next to it: its
// own guardrails, when it was last used, and what it has spent.
//
// Requests counts only what was served, so it explains the cost. Last use
// counts every attempt, because any use at all matters when deciding whether
// a key can be revoked.
type KeySummary struct {
	KeyInfo
	Limits      policy.Limits `json:"limits"`
	LastUsedAt  *time.Time    `json:"last_used_at,omitempty"`
	Requests    int64         `json:"requests"`
	SpendMicros int64         `json:"spend_micros"`
	// SubscriptionMicros and Plan are for a subscription key: what its
	// holder's Claude plan paid for, at API prices, and how much of the plan
	// is used.
	SubscriptionMicros int64             `json:"subscription_micros"`
	Plan               *policy.PlanUsage `json:"plan,omitempty"`
}

// KeyQuery narrows a key listing. OrgID is required; the rest are optional.
// UserID is set by a developer's own screen, to see only their keys.
type KeyQuery struct {
	OrgID     string
	ProjectID string
	UserID    string
	Since     time.Time
}

// KeySummaries lists an organisation's keys with their guardrails and their
// traffic since q.Since, in one query to avoid N+1. Since more than two days
// ago, the traffic comes from usage_daily, from the midnight before q.Since.
func (s *Store) KeySummaries(ctx context.Context, q KeyQuery) ([]KeySummary, error) {
	traffic := `LEFT JOIN LATERAL (
		    SELECT max(e.ts) AS last_used_at,
		           count(*) FILTER (WHERE e.status < 400) AS requests,
		           COALESCE(sum(e.cost_micros), 0) AS micros,
		           COALESCE(sum(e.list_cost_micros), 0) AS list_micros
		    FROM usage_events e WHERE e.key_id = k.id AND e.ts >= $3
		) u ON true`
	since, _, daily := DayWindow(q.Since, time.Now())
	if daily {
		traffic = `LEFT JOIN (
		    SELECT key_id, max(last_at) AS last_used_at,
		           sum(requests - refused - failed) AS requests,
		           sum(cost_micros) AS micros, sum(list_cost_micros) AS list_micros
		    FROM usage_daily WHERE org_id = $1 AND day >= $3::date
		    GROUP BY key_id
		) u ON u.key_id = k.id`
	}
	return queryAll(ctx, s.pool, scanKeySummary, `
		SELECT `+keyColumnsOf("k.")+`, `+limitColumns+`,
		       u.last_used_at, COALESCE(u.requests, 0), COALESCE(u.micros, 0),
		       COALESCE(u.list_micros, 0), pu.five_hour, pu.five_hour_resets_at,
		       pu.seven_day, pu.seven_day_resets_at, pu.status, pu.updated_at
		FROM api_keys k
		LEFT JOIN guardrails p ON p.scope_type = 'key' AND p.scope_id = k.id
		LEFT JOIN plan_usage pu ON pu.key_id = k.id
		`+traffic+`
		WHERE k.org_id = $1 AND ($2 = '' OR k.project_id = $2) AND ($4 = '' OR k.user_id = $4)
		ORDER BY k.created_at DESC`, q.OrgID, q.ProjectID, since, q.UserID)
}

func scanKeySummary(r row) (KeySummary, error) {
	var (
		k        KeySummary
		plan     policy.PlanUsage
		status   *string
		reported *time.Time
	)
	dest := append(keyTargets(&k.KeyInfo), limitTargets(&k.Limits)...)
	dest = append(dest, &k.LastUsedAt, &k.Requests, &k.SpendMicros, &k.SubscriptionMicros,
		&plan.FiveHour, &plan.FiveHourResetsAt, &plan.SevenDay, &plan.SevenDayResetsAt,
		&status, &reported)
	if err := r.Scan(dest...); err != nil {
		return KeySummary{}, err
	}
	if reported != nil {
		plan.UpdatedAt = *reported
		if status != nil {
			plan.Status = *status
		}
		k.Plan = &plan
	}
	return k, nil
}

// Setup counts what a deployment has, so the panel can tell a new operator
// which step they are on.
type Setup struct {
	Orgs     int64 `json:"orgs"`
	Keys     int64 `json:"keys"`
	Models   int64 `json:"models"`
	Requests int64 `json:"requests"`
	// Filters, Routers, Sandboxes and MCP servers are optional.
	// The panel hides their screens until there is at least one.
	Filters    int64 `json:"filters"`
	Routers    int64 `json:"routers"`
	Sandboxes  int64 `json:"sandboxes"`
	MCPServers int64 `json:"mcp_servers"`
	// Credits counts the money ever put into credit, paid or granted. The
	// first run stays until there is some.
	Credits int64 `json:"credits"`
	// Invoiced says the organisation is billed by invoice, so it needs no
	// credit. Always false across every organisation.
	Invoiced bool `json:"invoiced"`
}

// SetupState counts the objects a first run has to create. An empty orgID
// counts across every organisation.
func (s *Store) SetupState(ctx context.Context, orgID string) (Setup, error) {
	var st Setup
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM orgs),
		(SELECT count(*) FROM api_keys WHERE ($1 = '' OR org_id = $1) AND revoked_at IS NULL),
		(SELECT count(*) FROM models WHERE enabled AND ($1 = '' OR org_id = $1)),
		-- 1 once anything was served, as counting it all would read the
		-- whole usage log on every panel load. The daily totals outlive the
		-- log, so the first run does not come back after retention.
		(SELECT count(*) FROM (SELECT 1 FROM usage_daily WHERE $1 = '' OR org_id = $1 LIMIT 1) AS served),
		(SELECT count(*) FROM filters WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM routers WHERE $1 = '' OR org_id = $1),
		-- Classes, not running machines, so the screen stays when none is up.
		(SELECT count(*) FROM sandbox_classes WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM mcp_servers WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM payments WHERE ($1 = '' OR org_id = $1)
			AND state = 'paid' AND micros > 0),
		EXISTS (SELECT 1 FROM credit_accounts WHERE org_id = $1 AND invoiced)`, orgID,
	).Scan(&st.Orgs, &st.Keys, &st.Models, &st.Requests,
		&st.Filters, &st.Routers, &st.Sandboxes, &st.MCPServers, &st.Credits,
		&st.Invoiced)
	return st, err
}

// AnyUsed reports whether any of keyIDs has ever made a request. It reads the
// daily totals, which outlive the request log.
func (s *Store) AnyUsed(ctx context.Context, orgID string, keyIDs []string) (bool, error) {
	if len(keyIDs) == 0 {
		return false, nil
	}
	var used bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM usage_daily
		WHERE org_id = $1 AND key_id = ANY($2))`, orgID, keyIDs).Scan(&used)
	return used, err
}

// Refusal is one request the gateway turned away. The status is the reason:
// 402 a spent budget, 429 a rate limit, 404 a model this key may not use.
type Refusal struct {
	CreatedAt time.Time `json:"created_at"`
	Alias     string    `json:"alias,omitempty"`
	Status    int       `json:"status"`
	KeyID     string    `json:"key_id,omitempty"`
	// Error is what the client was told: the guardrail's wording for a
	// refusal, the inference plane's for a failure.
	Error string `json:"error,omitempty"`
}

// Refusals lists the most recent refused or failed requests made with one of
// keyIDs, newest first. An empty keyIDs returns nothing, not everything: the
// caller is asking about their own keys.
func (s *Store) Refusals(ctx context.Context, orgID string, keyIDs []string,
	since time.Time, limit int) ([]Refusal, error) {
	if len(keyIDs) == 0 {
		return []Refusal{}, nil
	}
	return queryAll(ctx, s.pool, scanRefusal, `SELECT ts, alias, status, COALESCE(key_id, ''),
		COALESCE(error, '')
		FROM usage_events
		WHERE org_id = $1 AND key_id = ANY($2) AND ts >= $3 AND status >= 400
		ORDER BY ts DESC LIMIT $4`, orgID, keyIDs, since, pageLimit(limit, 20, 200))
}

func scanRefusal(r row) (Refusal, error) {
	var f Refusal
	err := r.Scan(&f.CreatedAt, &f.Alias, &f.Status, &f.KeyID, &f.Error)
	return f, err
}
