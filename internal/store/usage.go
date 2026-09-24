package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Event is one completed inference request, as the gateway saw it.
type Event struct {
	TS     time.Time
	OrgID  string
	TeamID string
	UserID string
	KeyID  string
	Alias  string
	// Client is what the client called itself: a known coding agent's name,
	// otherwise the product token of its User-Agent, or empty. It is a claim,
	// never a credential; see internal/connect/identify.go.
	Client      string
	InputTokens int
	// CachedInputTokens is the part of InputTokens the provider served from its
	// prompt cache at a lower price. It lets a row be checked against an
	// invoice. Zero for self-hosted models, which cache nothing.
	CachedInputTokens int
	OutputTokens      int
	CostMicros        int64
	Status            int
	Latency           time.Duration
	TTFT              time.Duration
	Stream            bool
	Estimated         bool
	Canceled          bool
	// Error is what the caller was told went wrong, empty on success: the
	// inference plane's wording for an upstream failure, the guardrail's for a
	// refusal.
	Error string
	// SessionKey names the agent conversation this request belongs to. It is a
	// hash of what the client sent, never the conversation itself, and empty
	// for a request that belongs to none. See internal/gateway/session.go.
	SessionKey string
	// FilterRuns is what each of this request's filters did, in the order they
	// ran. They ride on the event so both are written in the same batch.
	FilterRuns []FilterRun
	// Router is the router that chose the destination, empty for a request
	// that named a model directly. Alias is still the model that answered.
	Router string
	// RouterOutcome says whether the router decided, fell back, or could not
	// decide. RouterMS is what deciding added to the wait.
	RouterOutcome RouterOutcome
	RouterMS      int64
	// Spans is where this request's latency went, step by step. See spans.go.
	Spans []Span
	// Scopes the cost is charged against, so the spend roll-up does not have
	// to work out the hierarchy again.
	Scopes []policy.Scope
	// Tool is set on a tool call through the MCP proxy. The event is then
	// written to tool_calls rather than usage_events. See mcp.go.
	Tool *ToolCall
}

// maxErrorBytes bounds the message kept on a usage row. Upstreams may answer
// with a stack trace or the whole prompt, and a reader only needs the start.
const maxErrorBytes = 1000

// WriteEvents inserts a batch of usage events and their filter runs, adds
// their cost to the spend roll-up, and announces that the log has grown, all
// in one round trip. pg_notify with no listener does nothing.
//
// Spend is summed per window here rather than upserted per event. A batch has
// hundreds of events but touches only a few spend rows, and one upsert per
// event held locks on those busy rows until the batch committed.
func (s *Store) WriteEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	spend := make(map[spendKey]int64)
	for _, e := range events {
		queueEvent(batch, e)
		if e.CostMicros == 0 {
			continue
		}
		for _, sc := range e.Scopes {
			spend[spendKey{sc.Type, sc.ID, sc.Period, sc.Period.Start(e.TS)}] += e.CostMicros
		}
	}
	for _, k := range spendOrder(spend) {
		batch.Queue(`INSERT INTO spend (scope_type, scope_id, period, period_start, micros)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (scope_type, scope_id, period, period_start)
			DO UPDATE SET micros = spend.micros + EXCLUDED.micros`,
			string(k.scopeType), k.scopeID, string(k.period), k.start, spend[k])
	}
	batch.Queue("SELECT pg_notify($1, '')", EventsChannel)
	return s.pool.SendBatch(ctx, batch).Close()
}

// queueEvent adds one event's usage row and its filter runs to the batch.
func queueEvent(batch *pgx.Batch, e Event) {
	if e.Tool != nil {
		queueToolCall(batch, e)
		return
	}
	batch.Queue(`INSERT INTO usage_events (ts, org_id, team_id, user_id, key_id, alias,
		client, input_tokens, cached_input_tokens, output_tokens, cost_micros, status,
		latency_ms, ttft_ms, stream, estimated, canceled, error, session_key, router,
		router_outcome, router_ms, spans)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,
		$22,$23)`,
		e.TS, e.OrgID, nullable(e.TeamID), nullable(e.UserID), nullable(e.KeyID), e.Alias,
		nullable(e.Client),
		e.InputTokens, e.CachedInputTokens, e.OutputTokens, e.CostMicros, e.Status,
		e.Latency.Milliseconds(), e.TTFT.Milliseconds(), e.Stream, e.Estimated, e.Canceled,
		nullable(truncate(e.Error, maxErrorBytes)), nullable(e.SessionKey),
		nullable(e.Router), nullable(string(e.RouterOutcome)), e.RouterMS,
		spansJSON(e.Spans))
	queueFilterRuns(batch, e, e.Alias)
}

// queueFilterRuns adds an event's filter runs to the batch, against what the
// request was addressed to.
func queueFilterRuns(batch *pgx.Batch, e Event, alias string) {
	for _, f := range e.FilterRuns {
		batch.Queue(`INSERT INTO filter_runs (ts, org_id, filter, mode, shadow, outcome,
			latency_ms, cost_micros, segments, changed, team_id, user_id, key_id, alias)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			e.TS, e.OrgID, f.Filter, string(f.Mode), f.Shadow, string(f.Outcome),
			f.LatencyMS, f.CostMicros, f.Segments, f.Changed,
			nullable(e.TeamID), nullable(e.UserID), nullable(e.KeyID), alias)
	}
}

// spendKey is one budget window: the row the roll-up upserts into.
//
// The period start is part of it because a batch can cross midnight or a month
// boundary. It is safe as a map key because Period.Start always builds its
// result in UTC, so equal windows are identical values.
type spendKey struct {
	scopeType policy.ScopeType
	scopeID   string
	period    policy.Period
	start     time.Time
}

// spendOrder sorts the windows a batch touches. Every replica writes the same
// few rows, and taking their locks in one fixed order avoids deadlocks.
func spendOrder(spend map[spendKey]int64) []spendKey {
	keys := make([]spendKey, 0, len(spend))
	for k := range spend {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b spendKey) int {
		return cmp.Or(
			cmp.Compare(a.scopeType, b.scopeType),
			cmp.Compare(a.scopeID, b.scopeID),
			cmp.Compare(a.period, b.period),
			a.start.Compare(b.start),
		)
	})
	return keys
}

// SpendRow is current spend against one budget window.
type SpendRow struct {
	ScopeType   policy.ScopeType `json:"scope_type"`
	ScopeID     string           `json:"scope_id"`
	Period      policy.Period    `json:"period"`
	PeriodStart time.Time        `json:"period_start"`
	Micros      int64            `json:"micros"`
}

// LoadSpend reads every spend row for the currently open day and month
// windows. The gateway refreshes its budget view from this.
func (s *Store) LoadSpend(ctx context.Context, now time.Time) ([]SpendRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT scope_type, scope_id, period, period_start, micros
		FROM spend WHERE (period = 'day' AND period_start = $1)
		           OR (period = 'month' AND period_start = $2)`,
		policy.PeriodDay.Start(now), policy.PeriodMonth.Start(now))
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (SpendRow, error) {
		var sr SpendRow
		err := r.Scan(&sr.ScopeType, &sr.ScopeID, &sr.Period, &sr.PeriodStart, &sr.Micros)
		return sr, err
	})
}

// UsageBucket is one row of an aggregated usage report.
type UsageBucket struct {
	Group        string `json:"group"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicros   int64  `json:"cost_micros"`
}

// Scope narrows a report to one team, key, person or model inside the
// organisation. The zero value is the whole tenant.
//
// One type for every report, so the totals, the chart and the request log of
// an entity's screen are narrowed the same way.
type Scope struct {
	TeamID string
	KeyID  string
	UserID string
	// Alias is the model, named the way a client names it.
	Alias string
}

// Empty reports whether this scope narrows anything at all.
func (sc Scope) Empty() bool {
	return sc.TeamID == "" && sc.KeyID == "" && sc.UserID == "" && sc.Alias == ""
}

// scopeClause is the SQL every scoped report shares, written against the four
// placeholders args appends in the same order.
const scopeClause = ` AND ($%[2]d = '' OR %[1]steam_id = $%[2]d)
	AND ($%[3]d = '' OR %[1]skey_id = $%[3]d)
	AND ($%[4]d = '' OR %[1]suser_id = $%[4]d)
	AND ($%[5]d = '' OR %[1]salias = $%[5]d)`

// args returns the scope's four values, in the order narrow numbers them.
func (sc Scope) args() []any {
	return []any{sc.TeamID, sc.KeyID, sc.UserID, sc.Alias}
}

// narrow renders the scope's conditions for a query whose last parameter is
// number n, with each column prefixed by table ("e." or ""). The numbers are
// computed, so adding a parameter to a query cannot shift them silently.
func (sc Scope) narrow(table string, n int) string {
	return fmt.Sprintf(scopeClause, table, n+1, n+2, n+3, n+4)
}

// UsageQuery narrows a usage report.
type UsageQuery struct {
	OrgID string
	Scope
	From    time.Time
	To      time.Time
	GroupBy string // team, key, user, model, client, day or org; anything else groups by model
}

// groupColumns maps the public group_by values to columns, which keeps the
// caller's string out of the SQL text.
var groupColumns = map[string]string{
	"team":   "COALESCE(team_id, '')",
	"key":    "COALESCE(key_id, '')",
	"user":   "COALESCE(user_id, '')",
	"model":  "alias",
	"client": "COALESCE(client, '')",
	"day":    "to_char(date_trunc('day', ts), 'YYYY-MM-DD')",
	"org":    "org_id",
}

// ValidGroupBy reports whether s is a grouping Usage understands. The empty
// string is not one: the handler picks the default.
func ValidGroupBy(s string) bool {
	_, ok := groupColumns[s]
	return ok
}

// GroupBys lists the groupings, for the error a refused one earns.
func GroupBys() []string {
	return sortedKeys(groupColumns)
}

// sortedKeys returns the keys of m in order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Usage aggregates the event log: what billing and the usage report read.
//
// It counts only what was served. A refusal consumed nothing, so counting it
// would put a request next to a cost that does not explain it.
func (s *Store) Usage(ctx context.Context, q UsageQuery) ([]UsageBucket, error) {
	col, ok := groupColumns[q.GroupBy]
	if !ok {
		col = groupColumns["model"]
	}
	sql := `SELECT ` + col + ` AS grp, count(*), COALESCE(sum(input_tokens),0),
		COALESCE(sum(output_tokens),0), COALESCE(sum(cost_micros),0)
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND ($3 = '' OR org_id = $3) AND status < 400` +
		q.narrow("", 3) + `
		GROUP BY grp ORDER BY 5 DESC`
	rows, err := s.pool.Query(ctx, sql, append([]any{q.From, q.To, q.OrgID}, q.args()...)...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (UsageBucket, error) {
		var b UsageBucket
		err := r.Scan(&b.Group, &b.Requests, &b.InputTokens, &b.OutputTokens, &b.CostMicros)
		return b, err
	})
}

// Audit appends one control-plane action to the audit log. The gateway path
// does not write here; its record is the usage event.
//
// orgID is the tenant the action happened in, so no tenant can read another's
// history. It is empty only for actions outside any tenant, such as a change
// to the shared catalogue.
func (s *Store) Audit(ctx context.Context, actor, orgID, action, targetType, targetID string, detail any) error {
	var raw []byte
	if detail != nil {
		var err error
		if raw, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_log
		(actor, org_id, action, target_type, target_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		actor, nullable(orgID), action, nullable(targetType), nullable(targetID), raw)
	return err
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	ID         int64           `json:"id"`
	TS         time.Time       `json:"ts"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type,omitempty"`
	TargetID   string          `json:"target_id,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

// AuditQuery narrows the audit log. Every field is optional; the zero value is
// "the most recent entries of this tenant".
type AuditQuery struct {
	OrgID  string
	Actor  string
	Action string
	From   time.Time
	To     time.Time
	// Before pages backwards to entries with a lower id. Ids only grow, so it
	// stays stable while the log is written to, unlike an offset.
	Before int64
	Limit  int
}

// ListAudit returns audit entries matching q, newest first.
//
// An empty OrgID means every tenant, which only an operator asks for. Anyone
// else sees only their own organisation's entries, not the unscoped ones.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, actor, action, COALESCE(target_type,''),
		COALESCE(target_id,''), detail FROM audit_log
		WHERE ($1 = '' OR org_id = $1)
		  AND ($2 = '' OR actor = $2)
		  AND ($3 = '' OR action = $3)
		  AND ($4::timestamptz IS NULL OR ts >= $4)
		  AND ($5::timestamptz IS NULL OR ts < $5)
		  AND ($6 = 0 OR id < $6)
		ORDER BY id DESC LIMIT $7`,
		q.OrgID, q.Actor, q.Action, nullableTime(q.From), nullableTime(q.To), q.Before, q.Limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (AuditEntry, error) {
		var a AuditEntry
		err := r.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.TargetType, &a.TargetID, &a.Detail)
		return a, err
	})
}

// AuditFacets is what the log can be filtered by: the actions and actors that
// actually occur in this tenant, so no filter is offered that matches nothing.
type AuditFacets struct {
	Actions []string `json:"actions"`
	Actors  []string `json:"actors"`
}

// AuditFilters reads the distinct actions and actors in one tenant's log.
func (s *Store) AuditFilters(ctx context.Context, orgID string) (AuditFacets, error) {
	f := AuditFacets{Actions: []string{}, Actors: []string{}}
	rows, err := s.pool.Query(ctx, `
		SELECT 'action', action FROM audit_log WHERE $1 = '' OR org_id = $1
		GROUP BY action
		UNION ALL
		SELECT 'actor', actor FROM audit_log WHERE $1 = '' OR org_id = $1
		GROUP BY actor
		ORDER BY 1, 2`, orgID)
	if err != nil {
		return f, err
	}
	var kind, value string
	_, err = pgx.ForEachRow(rows, []any{&kind, &value}, func() error {
		if kind == "action" {
			f.Actions = append(f.Actions, value)
		} else {
			f.Actors = append(f.Actors, value)
		}
		return nil
	})
	return f, err
}

// truncate bounds a string to n bytes without splitting a rune. It marks the
// cut, so a shortened message does not look like the whole one.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
