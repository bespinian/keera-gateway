package store

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The request log lists recorded inference requests from the same table as
// the dashboard, so the two always agree.

// Outcome is which requests a log asks for.
type Outcome string

const (
	// OutcomeAny is every request.
	OutcomeAny Outcome = ""
	// OutcomeOK is a request that was served without error.
	OutcomeOK Outcome = "ok"
	// OutcomeUnhappy is everything that did not deliver: a status of 400 or
	// more, or a message.
	OutcomeUnhappy Outcome = "unhappy"
	// OutcomeFailed is a request the inference plane could not answer, as the
	// dashboard counts it.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused is a request that got a 4xx, mostly from a guardrail, or
	// because it was too large. It cost nothing.
	OutcomeRefused Outcome = "refused"
	// OutcomeInterrupted is a request that was answered but did not finish,
	// such as a stream that ended early. Its status is the 200 the client got.
	OutcomeInterrupted Outcome = "interrupted"
)

// outcomeClauses maps an outcome to its condition, keeping the caller's string
// out of the SQL.
var outcomeClauses = map[Outcome]string{
	OutcomeAny:         "TRUE",
	OutcomeOK:          "status < 400 AND error IS NULL",
	OutcomeUnhappy:     "(status >= 400 OR error IS NOT NULL)",
	OutcomeFailed:      "status >= 500",
	OutcomeRefused:     "status BETWEEN 400 AND 499",
	OutcomeInterrupted: "status < 400 AND error IS NOT NULL",
}

func ValidOutcome(o Outcome) bool {
	_, ok := outcomeClauses[o]
	return ok
}

// Outcomes lists the outcomes, for the error a refused one earns.
func Outcomes() []string {
	var out []string
	for o := range outcomeClauses {
		if o != OutcomeAny {
			out = append(out, string(o))
		}
	}
	slices.Sort(out)
	return out
}

// outcomeClause is the condition for o, or OutcomeAny's for an unknown one.
func outcomeClause(o Outcome) string {
	if clause, ok := outcomeClauses[o]; ok {
		return clause
	}
	return outcomeClauses[OutcomeAny]
}

// countOutcome counts the rows with outcome o, so every report splits
// requests the way the request log does.
func countOutcome(o Outcome) string {
	return "count(*) FILTER (WHERE " + outcomeClauses[o] + ")"
}

// Request is one recorded inference request, served or failed.
type Request struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Alias     string    `json:"alias,omitempty"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	// Note is a problem on a request that was still served, such as a router
	// falling back.
	Note      string `json:"note,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	OrgID     string `json:"org_id,omitempty"`
	// Client is what sent the request, as it named itself.
	Client      string `json:"client,omitempty"`
	InputTokens int64  `json:"input_tokens"`
	// CachedInputTokens is the part of InputTokens the provider served from its
	// prompt cache at a lower price.
	CachedInputTokens int64 `json:"cached_input_tokens"`
	// CacheWriteTokens is the part of InputTokens the provider wrote to its
	// prompt cache.
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CostMicros       int64 `json:"cost_micros"`
	LatencyMS        int64 `json:"latency_ms"`
	// TTFTMS is how long the client waited for the first token.
	TTFTMS int64 `json:"ttft_ms"`
	Stream bool  `json:"stream"`
	// Estimated marks token counts the gateway guessed because the client hung
	// up before the upstream reported them. The cost is then an estimate too.
	Estimated bool `json:"estimated"`
	Canceled  bool `json:"canceled"`
	// SessionKey names the agent conversation of this request, if any.
	SessionKey string `json:"session_key,omitempty"`
	// Spans is where the latency went, step by step, loaded with the row to
	// save a round trip.
	Spans []Span `json:"spans,omitempty"`
	// RequestID is the id the gateway answered with, and ClientRequestID the
	// client's own. Rows written before v0.7.0 have neither.
	RequestID       string `json:"request_id,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
}

// requestColumns is the select list every read of a request row shares.
// scanRequest reads it in order.
const requestColumns = `id, ts, alias, status, COALESCE(error, ''),
	COALESCE(key_id, ''), COALESCE(project_id, ''), COALESCE(user_id, ''), org_id,
	COALESCE(client, ''), input_tokens, cached_input_tokens, cache_write_tokens,
	output_tokens, cost_micros,
	latency_ms, ttft_ms, stream, estimated, canceled, COALESCE(session_key, ''),
	COALESCE(spans, spans_jsonb::json), COALESCE(request_id, ''),
	COALESCE(client_request_id, ''), COALESCE(note, '')`

func scanRequest(r row) (Request, error) {
	var q Request
	err := r.Scan(&q.ID, &q.CreatedAt, &q.Alias, &q.Status, &q.Error,
		&q.KeyID, &q.ProjectID, &q.UserID, &q.OrgID, &q.Client,
		&q.InputTokens, &q.CachedInputTokens, &q.CacheWriteTokens, &q.OutputTokens,
		&q.CostMicros,
		&q.LatencyMS, &q.TTFTMS, &q.Stream, &q.Estimated, &q.Canceled, &q.SessionKey,
		&q.Spans, &q.RequestID, &q.ClientRequestID, &q.Note)
	return q, err
}

// RequestQuery narrows the request log. Every field is optional.
type RequestQuery struct {
	OrgID string
	ReportScope
	// Holder keeps only one person's requests, for a member reading the log.
	// Unlike UserID, it also narrows the filter choices.
	Holder string
	// RequestID matches the gateway's request id or the client's own.
	RequestID string
	Outcome   Outcome
	Status    int
	// StatusClass matches statuses by their first digit, such as 5 for 5xx.
	StatusClass int
	From        time.Time
	To          time.Time
	// Before pages backwards by id, which, unlike an offset, does not shift
	// while the log grows.
	Before int64
	// After is the other end of the cursor, for a live reader: everything
	// since the last row shown. It uses the id, as timestamps can tie.
	After int64
	// Skip leaves out rows a live reader already has, when it reads past After
	// again for rows committed late.
	Skip []int64
	// Sort is SortRecent, SortCost, SortTokens or SortFirstToken. Before only
	// works under SortRecent.
	Sort  Sort
	Limit int
}

// requestOrder maps a sort to its ORDER BY, keeping the caller's string out of
// the SQL.
var requestOrder = map[Sort]string{
	SortRecent:     "id DESC",
	SortCost:       "cost_micros DESC, id DESC",
	SortTokens:     "input_tokens + output_tokens DESC, id DESC",
	SortFirstToken: "ttft_ms DESC, id DESC",
}

func RequestSorts() []Sort {
	return []Sort{SortRecent, SortCost, SortTokens, SortFirstToken}
}

// where is the condition on everything in q but the outcome, on $1 to $14.
func (q RequestQuery) where() string {
	return `($1 = '' OR org_id = $1)
		  AND ($14 = '' OR request_id = $14 OR client_request_id = $14)
		  AND ($2::timestamptz IS NULL OR ts >= $2)
		  AND ($3::timestamptz IS NULL OR ts < $3)
		  AND ($4 = 0 OR status = $4)
		  AND ($5 = 0 OR status / 100 = $5)
		  AND ($6 = 0 OR id < $6)
		  AND ($7 = 0 OR id > $7)
		  AND ($8::bigint[] IS NULL OR id <> ALL($8))
		  AND ($9 = '' OR user_id = $9)` + q.narrow("", 9)
}

func (q RequestQuery) whereArgs() []any {
	return append(append([]any{
		q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Status,
		q.StatusClass, q.Before, q.After, q.Skip, q.Holder,
	}, q.args()...), q.RequestID)
}

// Requests returns the matching rows in the order q asks for. An empty OrgID
// means every tenant, which only an operator asks for.
func (s *Store) Requests(ctx context.Context, q RequestQuery) ([]Request, error) {
	order, ok := requestOrder[q.Sort]
	if !ok {
		order = requestOrder[SortRecent]
	}
	return queryAll(ctx, s.pool, scanRequest, `SELECT `+requestColumns+`
		FROM usage_events
		WHERE `+outcomeClause(q.Outcome)+` AND `+q.where()+`
		ORDER BY `+order+` LIMIT $15`,
		append(q.whereArgs(), pageLimit(q.Limit, 100, 5000))...)
}

// RequestOutcomes counts one window by outcome.
type RequestOutcomes struct {
	Total       int64 `json:"total"`
	OK          int64 `json:"ok"`
	Failed      int64 `json:"failed"`
	Refused     int64 `json:"refused"`
	Interrupted int64 `json:"interrupted"`
}

// Outcomes counts the window q describes by outcome. It ignores q's outcome,
// so the screen can offer the others, but applies everything else, cursors
// included, so the total matches the table beside it.
func (s *Store) Outcomes(ctx context.Context, q RequestQuery) (RequestOutcomes, error) {
	var c RequestOutcomes
	err := s.pool.QueryRow(ctx, `SELECT count(*),
		`+countOutcome(OutcomeOK)+`,
		`+countOutcome(OutcomeFailed)+`,
		`+countOutcome(OutcomeRefused)+`,
		`+countOutcome(OutcomeInterrupted)+`
		FROM usage_events
		WHERE `+q.where(), q.whereArgs()...,
	).Scan(&c.Total, &c.OK, &c.Failed, &c.Refused, &c.Interrupted)
	return c, err
}

func (c *RequestOutcomes) Add(d RequestOutcomes) {
	c.Total += d.Total
	c.OK += d.OK
	c.Failed += d.Failed
	c.Refused += d.Refused
	c.Interrupted += d.Interrupted
}

// FacetCount is one value a log can be narrowed by, with how many rows in the
// window have it.
type FacetCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// holderFacets counts table's rows by key, project and person, for a facet
// query to append to its own branches.
func holderFacets(table string) string {
	return `
		UNION ALL
		SELECT 'key', key_id, count(*) FROM ` + table + ` WHERE key_id <> '' GROUP BY key_id
		UNION ALL
		SELECT 'project', project_id, count(*) FROM ` + table + ` WHERE project_id <> '' GROUP BY project_id
		UNION ALL
		SELECT 'user', user_id, count(*) FROM ` + table + ` WHERE user_id <> '' GROUP BY user_id`
}

// facets runs a facet query. Every facet starts empty rather than null.
func (s *Store) facets(ctx context.Context, sql string, args ...any) (RequestFacets, error) {
	f := RequestFacets{
		Models:   []FacetCount{},
		Keys:     []FacetCount{},
		Projects: []FacetCount{},
		Users:    []FacetCount{},
		Statuses: []FacetCount{},
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return f, err
	}
	var (
		facet string
		c     FacetCount
	)
	_, err = pgx.ForEachRow(rows, []any{&facet, &c.Value, &c.Count}, func() error {
		f.add(facet, c)
		return nil
	})
	return f, err
}

// RequestFacets is what the request log can be narrowed by: the models, keys,
// projects, people and statuses in the window, each with its count. They come
// from the log, so only values that made requests are offered. The session
// list uses them too, with each session's last status.
type RequestFacets struct {
	Models   []FacetCount `json:"models"`
	Keys     []FacetCount `json:"keys"`
	Projects []FacetCount `json:"projects"`
	Users    []FacetCount `json:"users"`
	Statuses []FacetCount `json:"statuses,omitempty"`
}

func (f *RequestFacets) add(facet string, c FacetCount) {
	switch facet {
	case "model":
		f.Models = append(f.Models, c)
	case "key":
		f.Keys = append(f.Keys, c)
	case "project":
		f.Projects = append(f.Projects, c)
	case "user":
		f.Users = append(f.Users, c)
	case "status":
		f.Statuses = append(f.Statuses, c)
	}
}

// RequestFilters counts the window by model, key, project, person and status.
// It reads only q's tenant, holder, outcome and time bounds, so the screen
// still offers other values after one is picked.
func (s *Store) RequestFilters(ctx context.Context, q RequestQuery) (RequestFacets, error) {
	return s.facets(ctx, `
		WITH matching AS (
		    SELECT alias, COALESCE(key_id, '') AS key_id,
		           COALESCE(project_id, '') AS project_id, COALESCE(user_id, '') AS user_id,
		           status
		    FROM usage_events
		    WHERE `+outcomeClause(q.Outcome)+`
		      AND ($1 = '' OR org_id = $1)
		      AND ($2::timestamptz IS NULL OR ts >= $2)
		      AND ($3::timestamptz IS NULL OR ts < $3)
		      AND ($4 = '' OR user_id = $4)
		)
		SELECT 'model' AS facet, alias AS value, count(*) FROM matching
		    WHERE alias <> '' GROUP BY alias`+holderFacets("matching")+`
		UNION ALL
		SELECT 'status', status::text, count(*) FROM matching GROUP BY status
		ORDER BY facet, 3 DESC, value`,
		q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Holder)
}

// LatestEventID is the highest id in the log, or zero when it is empty. A live
// reader with no row yet starts from it, so it does not replay the window on
// screen. It is the highest overall, not the highest matching, or a reader
// filtering for something rare would start at zero.
func (s *Store) LatestEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT COALESCE(max(id), 0) FROM usage_events").Scan(&id)
	return id, err
}
