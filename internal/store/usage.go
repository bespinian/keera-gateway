package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Event is one completed inference request, as the gateway saw it.
type Event struct {
	TS        time.Time
	OrgID     string
	ProjectID string
	UserID    string
	KeyID     string
	Alias     string
	// Client is what the client called itself: a known coding agent's name,
	// otherwise the product token of its User-Agent, or empty. It is a claim,
	// never a credential; see internal/connect/identify.go.
	Client string
	// RequestID is the id the gateway answered with in X-Request-Id, and
	// ClientRequestID the client's own X-Client-Request-Id, or empty.
	RequestID       string
	ClientRequestID string
	InputTokens     int
	// CachedInputTokens is the part of InputTokens the provider served from its
	// prompt cache at a lower price. It lets a row be checked against an
	// invoice. Zero for self-hosted models, which cache nothing.
	CachedInputTokens int
	// CacheWriteTokens is the part of InputTokens the provider wrote to its
	// prompt cache, which some charge more for.
	CacheWriteTokens int
	// CacheWriteHourTokens is the part of CacheWriteTokens kept for an hour.
	// It is not stored: it only prices the request.
	CacheWriteHourTokens int
	OutputTokens         int
	CostMicros           int64
	// ListCostMicros is what a request a subscription paid for would have
	// cost at the model's API prices. No budget is charged it.
	ListCostMicros int64
	// Plan is how much of the caller's Claude plan is used, on a request a
	// subscription paid for. Only the newest one per key is kept.
	Plan      *policy.PlanUsage
	Status    int
	Latency   time.Duration
	TTFT      time.Duration
	Stream    bool
	Estimated bool
	Canceled  bool
	// Error is what the caller was told went wrong, empty on success: the
	// inference plane's wording for an upstream failure, the guardrail's for a
	// refusal.
	Error string
	// Note is what went wrong on the way to an answer that was served all the
	// same, such as a router falling back. Unlike Error, it does not make the
	// request a failure.
	Note string
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
	// Bills is every call this event made to a model on the deployment's
	// own provider key. Each is a row in billing_lines.
	Bills []policy.BillLine
	// BillsOnly is a check's calls: billed, but not a request, so no usage
	// row is written for it.
	BillsOnly bool
}

// maxErrorBytes bounds the message kept on a usage row. Upstreams may answer
// with a stack trace or the whole prompt, and a reader only needs the start.
const maxErrorBytes = 1000

// LostEvents is what WriteEvents returns when Postgres refused some events
// even on their own. The others were written.
type LostEvents struct {
	N   int
	Err error
}

func (e *LostEvents) Error() string {
	return fmt.Sprintf("%d usage events could not be written: %v", e.N, e.Err)
}

func (e *LostEvents) Unwrap() error { return e.Err }

// WriteEvents inserts a batch of usage events and their filter runs, adds
// them to the spend and daily roll-ups, and announces that the log has grown,
// in one round trip. pg_notify with no listener does nothing.
//
// A batch is one transaction, so one row Postgres refuses would lose the
// whole batch. Each event is then written again on its own; one still
// refused keeps its bills but loses its usage row.
func (s *Store) WriteEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	for i := range events {
		events[i].clean()
	}
	err := s.writeBatch(ctx, events)
	if !refused(err) {
		return err
	}
	lost := &LostEvents{Err: err}
	for _, e := range events {
		err := s.writeBatch(ctx, []Event{e})
		if !refused(err) {
			if err != nil {
				return err
			}
			continue
		}
		lost.N++
		if len(e.Bills) > 0 && !e.BillsOnly {
			e.BillsOnly, e.Plan = true, nil
			if err := s.writeBatch(ctx, []Event{e}); err != nil && !refused(err) {
				return err
			}
		}
	}
	if lost.N > 0 {
		return lost
	}
	return nil
}

// refused reports whether Postgres itself refused a write. A batch is one
// transaction, so nothing of a refused one was written.
func refused(err error) bool {
	_, ok := errors.AsType[*pgconn.PgError](err)
	return ok
}

// writeBatch writes events in one round trip.
//
// Spend is summed per window here rather than upserted per event. A batch has
// hundreds of events but touches only a few spend rows, and one upsert per
// event held locks on those busy rows until the batch committed.
func (s *Store) writeBatch(ctx context.Context, events []Event) error {
	batch := &pgx.Batch{}
	spend := make(map[spendKey]int64)
	plans := make(map[string]policy.PlanUsage)
	daily := make(map[dailyKey]*dailySum)
	for _, e := range events {
		if !e.BillsOnly {
			queueEvent(batch, e)
			if e.Tool == nil {
				addDaily(daily, e)
			}
		}
		queueBills(batch, e)
		if e.Plan != nil && e.KeyID != "" && !e.Plan.UpdatedAt.Before(plans[e.KeyID].UpdatedAt) {
			plans[e.KeyID] = *e.Plan
		}
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
	queueDaily(batch, daily)
	queueCredit(batch, events)
	queuePlans(batch, plans)
	if payload, ok := eventsPayload(events); ok {
		batch.Queue("SELECT pg_notify($1, $2)", EventsChannel, payload)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// clean makes every text an event carries fit for Postgres, which refuses a
// NUL byte or invalid UTF-8 in text and JSON. Much of it is what a client or
// an upstream sent, such as the model name or an error message.
func (e *Event) clean() {
	for _, s := range []*string{&e.Alias, &e.Client, &e.Error, &e.Note, &e.SessionKey, &e.Router,
		&e.RequestID, &e.ClientRequestID} {
		*s = cleanText(*s)
	}
	if e.Tool != nil {
		e.Tool.Server, e.Tool.Tool = cleanText(e.Tool.Server), cleanText(e.Tool.Tool)
	}
	for i := range e.FilterRuns {
		e.FilterRuns[i].Filter = cleanText(e.FilterRuns[i].Filter)
	}
	for i := range e.Spans {
		e.Spans[i].Of, e.Spans[i].Note = cleanText(e.Spans[i].Of), cleanText(e.Spans[i].Note)
	}
	for i := range e.Bills {
		e.Bills[i].Alias = cleanText(e.Bills[i].Alias)
		e.Bills[i].BackendModel = cleanText(e.Bills[i].BackendModel)
	}
}

// cleanText drops NUL bytes and replaces invalid UTF-8.
func cleanText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// queueEvent adds one event's usage row and its filter runs to the batch.
func queueEvent(batch *pgx.Batch, e Event) {
	if e.Tool != nil {
		queueToolCall(batch, e)
		return
	}
	batch.Queue(`INSERT INTO usage_events (ts, org_id, project_id, user_id, key_id, alias,
		client, input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, cost_micros,
		list_cost_micros, status, latency_ms, ttft_ms, stream, estimated, canceled, error,
		session_key, router, router_outcome, router_ms, spans, request_id, client_request_id,
		note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,
		$22,$23,$24,$25,$26,$27,$28)`,
		e.TS, e.OrgID, nullable(e.ProjectID), nullable(e.UserID), nullable(e.KeyID), e.Alias,
		nullable(e.Client),
		e.InputTokens, e.CachedInputTokens, e.CacheWriteTokens, e.OutputTokens, e.CostMicros,
		e.ListCostMicros,
		e.Status,
		e.Latency.Milliseconds(), e.TTFT.Milliseconds(), e.Stream, e.Estimated, e.Canceled,
		nullable(truncate(e.Error, maxErrorBytes)), nullable(e.SessionKey),
		nullable(e.Router), nullable(string(e.RouterOutcome)), e.RouterMS,
		spansJSON(e.Spans), nullable(e.RequestID), nullable(e.ClientRequestID),
		nullable(truncate(e.Note, maxErrorBytes)))
	queueFilterRuns(batch, e, e.Alias)
}

// queuePlans keeps the newest plan usage of each key. A row from another
// replica's later batch can arrive first, so an older reading never replaces a
// newer one. Keys are sorted so every replica takes the row locks in one order.
func queuePlans(batch *pgx.Batch, plans map[string]policy.PlanUsage) {
	for _, keyID := range slices.Sorted(maps.Keys(plans)) {
		p := plans[keyID]
		batch.Queue(`INSERT INTO plan_usage (key_id, five_hour, five_hour_resets_at,
			seven_day, seven_day_resets_at, status, updated_at)
			SELECT $1,$2,$3,$4,$5,$6,$7 WHERE EXISTS (SELECT 1 FROM api_keys WHERE id = $1)
			ON CONFLICT (key_id) DO UPDATE SET five_hour = EXCLUDED.five_hour,
				five_hour_resets_at = EXCLUDED.five_hour_resets_at,
				seven_day = EXCLUDED.seven_day,
				seven_day_resets_at = EXCLUDED.seven_day_resets_at,
				status = EXCLUDED.status, updated_at = EXCLUDED.updated_at
			WHERE plan_usage.updated_at <= EXCLUDED.updated_at`,
			keyID, p.FiveHour, p.FiveHourResetsAt, p.SevenDay, p.SevenDayResetsAt, p.Status,
			p.UpdatedAt)
	}
}

// queueFilterRuns adds an event's filter runs to the batch, against what the
// request was addressed to.
func queueFilterRuns(batch *pgx.Batch, e Event, alias string) {
	for _, f := range e.FilterRuns {
		batch.Queue(`INSERT INTO filter_runs (ts, org_id, filter, mode, shadow, outcome,
			latency_ms, cost_micros, segments, changed, project_id, user_id, key_id, alias)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			e.TS, e.OrgID, f.Filter, string(f.Mode), f.Shadow, string(f.Outcome),
			f.LatencyMS, f.CostMicros, f.Segments, f.Changed,
			nullable(e.ProjectID), nullable(e.UserID), nullable(e.KeyID), alias)
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
	return slices.SortedFunc(maps.Keys(spend), func(a, b spendKey) int {
		return cmp.Or(
			cmp.Compare(a.scopeType, b.scopeType),
			cmp.Compare(a.scopeID, b.scopeID),
			cmp.Compare(a.period, b.period),
			a.start.Compare(b.start),
		)
	})
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
	return queryAll(ctx, s.pool, scanSpendRow, `SELECT scope_type, scope_id, period, period_start, micros
		FROM spend WHERE (period = 'day' AND period_start = $1)
		           OR (period = 'month' AND period_start = $2)`,
		policy.PeriodDay.Start(now), policy.PeriodMonth.Start(now))
}

func scanSpendRow(r row) (SpendRow, error) {
	var sr SpendRow
	err := r.Scan(&sr.ScopeType, &sr.ScopeID, &sr.Period, &sr.PeriodStart, &sr.Micros)
	return sr, err
}

// UsageBucket is one row of an aggregated usage report.
type UsageBucket struct {
	Group string `json:"group"`
	// OrgID is set only when a report across every organisation groups by
	// model: two organisations can each have a model of the same alias, and
	// they are two rows, not one.
	OrgID        string `json:"org_id,omitempty"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicros   int64  `json:"cost_micros"`
	// SubscriptionMicros is what Claude subscriptions paid for, at API
	// prices. It is not part of CostMicros.
	SubscriptionMicros int64 `json:"subscription_micros"`
}

// ReportScope narrows a report to one project, key, person or model inside the
// organisation. The zero value is the whole tenant. The usage reports and the
// request log share it, so an entity's screen is narrowed the same way
// throughout.
type ReportScope struct {
	ProjectID string
	KeyID     string
	UserID    string
	// Alias is the model, named the way a client names it.
	Alias string
}

func (sc ReportScope) Empty() bool {
	return sc.ProjectID == "" && sc.KeyID == "" && sc.UserID == "" && sc.Alias == ""
}

// scopeClause is the SQL every scoped report shares, written against the four
// placeholders args appends in the same order.
const scopeClause = ` AND ($%[2]d = '' OR %[1]sproject_id = $%[2]d)
	AND ($%[3]d = '' OR %[1]skey_id = $%[3]d)
	AND ($%[4]d = '' OR %[1]suser_id = $%[4]d)
	AND ($%[5]d = '' OR %[1]salias = $%[5]d)`

// args returns the scope's four values, in the order narrow numbers them.
func (sc ReportScope) args() []any {
	return []any{sc.ProjectID, sc.KeyID, sc.UserID, sc.Alias}
}

// narrow renders the scope's conditions for a query whose last parameter is
// number n, with each column prefixed by table ("e." or ""). The numbers are
// computed, so adding a parameter to a query cannot shift them silently.
func (sc ReportScope) narrow(table string, n int) string {
	return fmt.Sprintf(scopeClause, table, n+1, n+2, n+3, n+4)
}

type UsageQuery struct {
	OrgID string
	ReportScope
	From    time.Time
	To      time.Time
	GroupBy string // project, key, user, model, client, day or org; empty or anything else means DefaultGroupBy
}

// DefaultGroupBy is the grouping a usage report gets when it asks for none.
const DefaultGroupBy = "model"

// groupBy is q.GroupBy, or DefaultGroupBy for one Usage does not know.
func (q UsageQuery) groupBy() string {
	if ValidGroupBy(q.GroupBy) {
		return q.GroupBy
	}
	return DefaultGroupBy
}

// groupColumns maps the public group_by values to columns, which keeps the
// caller's string out of the SQL text.
var groupColumns = map[string]string{
	"project": "COALESCE(project_id, '')",
	"key":     "COALESCE(key_id, '')",
	"user":    "COALESCE(user_id, '')",
	"model":   "alias",
	"client":  "COALESCE(client, '')",
	"day":     "to_char(date_trunc('day', ts), 'YYYY-MM-DD')",
	"org":     "org_id",
}

// ValidGroupBy reports whether s is a grouping Usage understands. The empty
// string is not one: Usage reads it as DefaultGroupBy.
func ValidGroupBy(s string) bool {
	_, ok := groupColumns[s]
	return ok
}

// GroupBys lists the groupings, for the error a refused one earns.
func GroupBys() []string {
	return slices.Sorted(maps.Keys(groupColumns))
}

// Usage aggregates the event log for the usage report.
//
// Cost and tokens add up every row, as budgets do: a refused or failed
// request can still have paid for a filter or router call. Requests counts
// only what was served, as the keys screen does.
func (s *Store) Usage(ctx context.Context, q UsageQuery) ([]UsageBucket, error) {
	src, from, to := sourceFor(q.From, q.To)
	groupBy := q.groupBy()
	col := src.group(groupBy)
	org, group := "''", "grp"
	if q.OrgID == "" && groupBy == "model" {
		org, group = "org_id", "grp, org_id"
	}
	// Days read as a timeline; every other grouping is ranked by cost.
	order := "6 DESC"
	if groupBy == "day" {
		order = "grp"
	}
	// The alias is whatever the client sent, so a mistyped name is on its
	// refusal's row. A name that was never served and cost nothing is not a
	// model; the request log still shows the refusal.
	having := ""
	if groupBy == "model" {
		having = ` HAVING ` + src.served + ` > 0
		    OR sum(input_tokens + output_tokens) > 0
		    OR sum(cost_micros + list_cost_micros) > 0`
	}
	sql := `SELECT ` + col + ` AS grp, ` + org + `, ` + src.served + `,
		COALESCE(sum(input_tokens),0),
		COALESCE(sum(output_tokens),0), COALESCE(sum(cost_micros),0),
		COALESCE(sum(list_cost_micros),0)
		FROM ` + src.table + `
		WHERE ` + src.window + ` AND ($3 = '' OR org_id = $3)` +
		q.narrow("", 3) + `
		GROUP BY ` + group + having + ` ORDER BY ` + order
	return queryAll(ctx, s.pool, scanUsageBucket, sql, append([]any{from, to, q.OrgID}, q.args()...)...)
}

// UsageChart is the usage report's chart: served requests over time, one line
// for each of the busiest groups and one for the rest.
type UsageChart struct {
	Bucket string      `json:"bucket"`
	At     []time.Time `json:"at"`
	Lines  []UsageLine `json:"lines"`
}

// UsageLine is one group's served requests, a count for each time in At.
type UsageLine struct {
	Group string `json:"group"`
	OrgID string `json:"org_id,omitempty"`
	// Other sums every group without a line of its own.
	Other  bool    `json:"other,omitempty"`
	Total  int64   `json:"total"`
	Counts []int64 `json:"counts"`
}

// chartLines is how many groups get a line of their own. The panel has six
// series colours, and the sixth is for the rest.
const chartLines = 5

// UsageChart draws the report Usage lists over time. It counts what Usage
// counts, so the lines add up to the report's served requests.
func (s *Store) UsageChart(ctx context.Context, q UsageQuery) (UsageChart, error) {
	src, from, to := sourceFor(q.From, q.To)
	groupBy := q.groupBy()
	col := src.group(groupBy)
	org := "''"
	if q.OrgID == "" && groupBy == "model" {
		org = "org_id"
	}
	// Each day is a single point on a chart over days, so a report by day
	// draws one line.
	if groupBy == "day" {
		col = "''"
	}
	c := UsageChart{Bucket: chartBucket(from, to)}

	// Every bucket in the window is listed, as on the dashboard, so a quiet
	// stretch shows as a dip.
	var err error
	c.At, err = queryAll(ctx, s.pool, func(r row) (time.Time, error) {
		var t time.Time
		return t, r.Scan(&t)
	}, `SELECT generate_series(date_trunc($3, $1::timestamptz),
		date_trunc($3, $2::timestamptz), ('1 ' || $3)::interval)`,
		from, src.lastBucket(to), c.Bucket)
	if err != nil {
		return c, err
	}
	index := make(map[int64]int, len(c.At))
	for i, t := range c.At {
		index[t.UnixMicro()] = i
	}

	type point struct {
		at         time.Time
		group, org string
		other      bool
		n          int64
	}
	points, err := queryAll(ctx, s.pool, func(r row) (point, error) {
		var p point
		return p, r.Scan(&p.at, &p.group, &p.org, &p.other, &p.n)
	}, `
		WITH counts AS (
		    SELECT date_trunc($4, `+src.at+`) AS at, `+col+` AS grp, `+org+` AS org,
		           `+src.served+` AS n
		    FROM `+src.table+`
		    WHERE `+src.window+` AND ($3 = '' OR org_id = $3)`+q.narrow("", 4)+`
		    GROUP BY 1, 2, 3
		), top AS (
		    SELECT grp, org FROM counts
		    GROUP BY grp, org HAVING sum(n) > 0
		    ORDER BY sum(n) DESC, grp LIMIT `+fmt.Sprint(chartLines)+`
		)
		SELECT c.at, COALESCE(t.grp, ''), COALESCE(t.org, ''), t.grp IS NULL,
		       sum(c.n)::bigint
		FROM counts c LEFT JOIN top t ON t.grp = c.grp AND t.org = c.org
		GROUP BY 1, 2, 3, 4`,
		append([]any{from, to, q.OrgID, c.Bucket}, q.args()...)...)
	if err != nil {
		return c, err
	}

	lines := map[[2]string]*UsageLine{}
	var other *UsageLine
	for _, p := range points {
		i, ok := index[p.at.UnixMicro()]
		if !ok || p.n == 0 {
			continue
		}
		l := other
		if !p.other {
			l = lines[[2]string{p.group, p.org}]
		}
		if l == nil {
			l = &UsageLine{Group: p.group, OrgID: p.org, Other: p.other,
				Counts: make([]int64, len(c.At))}
			if p.other {
				other = l
			} else {
				lines[[2]string{p.group, p.org}] = l
			}
		}
		l.Counts[i] += p.n
		l.Total += p.n
	}
	for _, l := range lines {
		c.Lines = append(c.Lines, *l)
	}
	slices.SortFunc(c.Lines, func(a, b UsageLine) int {
		return cmp.Or(cmp.Compare(b.Total, a.Total), cmp.Compare(a.Group, b.Group))
	})
	if other != nil {
		c.Lines = append(c.Lines, *other)
	}
	return c, nil
}

func scanUsageBucket(r row) (UsageBucket, error) {
	var b UsageBucket
	err := r.Scan(&b.Group, &b.OrgID, &b.Requests, &b.InputTokens, &b.OutputTokens,
		&b.CostMicros, &b.SubscriptionMicros)
	return b, err
}

// Audit appends one control-plane action to the audit log. The gateway path
// does not write here; its record is the usage event.
//
// orgID is the tenant the action happened in, so no tenant can read another's
// history.
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
		actor, orgID, action, nullable(targetType), nullable(targetID), raw)
	return err
}

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	ID         int64           `json:"id"`
	CreatedAt  time.Time       `json:"created_at"`
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
// else sees only their own organisation's entries.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	q.Limit = pageLimit(q.Limit, 100, 5000)
	return queryAll(ctx, s.pool, scanAuditEntry, `SELECT id, ts, actor, action, COALESCE(target_type,''),
		COALESCE(target_id,''), detail FROM audit_log
		WHERE ($1 = '' OR org_id = $1)
		  AND ($2 = '' OR actor = $2)
		  AND ($3 = '' OR action = $3)
		  AND ($4::timestamptz IS NULL OR ts >= $4)
		  AND ($5::timestamptz IS NULL OR ts < $5)
		  AND ($6 = 0 OR id < $6)
		ORDER BY id DESC LIMIT $7`,
		q.OrgID, q.Actor, q.Action, nullableTime(q.From), nullableTime(q.To), q.Before, q.Limit)
}

func scanAuditEntry(r row) (AuditEntry, error) {
	var a AuditEntry
	err := r.Scan(&a.ID, &a.CreatedAt, &a.Actor, &a.Action, &a.TargetType, &a.TargetID, &a.Detail)
	return a, err
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
