package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The failure log: the usage rows that did not deliver, one at a time, with
// the message each client was given. The dashboard only counts them.

// FailureKind is which sort of unhappy request a report asks for. They are kept
// apart because each needs a different response.
type FailureKind string

const (
	// KindAny is every row with a status of 400 or more, or a message.
	KindAny FailureKind = ""
	// KindFailed is a request the inference plane could not answer. It is the
	// same set the dashboard counts as failed.
	KindFailed FailureKind = "failed"
	// KindRefused is a request a guardrail stopped: a spent budget, a rate
	// limit, a model this key may not use. It cost nothing and is working as
	// configured.
	KindRefused FailureKind = "refused"
	// KindInterrupted is a request that was answered and then did not finish,
	// such as a stream that ended early. Its status is the 200 the client got;
	// the message says what went wrong afterwards.
	KindInterrupted FailureKind = "interrupted"
)

// kindClauses maps a kind to its condition, which keeps the caller's string out
// of the SQL text.
var kindClauses = map[FailureKind]string{
	KindAny:         "(status >= 400 OR error IS NOT NULL)",
	KindFailed:      "status >= 500",
	KindRefused:     "status BETWEEN 400 AND 499",
	KindInterrupted: "status < 400 AND error IS NOT NULL",
}

// kindClause is the condition for k, or KindAny's for a kind it does not know.
func kindClause(k FailureKind) string {
	if clause, ok := kindClauses[k]; ok {
		return clause
	}
	return kindClauses[KindAny]
}

// Failure is one request that did not deliver, with what is needed to act on
// it: model, key, time, how long it took to fail, and what the client was told.
type Failure struct {
	ID        int64     `json:"id"`
	TS        time.Time `json:"ts"`
	Alias     string    `json:"alias,omitempty"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	KeyID     string    `json:"key_id,omitempty"`
	TeamID    string    `json:"team_id,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	OrgID     string    `json:"org_id,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	Stream    bool      `json:"stream"`
	Canceled  bool      `json:"canceled"`
}

// FailureQuery narrows the failure log. Every field is optional; the zero value
// is "the most recent unhappy requests of this tenant".
type FailureQuery struct {
	OrgID  string
	Alias  string
	KeyID  string
	TeamID string
	Kind   FailureKind
	// Status matches one exact status.
	Status int
	From   time.Time
	To     time.Time
	// Before pages backwards by id, like the audit log. An offset would shift
	// while the log is being written to.
	Before int64
	Limit  int
}

// Failures returns the matching rows, newest first.
//
// An empty OrgID means every tenant, which only an operator ever asks for.
func (s *Store) Failures(ctx context.Context, q FailureQuery) ([]Failure, error) {
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, alias, status, COALESCE(error, ''),
		COALESCE(key_id, ''), COALESCE(team_id, ''), COALESCE(user_id, ''), org_id,
		latency_ms, stream, canceled
		FROM usage_events
		WHERE `+kindClause(q.Kind)+`
		  AND ($1 = '' OR org_id = $1)
		  AND ($2 = '' OR alias = $2)
		  AND ($3 = '' OR key_id = $3)
		  AND ($4 = '' OR team_id = $4)
		  AND ($5 = 0 OR status = $5)
		  AND ($6::timestamptz IS NULL OR ts >= $6)
		  AND ($7::timestamptz IS NULL OR ts < $7)
		  AND ($8 = 0 OR id < $8)
		ORDER BY id DESC LIMIT $9`,
		q.OrgID, q.Alias, q.KeyID, q.TeamID, q.Status,
		nullableTime(q.From), nullableTime(q.To), q.Before, q.Limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (Failure, error) {
		var f Failure
		err := r.Scan(&f.ID, &f.TS, &f.Alias, &f.Status, &f.Error,
			&f.KeyID, &f.TeamID, &f.UserID, &f.OrgID,
			&f.LatencyMS, &f.Stream, &f.Canceled)
		return f, err
	})
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

// FailureFacets is what the failure log can be narrowed by, over the whole
// window: the models, keys and statuses that occur, each with its count. They
// are read from the log, so no filter is offered that matches nothing.
type FailureFacets struct {
	Models   []FacetCount `json:"models"`
	Keys     []FacetCount `json:"keys"`
	Statuses []FacetCount `json:"statuses"`
	// Total is how many rows match the window and the kind, before any model,
	// key or status narrowing.
	Total int64 `json:"total"`
}

// FailureFilters counts the window by model, by key and by status. Only OrgID,
// Kind and the time bounds of q are read, so the screen still offers the other
// values after one is picked.
func (s *Store) FailureFilters(ctx context.Context, q FailureQuery) (FailureFacets, error) {
	f := FailureFacets{
		Models:   []FacetCount{},
		Keys:     []FacetCount{},
		Statuses: []FacetCount{},
	}
	rows, err := s.pool.Query(ctx, `
		WITH matching AS (
		    SELECT alias, COALESCE(key_id, '') AS key_id, status FROM usage_events
		    WHERE `+kindClause(q.Kind)+`
		      AND ($1 = '' OR org_id = $1)
		      AND ($2::timestamptz IS NULL OR ts >= $2)
		      AND ($3::timestamptz IS NULL OR ts < $3)
		)
		SELECT 'model' AS facet, alias AS value, count(*) FROM matching
		    WHERE alias <> '' GROUP BY alias
		UNION ALL
		SELECT 'key', key_id, count(*) FROM matching WHERE key_id <> '' GROUP BY key_id
		UNION ALL
		SELECT 'status', status::text, count(*) FROM matching GROUP BY status
		UNION ALL
		SELECT 'total', '', count(*) FROM matching
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
		case "status":
			f.Statuses = append(f.Statuses, c)
		case "total":
			f.Total = c.Count
		}
	})
	return f, err
}
