package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// TeamSummary is a team with what the panel shows next to it: its guardrails,
// its spend against them, and how many keys are live.
type TeamSummary struct {
	Team
	Limits       policy.Limits `json:"limits"`
	ActiveKeys   int           `json:"active_keys"`
	SpendMicros  int64         `json:"spend_micros"`
	BudgetMicros int64         `json:"budget_micros"`
	Period       policy.Period `json:"period"`
}

// TeamSummaries lists an organisation's teams with their guardrails and spend,
// in one query to avoid N+1. An empty orgID means every organisation.
func (s *Store) TeamSummaries(ctx context.Context, orgID string, now time.Time) ([]TeamSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.org_id, t.name, t.created_at, `+limitColumns+`,
		       (SELECT count(*) FROM api_keys k
		         WHERE k.team_id = t.id AND k.revoked_at IS NULL),
		       COALESCE(sp.micros, 0)
		FROM teams t
		LEFT JOIN guardrails p ON p.scope_type = 'team' AND p.scope_id = t.id
		LEFT JOIN spend sp ON sp.scope_type = 'team' AND sp.scope_id = t.id
		     AND sp.period = COALESCE(p.budget_period, 'month')
		     AND sp.period_start = CASE WHEN COALESCE(p.budget_period, 'month') = 'day'
		                                THEN $2::date ELSE $3::date END
		WHERE ($1 = '' OR t.org_id = $1)
		ORDER BY t.name`,
		orgID, policy.PeriodDay.Start(now), policy.PeriodMonth.Start(now))
	if err != nil {
		return nil, err
	}
	return collect(rows, scanTeamSummary)
}

func scanTeamSummary(r row) (TeamSummary, error) {
	var (
		t      TeamSummary
		period *string
		keys   int64
	)
	dest := append([]any{&t.ID, &t.OrgID, &t.Name, &t.CreatedAt},
		limitTargets(&t.Limits, &period)...)
	if err := r.Scan(append(dest, &keys, &t.SpendMicros)...); err != nil {
		return TeamSummary{}, err
	}
	t.ActiveKeys = int(keys)
	// A budget with no period resets monthly, as the spend join above assumes.
	if period == nil {
		month := string(policy.PeriodMonth)
		period = &month
	}
	t.Period = policy.Period(*period)
	t.Limits.BudgetPeriod = periodPtr(period)
	if t.Limits.BudgetMicros != nil {
		t.BudgetMicros = *t.Limits.BudgetMicros
	}
	return t, nil
}

// SeriesPoint is one bucket of the dashboard's chart.
type SeriesPoint struct {
	At         time.Time `json:"at"`
	Requests   int64     `json:"requests"`
	Tokens     int64     `json:"tokens"`
	CostMicros int64     `json:"cost_micros"`
	Errors     int64     `json:"errors"`
}

// Overview is everything the dashboard needs, in one round trip.
//
// One team's, key's or model's own screen uses it too, over fewer rows.
// Computing both the same way keeps the screens from disagreeing.
type Overview struct {
	From         time.Time     `json:"from"`
	To           time.Time     `json:"to"`
	Bucket       string        `json:"bucket"`
	Requests     int64         `json:"requests"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	CostMicros   int64         `json:"cost_micros"`
	Refused      int64         `json:"refused"`
	Failed       int64         `json:"failed"`
	TTFTMedianMS int64         `json:"ttft_median_ms"`
	TTFTP95MS    int64         `json:"ttft_p95_ms"`
	Series       []SeriesPoint `json:"series"`
	TopTeams     []UsageBucket `json:"top_teams"`
	TopModels    []UsageBucket `json:"top_models"`
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
func (s *Store) Overview(ctx context.Context, orgID string, from, to time.Time,
	sc Scope) (Overview, error) {
	o := Overview{From: from, To: to, Bucket: chartBucket(from, to)}

	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
		       COALESCE(sum(cost_micros), 0),
		       count(*) FILTER (WHERE status BETWEEN 400 AND 499),
		       count(*) FILTER (WHERE status >= 500),
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms)
		                      FILTER (WHERE ttft_ms > 0)), 0)::bigint,
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms)
		                      FILTER (WHERE ttft_ms > 0)), 0)::bigint
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3)`+sc.narrow("", 3),
		append([]any{from, to, orgID}, sc.args()...)...,
	).Scan(&o.Requests, &o.InputTokens, &o.OutputTokens, &o.CostMicros,
		&o.Refused, &o.Failed, &o.TTFTMedianMS, &o.TTFTP95MS)
	if err != nil {
		return o, err
	}
	if o.Series, err = s.series(ctx, orgID, from, to, o.Bucket, sc); err != nil {
		return o, err
	}

	q := UsageQuery{OrgID: orgID, Scope: sc, From: from, To: to}
	if o.TopTeams, err = s.topUsage(ctx, q, "team"); err != nil {
		return o, err
	}
	if o.TopModels, err = s.topUsage(ctx, q, "model"); err != nil {
		return o, err
	}
	// Across a whole organisation the key list is long and says little, so the
	// dashboard skips it. Inside one team or model it names who is responsible.
	if !sc.Empty() {
		if o.TopKeys, err = s.topUsage(ctx, q, "key"); err != nil {
			return o, err
		}
	}
	return o, nil
}

// series is the dashboard's chart. Every bucket in the window is generated, not
// only those with traffic, so a quiet stretch shows as a dip, not a flat line.
func (s *Store) series(ctx context.Context, orgID string, from, to time.Time,
	bucket string, sc Scope) ([]SeriesPoint, error) {
	rows, err := s.pool.Query(ctx, `
		WITH buckets AS (
		    SELECT generate_series(
		        date_trunc($4, $1::timestamptz),
		        date_trunc($4, $2::timestamptz),
		        ('1 ' || $4)::interval) AS at
		)
		SELECT b.at, count(e.id),
		       COALESCE(sum(e.input_tokens + e.output_tokens), 0),
		       COALESCE(sum(e.cost_micros), 0),
		       count(e.id) FILTER (WHERE e.status >= 500)
		FROM buckets b
		LEFT JOIN usage_events e
		     ON date_trunc($4, e.ts) = b.at
		    AND e.ts >= $1 AND e.ts < $2
		    AND ($3 = '' OR e.org_id = $3)`+sc.narrow("e.", 4)+`
		GROUP BY b.at ORDER BY b.at`,
		append([]any{from, to, orgID, bucket}, sc.args()...)...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (SeriesPoint, error) {
		var p SeriesPoint
		err := r.Scan(&p.At, &p.Requests, &p.Tokens, &p.CostMicros, &p.Errors)
		return p, err
	})
}

// topUsage is the first six buckets of q grouped by groupBy.
func (s *Store) topUsage(ctx context.Context, q UsageQuery, groupBy string) ([]UsageBucket, error) {
	q.GroupBy = groupBy
	b, err := s.Usage(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(b) > 6 {
		return b[:6], nil
	}
	return b, nil
}

// TeamOrg returns which organisation a team belongs to, so a request naming a
// team can be checked against the caller's own tenant first.
func (s *Store) TeamOrg(ctx context.Context, teamID string) (string, error) {
	var orgID string
	err := s.pool.QueryRow(ctx, "SELECT org_id FROM teams WHERE id = $1", teamID).Scan(&orgID)
	if err != nil {
		return "", notFound(err)
	}
	return orgID, nil
}

// KeyOwner returns which organisation a key belongs to and which user it is
// for, empty for a key issued for nobody in particular.
//
// Members may revoke only their own keys, so both are read in one row: a
// second query could see a key that was reassigned in between.
func (s *Store) KeyOwner(ctx context.Context, keyID string) (string, string, error) {
	var orgID, userID string
	err := s.pool.QueryRow(ctx,
		"SELECT org_id, COALESCE(user_id,'') FROM api_keys WHERE id = $1", keyID,
	).Scan(&orgID, &userID)
	if err != nil {
		return "", "", notFound(err)
	}
	return orgID, userID, nil
}

// KeyScope is where a key sits in the hierarchy: its organisation and its
// team (empty for none).
func (s *Store) KeyScope(ctx context.Context, keyID string) (orgID, teamID string, err error) {
	err = s.pool.QueryRow(ctx,
		"SELECT org_id, COALESCE(team_id,'') FROM api_keys WHERE id = $1", keyID,
	).Scan(&orgID, &teamID)
	if err != nil {
		return "", "", notFound(err)
	}
	return orgID, teamID, nil
}

// ScopeName is what one org, team or key is called, for a report that names
// the level a limit came from.
func (s *Store) ScopeName(ctx context.Context, scope policy.ScopeType, id string) (string, error) {
	var query string
	switch scope {
	case policy.ScopeOrg:
		query = "SELECT name FROM orgs WHERE id = $1"
	case policy.ScopeTeam:
		query = "SELECT name FROM teams WHERE id = $1"
	case policy.ScopeKey:
		query = "SELECT alias FROM api_keys WHERE id = $1"
	default:
		return "", ErrNotFound
	}
	var name string
	if err := s.pool.QueryRow(ctx, query, id).Scan(&name); err != nil {
		return "", notFound(err)
	}
	return name, nil
}

// OrgExists reports whether an organisation id names anything.
func (s *Store) OrgExists(ctx context.Context, orgID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1)", orgID).Scan(&exists)
	return exists, err
}

// TeamNames maps team ids to names, for a report grouped by team.
func (s *Store) TeamNames(ctx context.Context, orgID string) (map[string]string, error) {
	return s.names(ctx, "SELECT id, name FROM teams WHERE $1 = '' OR org_id = $1", orgID)
}

// KeyAliases maps key ids to aliases, for a report grouped by key. Revoked
// keys are included, because their history stays.
func (s *Store) KeyAliases(ctx context.Context, orgID string) (map[string]string, error) {
	return s.names(ctx, "SELECT id, alias FROM api_keys WHERE $1 = '' OR org_id = $1", orgID)
}

// UserEmails maps user ids to addresses, for a report grouped by user.
func (s *Store) UserEmails(ctx context.Context, orgID string) (map[string]string, error) {
	return s.names(ctx, "SELECT id, email FROM users WHERE $1 = '' OR org_id = $1", orgID)
}

// names runs an (id, name) query and returns it as a map.
func (s *Store) names(ctx context.Context, sql, orgID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, sql, orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var id, name string
	_, err = pgx.ForEachRow(rows, []any{&id, &name}, func() error {
		out[id] = name
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
}

// KeyQuery narrows a key listing. OrgID is required; the rest are optional.
// UserID is set by a developer's own screen, to see only their keys.
type KeyQuery struct {
	OrgID  string
	TeamID string
	UserID string
	Since  time.Time
}

// KeySummaries lists an organisation's keys with their guardrails and their
// traffic since q.Since, in one query to avoid N+1.
func (s *Store) KeySummaries(ctx context.Context, q KeyQuery) ([]KeySummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT k.id, k.org_id, COALESCE(k.team_id,''), COALESCE(k.user_id,''),
		       k.alias, k.prefix, k.created_at, k.expires_at, k.revoked_at,
		       `+limitColumns+`,
		       u.last_used_at, COALESCE(u.requests, 0), COALESCE(u.micros, 0)
		FROM api_keys k
		LEFT JOIN guardrails p ON p.scope_type = 'key' AND p.scope_id = k.id
		LEFT JOIN LATERAL (
		    SELECT max(e.ts) AS last_used_at,
		           count(*) FILTER (WHERE e.status < 400) AS requests,
		           COALESCE(sum(e.cost_micros), 0) AS micros
		    FROM usage_events e WHERE e.key_id = k.id AND e.ts >= $3
		) u ON true
		WHERE k.org_id = $1 AND ($2 = '' OR k.team_id = $2) AND ($4 = '' OR k.user_id = $4)
		ORDER BY k.created_at DESC`, q.OrgID, q.TeamID, q.Since, q.UserID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanKeySummary)
}

func scanKeySummary(r row) (KeySummary, error) {
	var (
		k      KeySummary
		period *string
	)
	dest := []any{&k.ID, &k.OrgID, &k.TeamID, &k.UserID, &k.Alias, &k.Prefix,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt}
	dest = append(dest, limitTargets(&k.Limits, &period)...)
	dest = append(dest, &k.LastUsedAt, &k.Requests, &k.SpendMicros)
	if err := r.Scan(dest...); err != nil {
		return KeySummary{}, err
	}
	k.Limits.BudgetPeriod = periodPtr(period)
	return k, nil
}

// Setup counts what a deployment has, so the panel can tell a new operator
// which step they are on.
type Setup struct {
	Orgs     int64 `json:"orgs"`
	Teams    int64 `json:"teams"`
	Keys     int64 `json:"keys"`
	Models   int64 `json:"models"`
	People   int64 `json:"people"`
	Requests int64 `json:"requests"`
	// Filters, Routers, Sandboxes and MCP servers are optional. The panel
	// hides their screens until there is at least one.
	Filters    int64 `json:"filters"`
	Routers    int64 `json:"routers"`
	Sandboxes  int64 `json:"sandboxes"`
	MCPServers int64 `json:"mcp_servers"`
}

// SetupState counts the objects a first run has to create. An empty orgID
// counts across every organisation.
func (s *Store) SetupState(ctx context.Context, orgID string) (Setup, error) {
	var st Setup
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM orgs),
		(SELECT count(*) FROM teams  WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM api_keys WHERE ($1 = '' OR org_id = $1) AND revoked_at IS NULL),
		(SELECT count(*) FROM models WHERE enabled AND ($1 = '' OR org_id = $1)),
		(SELECT count(*) FROM users  WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM usage_events WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM filters WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM routers WHERE $1 = '' OR org_id = $1),
		-- Classes, not running machines, so the screen stays when none is up.
		(SELECT count(*) FROM sandbox_classes WHERE $1 = '' OR org_id = $1),
		(SELECT count(*) FROM mcp_servers WHERE $1 = '' OR org_id = $1)`, orgID,
	).Scan(&st.Orgs, &st.Teams, &st.Keys, &st.Models, &st.People, &st.Requests,
		&st.Filters, &st.Routers, &st.Sandboxes, &st.MCPServers)
	return st, err
}

// Refusal is one request the gateway turned away. The status is the reason:
// 402 a spent budget, 429 a rate limit, 404 a model this key may not use.
type Refusal struct {
	TS     time.Time `json:"ts"`
	Alias  string    `json:"alias,omitempty"`
	Status int       `json:"status"`
	KeyID  string    `json:"key_id,omitempty"`
	// Error is what the client was told: the guardrail's wording for a
	// refusal, the inference plane's for a failure.
	Error string `json:"error,omitempty"`
}

// Refusals lists the most recent refused requests made with one of keyIDs,
// newest first. An empty keyIDs returns nothing, not everything: the caller is
// asking about their own keys.
func (s *Store) Refusals(ctx context.Context, orgID string, keyIDs []string,
	since time.Time, limit int) ([]Refusal, error) {
	if len(keyIDs) == 0 {
		return []Refusal{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT ts, alias, status, COALESCE(key_id, ''),
		COALESCE(error, '')
		FROM usage_events
		WHERE org_id = $1 AND key_id = ANY($2) AND ts >= $3 AND status >= 400
		ORDER BY ts DESC LIMIT $4`, orgID, keyIDs, since, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (Refusal, error) {
		var f Refusal
		err := r.Scan(&f.TS, &f.Alias, &f.Status, &f.KeyID, &f.Error)
		return f, err
	})
}
