package store

import (
	"context"
	"time"
)

// The request log: every recorded inference request, one row at a time. It
// reads the same table as the dashboard and the failure log, so all three
// always agree on what a request was.

// Outcome is which requests a log asks for. It adds "everything" and "the ones
// that worked" to the failure log's kinds.
type Outcome string

const (
	// OutcomeAny is every request. An entity's own screen opens on it.
	OutcomeAny Outcome = ""
	// OutcomeOK is a request that was served and finished without error.
	OutcomeOK Outcome = "ok"
	// OutcomeUnhappy is everything that did not deliver, the same set as the
	// failure log's KindAny.
	OutcomeUnhappy Outcome = "unhappy"
	// OutcomeFailed means the same as KindFailed.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused means the same as KindRefused.
	OutcomeRefused Outcome = "refused"
	// OutcomeInterrupted means the same as KindInterrupted.
	OutcomeInterrupted Outcome = "interrupted"
)

// outcomeClauses maps an outcome to its condition, which keeps the caller's
// string out of the SQL text. The unhappy ones reuse the failure log's
// clauses, so the two screens count the same way.
var outcomeClauses = map[Outcome]string{
	OutcomeAny:         "TRUE",
	OutcomeOK:          "status < 400 AND error IS NULL",
	OutcomeUnhappy:     kindClauses[KindAny],
	OutcomeFailed:      kindClauses[KindFailed],
	OutcomeRefused:     kindClauses[KindRefused],
	OutcomeInterrupted: kindClauses[KindInterrupted],
}

// outcomeClause is the condition for o, or OutcomeAny's for an outcome it does
// not know.
func outcomeClause(o Outcome) string {
	if clause, ok := outcomeClauses[o]; ok {
		return clause
	}
	return outcomeClauses[OutcomeAny]
}

// Request is one recorded inference request with what an entity's screen shows
// in a row: when, which model, whose key, what it cost and what happened. It
// covers both served and failed requests, because the log is one table.
type Request struct {
	ID     int64     `json:"id"`
	TS     time.Time `json:"ts"`
	Alias  string    `json:"alias,omitempty"`
	Status int       `json:"status"`
	Error  string    `json:"error,omitempty"`
	KeyID  string    `json:"key_id,omitempty"`
	TeamID string    `json:"team_id,omitempty"`
	UserID string    `json:"user_id,omitempty"`
	OrgID  string    `json:"org_id,omitempty"`
	// Client is what sent the request, as it named itself.
	Client      string `json:"client,omitempty"`
	InputTokens int64  `json:"input_tokens"`
	// CachedInputTokens is the part of InputTokens the provider served from its
	// prompt cache at a lower price. Without it the cost cannot be recomputed
	// from the counts.
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CostMicros        int64 `json:"cost_micros"`
	LatencyMS         int64 `json:"latency_ms"`
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
	COALESCE(key_id, ''), COALESCE(team_id, ''), COALESCE(user_id, ''), org_id,
	COALESCE(client, ''), input_tokens, cached_input_tokens, output_tokens, cost_micros,
	latency_ms, ttft_ms, stream, estimated, canceled, COALESCE(session_key, ''),
	spans`

func scanRequest(r row) (Request, error) {
	var q Request
	err := r.Scan(&q.ID, &q.TS, &q.Alias, &q.Status, &q.Error,
		&q.KeyID, &q.TeamID, &q.UserID, &q.OrgID, &q.Client,
		&q.InputTokens, &q.CachedInputTokens, &q.OutputTokens, &q.CostMicros,
		&q.LatencyMS, &q.TTFTMS, &q.Stream, &q.Estimated, &q.Canceled, &q.SessionKey,
		&q.Spans)
	return q, err
}

// RequestQuery narrows the request log. Every field is optional; the zero value
// is "the most recent requests of this tenant".
type RequestQuery struct {
	OrgID string
	Scope
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
	Limit int
}

// Requests returns the matching rows, newest first.
//
// An empty OrgID means every tenant, which only an operator ever asks for.
func (s *Store) Requests(ctx context.Context, q RequestQuery) ([]Request, error) {
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+requestColumns+`
		FROM usage_events
		WHERE `+outcomeClause(q.Outcome)+`
		  AND ($1 = '' OR org_id = $1)
		  AND ($2 = 0 OR status = $2)
		  AND ($3::timestamptz IS NULL OR ts >= $3)
		  AND ($4::timestamptz IS NULL OR ts < $4)
		  AND ($5 = 0 OR id < $5)
		  AND ($6 = 0 OR status / 100 = $6)
		  AND ($7 = 0 OR id > $7)`+q.narrow("", 7)+`
		ORDER BY id DESC LIMIT $12`,
		append([]any{q.OrgID, q.Status, nullableTime(q.From), nullableTime(q.To), q.Before,
			q.StatusClass, q.After}, append(q.args(), q.Limit)...)...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRequest)
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
		count(*) FILTER (WHERE `+outcomeClauses[OutcomeOK]+`),
		count(*) FILTER (WHERE `+outcomeClauses[OutcomeFailed]+`),
		count(*) FILTER (WHERE `+outcomeClauses[OutcomeRefused]+`),
		count(*) FILTER (WHERE `+outcomeClauses[OutcomeInterrupted]+`)
		FROM usage_events
		WHERE ($1 = '' OR org_id = $1)
		  AND ($2::timestamptz IS NULL OR ts >= $2)
		  AND ($3::timestamptz IS NULL OR ts < $3)
		  AND ($4 = 0 OR status = $4)
		  AND ($5 = 0 OR status / 100 = $5)
		  AND ($6 = 0 OR id < $6)
		  AND ($7 = 0 OR id > $7)`+q.narrow("", 7),
		append([]any{q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Status,
			q.StatusClass, q.Before, q.After}, q.args()...)...,
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

// RequestFacets is what the request log can be narrowed by, over the whole
// window: the models, keys, teams, people and statuses that occur, each with
// its count. They are read from the log rather than listed from the roster,
// so only values that made requests are offered.
//
// There is no total: the outcome counts already carry one.
type RequestFacets struct {
	Models   []FacetCount `json:"models"`
	Keys     []FacetCount `json:"keys"`
	Teams    []FacetCount `json:"teams"`
	Users    []FacetCount `json:"users"`
	Statuses []FacetCount `json:"statuses"`
}

// RequestFilters counts the window by model, key, team, person and status.
//
// Like FailureFilters, only the tenant, the outcome and the time bounds of q
// are read. So a model picked after a team can match nothing, and the screen
// says so.
func (s *Store) RequestFilters(ctx context.Context, q RequestQuery) (RequestFacets, error) {
	f := RequestFacets{
		Models:   []FacetCount{},
		Keys:     []FacetCount{},
		Teams:    []FacetCount{},
		Users:    []FacetCount{},
		Statuses: []FacetCount{},
	}
	rows, err := s.pool.Query(ctx, `
		WITH matching AS (
		    SELECT alias, COALESCE(key_id, '') AS key_id,
		           COALESCE(team_id, '') AS team_id, COALESCE(user_id, '') AS user_id,
		           status
		    FROM usage_events
		    WHERE `+outcomeClause(q.Outcome)+`
		      AND ($1 = '' OR org_id = $1)
		      AND ($2::timestamptz IS NULL OR ts >= $2)
		      AND ($3::timestamptz IS NULL OR ts < $3)
		)
		SELECT 'model' AS facet, alias AS value, count(*) FROM matching
		    WHERE alias <> '' GROUP BY alias
		UNION ALL
		SELECT 'key', key_id, count(*) FROM matching WHERE key_id <> '' GROUP BY key_id
		UNION ALL
		SELECT 'team', team_id, count(*) FROM matching WHERE team_id <> '' GROUP BY team_id
		UNION ALL
		SELECT 'user', user_id, count(*) FROM matching WHERE user_id <> '' GROUP BY user_id
		UNION ALL
		SELECT 'status', status::text, count(*) FROM matching GROUP BY status
		ORDER BY facet, 3 DESC, value`,
		q.OrgID, nullableTime(q.From), nullableTime(q.To))
	if err != nil {
		return f, err
	}
	err = readFacets(rows, func(facet string, c FacetCount) {
		switch facet {
		case "model":
			f.Models = append(f.Models, c)
		case "key":
			f.Keys = append(f.Keys, c)
		case "team":
			f.Teams = append(f.Teams, c)
		case "user":
			f.Users = append(f.Users, c)
		case "status":
			f.Statuses = append(f.Statuses, c)
		}
	})
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
