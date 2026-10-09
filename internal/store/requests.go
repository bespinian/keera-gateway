package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The request log: every recorded inference request, one row at a time. It
// reads the same table as the dashboard, so the two always agree on what a
// request was.

// Outcome is which requests a log asks for. The unhappy ones are kept apart
// because each needs a different response.
type Outcome string

const (
	// OutcomeAny is every request. An entity's own screen opens on it.
	OutcomeAny Outcome = ""
	// OutcomeOK is a request that was served and finished without error.
	OutcomeOK Outcome = "ok"
	// OutcomeUnhappy is everything that did not deliver: a status of 400 or
	// more, or a message.
	OutcomeUnhappy Outcome = "unhappy"
	// OutcomeFailed is a request the inference plane could not answer. It is
	// the same set the dashboard counts as failed.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused is a request that got a 4xx. Mostly a guardrail stopped
	// it: a spent budget, a rate limit, a model this key may not use, a
	// filter. It can also be a request the gateway or backend could not take,
	// such as one too large. It cost nothing.
	OutcomeRefused Outcome = "refused"
	// OutcomeInterrupted is a request that was answered and then did not
	// finish, such as a stream that ended early. Its status is the 200 the
	// client got; the message says what went wrong afterwards.
	OutcomeInterrupted Outcome = "interrupted"
)

// outcomeClauses maps an outcome to its condition, which keeps the caller's
// string out of the SQL text.
var outcomeClauses = map[Outcome]string{
	OutcomeAny:         "TRUE",
	OutcomeOK:          "status < 400 AND error IS NULL",
	OutcomeUnhappy:     "(status >= 400 OR error IS NOT NULL)",
	OutcomeFailed:      "status >= 500",
	OutcomeRefused:     "status BETWEEN 400 AND 499",
	OutcomeInterrupted: "status < 400 AND error IS NOT NULL",
}

// outcomeClause is the condition for o, or OutcomeAny's for an outcome it does
// not know.
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

// Request is one recorded inference request with what an entity's screen shows
// in a row: when, which model, whose key, what it cost and what happened. It
// covers both served and failed requests, because the log is one table.
type Request struct {
	ID        int64     `json:"id"`
	TS        time.Time `json:"ts"`
	Alias     string    `json:"alias,omitempty"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	ProjectID string    `json:"project_id,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	OrgID     string    `json:"org_id,omitempty"`
	// Client is what sent the request, as it named itself.
	Client      string `json:"client,omitempty"`
	InputTokens int64  `json:"input_tokens"`
	// CachedInputTokens is the part of InputTokens the provider served from its
	// prompt cache at a lower price. Without it the cost cannot be recomputed
	// from the counts.
	CachedInputTokens int64 `json:"cached_input_tokens"`
	// CacheWriteTokens is the part of InputTokens the provider wrote to its
	// prompt cache, for the same reason.
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CostMicros       int64 `json:"cost_micros"`
	LatencyMS        int64 `json:"latency_ms"`
	// TTFTMS is how long the client waited for the first token: the latency a
	// developer notices.
	TTFTMS int64 `json:"ttft_ms"`
	Stream bool  `json:"stream"`
	// Estimated marks token counts the gateway guessed because the client hung
	// up before the upstream reported them. The cost is then an estimate too.
	Estimated bool `json:"estimated"`
	Canceled  bool `json:"canceled"`
	// SessionKey names the agent conversation this request was part of, so a
	// reader can open the whole task. Empty for one that was part of none.
	SessionKey string `json:"session_key,omitempty"`
	// Spans is where the latency went, step by step. It is loaded with the row
	// so opening a slow request needs no second round trip.
	Spans []Span `json:"spans,omitempty"`
}

// requestColumns is the select list every reading of a request row shares, so
// a new column reaches every screen at once. scanRequest reads it in order.
const requestColumns = `id, ts, alias, status, COALESCE(error, ''),
	COALESCE(key_id, ''), COALESCE(project_id, ''), COALESCE(user_id, ''), org_id,
	COALESCE(client, ''), input_tokens, cached_input_tokens, cache_write_tokens,
	output_tokens, cost_micros,
	latency_ms, ttft_ms, stream, estimated, canceled, COALESCE(session_key, ''),
	spans`

func scanRequest(r row) (Request, error) {
	var q Request
	err := r.Scan(&q.ID, &q.TS, &q.Alias, &q.Status, &q.Error,
		&q.KeyID, &q.ProjectID, &q.UserID, &q.OrgID, &q.Client,
		&q.InputTokens, &q.CachedInputTokens, &q.CacheWriteTokens, &q.OutputTokens,
		&q.CostMicros,
		&q.LatencyMS, &q.TTFTMS, &q.Stream, &q.Estimated, &q.Canceled, &q.SessionKey,
		&q.Spans)
	return q, err
}

// RequestQuery narrows the request log. Every field is optional; the zero value
// is "the most recent requests of this tenant".
type RequestQuery struct {
	OrgID string
	ReportScope
	// Holder keeps the log to the requests of one person's keys, whatever
	// else is narrowed. It is how a member reads the log. Unlike UserID, it
	// also narrows the filter choices, which would otherwise count everyone's.
	Holder  string
	Outcome Outcome
	// Status matches one exact status.
	Status int
	// StatusClass matches a whole class of statuses by its first digit: 5 for
	// every server error, 4 for every client error, 2 for every success.
	StatusClass int
	From        time.Time
	To          time.Time
	// Before pages backwards by id. An offset would shift while the log is
	// being written to.
	Before int64
	// After is the other end of the same cursor, for a live reader: everything
	// recorded since the last row shown. The id is used, not a timestamp,
	// because two requests can share a timestamp.
	After int64
	// Skip leaves out rows a live reader already has. A reader that reads
	// past After again, for rows that were committed late, needs it.
	Skip []int64
	// Sort is SortRecent, SortCost, SortTokens or SortFirstToken. Before only
	// pages under SortRecent, where id order and time order match.
	Sort  Sort
	Limit int
}

// requestOrder maps a sort to its ORDER BY, which keeps the caller's string out
// of the SQL text.
var requestOrder = map[Sort]string{
	SortRecent:     "id DESC",
	SortCost:       "cost_micros DESC, id DESC",
	SortTokens:     "input_tokens + output_tokens DESC, id DESC",
	SortFirstToken: "ttft_ms DESC, id DESC",
}

// RequestSorts are the rankings the request log offers.
func RequestSorts() []Sort {
	return []Sort{SortRecent, SortCost, SortTokens, SortFirstToken}
}

// where is the condition on everything in q but the outcome, on $1 to $13.
func (q RequestQuery) where() string {
	return `($1 = '' OR org_id = $1)
		  AND ($2::timestamptz IS NULL OR ts >= $2)
		  AND ($3::timestamptz IS NULL OR ts < $3)
		  AND ($4 = 0 OR status = $4)
		  AND ($5 = 0 OR status / 100 = $5)
		  AND ($6 = 0 OR id < $6)
		  AND ($7 = 0 OR id > $7)
		  AND ($8::bigint[] IS NULL OR id <> ALL($8))
		  AND ($9 = '' OR user_id = $9)` + q.narrow("", 9)
}

// whereArgs are the values where refers to.
func (q RequestQuery) whereArgs() []any {
	return append([]any{q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Status,
		q.StatusClass, q.Before, q.After, q.Skip, q.Holder}, q.args()...)
}

// Requests returns the matching rows in the order q asks for, newest first by
// default.
//
// An empty OrgID means every tenant, which only an operator ever asks for.
func (s *Store) Requests(ctx context.Context, q RequestQuery) ([]Request, error) {
	order, ok := requestOrder[q.Sort]
	if !ok {
		order = requestOrder[SortRecent]
	}
	return queryAll(ctx, s.pool, scanRequest, `SELECT `+requestColumns+`
		FROM usage_events
		WHERE `+outcomeClause(q.Outcome)+` AND `+q.where()+`
		ORDER BY `+order+` LIMIT $14`,
		append(q.whereArgs(), pageLimit(q.Limit, 100, 5000))...)
}

// RequestOutcomes counts one window by outcome, so the log can show how many
// of each there are before anybody narrows to one.
type RequestOutcomes struct {
	Total       int64 `json:"total"`
	OK          int64 `json:"ok"`
	Failed      int64 `json:"failed"`
	Refused     int64 `json:"refused"`
	Interrupted int64 `json:"interrupted"`
}

// Outcomes counts the window q describes by what happened to each request.
//
// The chosen outcome is ignored, or the screen would only offer the one it
// already shows. Everything else in q applies, status and id cursors included,
// so the total matches the table beside it. The live stream uses the cursors
// to count only the rows it has not counted yet.
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

// Add sums two sets of counts.
func (c *RequestOutcomes) Add(d RequestOutcomes) {
	c.Total += d.Total
	c.OK += d.OK
	c.Failed += d.Failed
	c.Refused += d.Refused
	c.Interrupted += d.Interrupted
}

// FacetCount is one value a log can be narrowed by, with how many rows in the
// window have it. The count shows which of many values is the problem.
type FacetCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// readFacets calls add for every (facet, value, count) row.
func readFacets(rows pgx.Rows, add func(facet string, c FacetCount)) error {
	var (
		facet string
		c     FacetCount
	)
	_, err := pgx.ForEachRow(rows, []any{&facet, &c.Value, &c.Count}, func() error {
		add(facet, c)
		return nil
	})
	return err
}

// RequestFacets is what the request log can be narrowed by, over the whole
// window: the models, keys, projects, people and statuses that occur, each with
// its count. They are read from the log rather than listed from the roster,
// so only values that made requests are offered.
//
// There is no total: the outcome counts already carry one. The session list
// reads the same facets but statuses, which belong to a request.
type RequestFacets struct {
	Models   []FacetCount `json:"models"`
	Keys     []FacetCount `json:"keys"`
	Projects []FacetCount `json:"projects"`
	Users    []FacetCount `json:"users"`
	Statuses []FacetCount `json:"statuses,omitempty"`
}

// add files one counted value under its facet.
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
//
// Only the tenant, the holder, the outcome and the time bounds of q are read, so the
// screen still offers the other values after one is picked. A model picked
// after a project can then match nothing, and the screen says so.
func (s *Store) RequestFilters(ctx context.Context, q RequestQuery) (RequestFacets, error) {
	f := RequestFacets{
		Models:   []FacetCount{},
		Keys:     []FacetCount{},
		Projects: []FacetCount{},
		Users:    []FacetCount{},
		Statuses: []FacetCount{},
	}
	rows, err := s.pool.Query(ctx, `
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
		    WHERE alias <> '' GROUP BY alias
		UNION ALL
		SELECT 'key', key_id, count(*) FROM matching WHERE key_id <> '' GROUP BY key_id
		UNION ALL
		SELECT 'project', project_id, count(*) FROM matching WHERE project_id <> '' GROUP BY project_id
		UNION ALL
		SELECT 'user', user_id, count(*) FROM matching WHERE user_id <> '' GROUP BY user_id
		UNION ALL
		SELECT 'status', status::text, count(*) FROM matching GROUP BY status
		ORDER BY facet, 3 DESC, value`,
		q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Holder)
	if err != nil {
		return f, err
	}
	err = readFacets(rows, f.add)
	return f, err
}

// LatestEventID is the highest id in the log, or zero when it is empty. A live
// reader with no row yet starts from it, so the stream does not replay the
// window already on screen.
//
// It is the highest id overall, not the highest matching one. Otherwise a
// reader watching for failures on a deployment with none would start at zero
// and get the whole window when the first one arrived.
func (s *Store) LatestEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT COALESCE(max(id), 0) FROM usage_events").Scan(&id)
	return id, err
}
