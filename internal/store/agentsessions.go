package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"time"
)

// The session log: the usage table read as tasks rather than as calls.
//
// A coding agent makes many calls to carry out one task. A session is a run of
// one conversation's requests with no long idle gap: the conversation is the
// session_key on each row, and the gap separates one task from the next.
//
// Nothing is stored per session. Runs are cut at read time, which keeps the
// write path a single insert, lets the gap change after the fact, and works
// across replicas, since every gateway derives the same key.

// DefaultSessionGap is how long a conversation may go quiet before the next
// request on it starts a new session. Much shorter splits a task whenever
// somebody is interrupted; much longer joins separate tasks.
const DefaultSessionGap = 30 * time.Minute

// A session key is a hash with a one-character prefix saying where it came
// from: named by the client, or derived from the opening prompt. It is a
// prefix, not a column, because it never changes within a session and the
// table gains a row per request.
const (
	statedPrefix  = "c" // the client named it
	derivedPrefix = "p" // derived from the conversation's opening prompt
)

// StatedSessionKey builds the key for a session the client named, from a hash
// already computed.
func StatedSessionKey(hash string) string { return statedPrefix + hash }

// DerivedSessionKey builds the key for a session derived from the opening
// prompt, from a hash already computed.
func DerivedSessionKey(hash string) string { return derivedPrefix + hash }

// MaxSessionInput bounds how much of a payload is hashed into a session key,
// so the cost per request stays predictable when a client sends a huge field.
// Two conversations that match for 128 KiB and then differ are told apart by
// the idle gap anyway.
const MaxSessionInput = 128 << 10

// SessionHash derives a session hash. It lives here because two callers need
// it: the gateway for each request, and a sandbox for the session id it will
// send. Two copies would drift apart, and the sessions would no longer match.
//
// The kind is mixed in so a stated id and an opening prompt with the same bytes
// still differ. The zero separator is a byte no id or JSON document contains.
//
// It keeps 12 bytes of SHA-256 (16 characters encoded): enough to avoid
// collisions within one tenant, and short on the largest table.
func SessionHash(keyID, kind string, payload []byte) string {
	if len(payload) > MaxSessionInput {
		payload = payload[:MaxSessionInput]
	}
	sum := sha256.New()
	sum.Write([]byte(keyID))
	sum.Write([]byte{0})
	sum.Write([]byte(kind))
	sum.Write([]byte{0})
	sum.Write(payload)
	return base64.RawURLEncoding.EncodeToString(sum.Sum(nil)[:12])
}

// SessionKindStated is the kind mixed in for a session the client named. It is
// exported so that the two callers cannot spell it differently.
const SessionKindStated = "stated"

// StatedSessionKeyFor is the key that appears on the usage rows of every
// request made with keyID that names session stated in a session header.
func StatedSessionKeyFor(keyID, stated string) string {
	return StatedSessionKey(SessionHash(keyID, SessionKindStated, []byte(stated)))
}

// StatedSession reports whether the client named this session itself.
func StatedSession(key string) bool { return strings.HasPrefix(key, statedPrefix) }

// AgentSession is one run of requests that carried out one task. It is not
// called Session because that name is already a browser sign-in.
type AgentSession struct {
	// ID is the id of the session's first request. Sessions have no stored id,
	// so this is how one is addressed, and a link to it stays stable.
	ID int64 `json:"id"`
	// Key is the conversation, as the gateway hashed it. The same key days
	// later is the same conversation resumed or its opening prompt sent again.
	Key string `json:"key"`
	// Stated is true when the client named the session itself, rather than it
	// being inferred. It tells a reader how far to trust the grouping.
	Stated bool `json:"stated"`

	StartedAt time.Time `json:"started_at"`
	// EndedAt is when the last request finished, not when it started.
	EndedAt time.Time `json:"ended_at"`

	Requests int64 `json:"requests"`
	// The four outcomes, counted as everywhere else in this log.
	OK          int64 `json:"ok"`
	Failed      int64 `json:"failed"`
	Refused     int64 `json:"refused"`
	Interrupted int64 `json:"interrupted"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	TTFTMedianMS int64 `json:"ttft_median_ms"`

	// Models is every alias the session used.
	Models []string `json:"models"`

	TeamID string `json:"team_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
	KeyID  string `json:"key_id,omitempty"`

	// LastStatus and LastError are how the session ended: the final request's
	// status and, if any, the error its client got. Showing them in the list
	// saves opening each session to find the one that failed.
	LastStatus int    `json:"last_status"`
	LastError  string `json:"last_error,omitempty"`
}

// Duration is how long the session ran.
func (a AgentSession) Duration() time.Duration { return a.EndedAt.Sub(a.StartedAt) }

// Unhappy reports whether anything in the session did not deliver.
func (a AgentSession) Unhappy() bool { return a.Failed+a.Refused+a.Interrupted > 0 }

// AgentSessionSort is the order a list of sessions is read in.
type AgentSessionSort string

// The rankings put the most expensive, the longest-running and the slowest
// tasks first, where a list by time would bury them.
const (
	// SortRecent is newest first, which is what a screen opens on.
	SortRecent AgentSessionSort = ""
	// SortCost is the most expensive first.
	SortCost AgentSessionSort = "cost"
	// SortRequests is the most requests first.
	SortRequests AgentSessionSort = "requests"
	// SortDuration is the longest-running first.
	SortDuration AgentSessionSort = "duration"
	// sortOldest is internal. AgentSessionAt uses it to find one session by its
	// start with the same query that lists them all.
	sortOldest AgentSessionSort = "oldest"
)

// sessionOrder maps a sort to its ORDER BY, which keeps the caller's string out
// of the SQL text.
var sessionOrder = map[AgentSessionSort]string{
	SortRecent:   "started_at DESC, id DESC",
	sortOldest:   "started_at ASC, id ASC",
	SortCost:     "cost_micros DESC, started_at DESC",
	SortRequests: "requests DESC, started_at DESC",
	SortDuration: "(ended_at - started_at) DESC, started_at DESC",
}

// AgentSessionQuery narrows the session report. The zero value is "this
// tenant's most recent sessions".
type AgentSessionQuery struct {
	OrgID string
	// TeamID, UserID and KeyID narrow to one entity. They apply before the
	// runs are cut: a session belongs to one key, so this drops whole sessions
	// and never splits one.
	TeamID string
	UserID string
	KeyID  string
	// Alias keeps the sessions that used one model, and applies after the runs
	// are cut. Applied to rows, it would split a session that used two models
	// into pieces.
	Alias string
	// Key narrows to one conversation.
	Key string
	// Unhappy keeps only the sessions with a failure, a refusal or an
	// interrupted answer.
	Unhappy bool

	From time.Time
	To   time.Time
	// Gap is the idle threshold. Zero means DefaultSessionGap.
	Gap  time.Duration
	Sort AgentSessionSort
	// Before pages backwards on the id of the session's first request. It only
	// works as a cursor under SortRecent, where id order and time order match;
	// under a ranking it would skip rows.
	Before int64
	Limit  int
}

func (q *AgentSessionQuery) setDefaults() {
	if q.Gap <= 0 {
		q.Gap = DefaultSessionGap
	}
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 100
	}
	if _, ok := sessionOrder[q.Sort]; !ok {
		q.Sort = SortRecent
	}
}

// sessionCTE cuts the usage rows into sessions. The list, the totals and a
// single session all start here.
//
// `ev` is the rows in scope, each with how long its conversation was quiet
// before it. `runs` numbers the sessions by counting the gaps so far.
// `grouped` turns each run into the row a reader sees.
//
// The window starts one gap early, and sessions that ended inside that margin
// are dropped in the HAVING. Without it, a task already running when the
// window opened would show only its last part.
//
// Rows are ordered by (ts, id), so ties always cut the same way. `SELECT *` in
// `ev` avoids a second column list to keep in step with requestColumns.
//
// It takes $1..$10: org, from, to, gap seconds, key, team, user, key id, alias,
// unhappy. What follows it supplies its own parameters from $11 on.
const sessionCTE = `
	WITH ev AS (
	    SELECT *, EXTRACT(EPOCH FROM (ts - lag(ts)
	               OVER (PARTITION BY session_key ORDER BY ts, id))) AS gap
	    FROM usage_events
	    WHERE session_key IS NOT NULL
	      AND ($1 = '' OR org_id = $1)
	      AND ($2::timestamptz IS NULL
	           OR ts >= $2::timestamptz - make_interval(secs => $4))
	      AND ($3::timestamptz IS NULL OR ts < $3)
	      AND ($5 = '' OR session_key = $5)
	      AND ($6 = '' OR team_id = $6)
	      AND ($7 = '' OR user_id = $7)
	      AND ($8 = '' OR key_id = $8)
	),
	runs AS (
	    SELECT *, count(*) FILTER (WHERE gap IS NULL OR gap > $4)
	                OVER (PARTITION BY session_key ORDER BY ts, id
	                      ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS run
	    FROM ev
	),
	grouped AS (
	    SELECT session_key,
	           (array_agg(id ORDER BY ts, id))[1] AS id,
	           min(ts) AS started_at,
	           max(ts + latency_ms * interval '1 millisecond') AS ended_at,
	           count(*) AS requests,
	           count(*) FILTER (WHERE status < 400 AND error IS NULL) AS ok,
	           count(*) FILTER (WHERE status >= 500) AS failed,
	           count(*) FILTER (WHERE status BETWEEN 400 AND 499) AS refused,
	           count(*) FILTER (WHERE status < 400 AND error IS NOT NULL) AS interrupted,
	           COALESCE(sum(input_tokens), 0) AS input_tokens,
	           COALESCE(sum(output_tokens), 0) AS output_tokens,
	           COALESCE(sum(cost_micros), 0) AS cost_micros,
	           COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms)
	                          FILTER (WHERE ttft_ms > 0)), 0)::bigint AS ttft_median_ms,
	           COALESCE(array_agg(DISTINCT alias) FILTER (WHERE alias <> ''),
	                    '{}'::text[]) AS models,
	           COALESCE(max(team_id), '') AS team_id,
	           COALESCE(max(user_id), '') AS user_id,
	           COALESCE(max(key_id), '') AS key_id,
	           (array_agg(status ORDER BY ts DESC, id DESC))[1] AS last_status,
	           (array_agg(COALESCE(error, '') ORDER BY ts DESC, id DESC))[1] AS last_error
	    FROM runs
	    GROUP BY session_key, run
	    HAVING ($2::timestamptz IS NULL OR max(ts) >= $2)
	       AND bool_or($9 = '' OR alias = $9)
	       AND (NOT $10 OR bool_or(status >= 400 OR error IS NOT NULL))
	)`

// args returns the ten values sessionCTE reads, in the order it numbers them.
func (q AgentSessionQuery) args() []any {
	return []any{q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Gap.Seconds(),
		q.Key, q.TeamID, q.UserID, q.KeyID, q.Alias, q.Unhappy}
}

func scanAgentSession(r row) (AgentSession, error) {
	var a AgentSession
	if err := r.Scan(&a.Key, &a.ID, &a.StartedAt, &a.EndedAt, &a.Requests,
		&a.OK, &a.Failed, &a.Refused, &a.Interrupted,
		&a.InputTokens, &a.OutputTokens, &a.CostMicros, &a.TTFTMedianMS,
		&a.Models, &a.TeamID, &a.UserID, &a.KeyID,
		&a.LastStatus, &a.LastError); err != nil {
		return AgentSession{}, err
	}
	a.Stated = StatedSession(a.Key)
	return a, nil
}

// AgentSessions groups the event log into sessions and returns them in the
// order q asks for.
func (s *Store) AgentSessions(ctx context.Context, q AgentSessionQuery) ([]AgentSession, error) {
	q.setDefaults()
	rows, err := s.pool.Query(ctx, sessionCTE+`
		SELECT session_key, id, started_at, ended_at, requests, ok, failed, refused,
		       interrupted, input_tokens, output_tokens, cost_micros,
		       ttft_median_ms, models, team_id, user_id, key_id, last_status, last_error
		FROM grouped
		WHERE ($11 = 0 OR id < $11)
		ORDER BY `+sessionOrder[q.Sort]+`
		LIMIT $12`,
		append(q.args(), q.Before, q.Limit)...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAgentSession)
}

// AgentSessionTotals is what a window of sessions adds up to, over the whole
// window rather than the page being read.
type AgentSessionTotals struct {
	Sessions int64 `json:"sessions"`
	// Unhappy is how many of them hold something that did not deliver.
	Unhappy      int64 `json:"unhappy"`
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	// Medians, not means, because one runaway session would skew a mean. The
	// maximums below show how far the worst one went.
	MedianRequests   int64 `json:"median_requests"`
	MedianDurationMS int64 `json:"median_duration_ms"`
	MedianCostMicros int64 `json:"median_cost_micros"`
	// LongestRequests and CostliestMicros are the runaway: the session worth
	// opening.
	LongestRequests int64 `json:"longest_requests"`
	CostliestMicros int64 `json:"costliest_micros"`
}

// AgentSessionSummary counts the sessions in one window.
//
// It reads everything q narrows by except paging and sort, so the totals do
// not change as somebody scrolls. That is why it walks the rows again rather
// than adding up the page.
func (s *Store) AgentSessionSummary(ctx context.Context,
	q AgentSessionQuery) (AgentSessionTotals, error) {
	q.setDefaults()
	var t AgentSessionTotals
	err := s.pool.QueryRow(ctx, sessionCTE+`
		SELECT count(*),
		       count(*) FILTER (WHERE failed + refused + interrupted > 0),
		       COALESCE(sum(requests), 0),
		       COALESCE(sum(input_tokens), 0),
		       COALESCE(sum(output_tokens), 0),
		       COALESCE(sum(cost_micros), 0),
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY requests)), 0)::bigint,
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP
		           (ORDER BY EXTRACT(EPOCH FROM (ended_at - started_at)) * 1000)), 0)::bigint,
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY cost_micros)), 0)::bigint,
		       COALESCE(max(requests), 0),
		       COALESCE(max(cost_micros), 0)
		FROM grouped`, q.args()...).Scan(&t.Sessions, &t.Unhappy, &t.Requests,
		&t.InputTokens, &t.OutputTokens, &t.CostMicros,
		&t.MedianRequests, &t.MedianDurationMS, &t.MedianCostMicros,
		&t.LongestRequests, &t.CostliestMicros)
	return t, err
}

// AgentSessionAt is the session holding any one of its requests, together with
// all of its requests. A reader usually arrives from one row in the request or
// failure log.
//
// The summary comes from the same query that lists sessions, so a session on
// its own screen always matches its row in a list.
func (s *Store) AgentSessionAt(ctx context.Context, orgID string, requestID int64,
	gap time.Duration) (AgentSession, []Request, error) {
	if gap <= 0 {
		gap = DefaultSessionGap
	}

	// The organisation is read back rather than filtered on, so a request of
	// another tenant is simply not found, as with every other scoped read.
	var (
		key   string
		ts    time.Time
		owner string
	)
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(session_key, ''), ts, org_id
		FROM usage_events WHERE id = $1`, requestID).Scan(&key, &ts, &owner)
	switch {
	case err != nil:
		return AgentSession{}, nil, notFound(err)
	case orgID != "" && owner != orgID, key == "":
		// A request in no conversation is in no session.
		return AgentSession{}, nil, ErrNotFound
	}

	start, err := s.sessionStart(ctx, owner, key, ts, gap)
	if err != nil {
		return AgentSession{}, nil, notFound(err)
	}

	// Starting at the session's own start, the first session in ascending
	// order is this one.
	sessions, err := s.AgentSessions(ctx, AgentSessionQuery{
		OrgID: owner, Key: key, From: start, Gap: gap, Sort: sortOldest, Limit: 1,
	})
	if err != nil {
		return AgentSession{}, nil, err
	}
	if len(sessions) == 0 {
		return AgentSession{}, nil, ErrNotFound
	}

	rows, err := s.agentSessionRequests(ctx, owner, key, start, gap)
	if err != nil {
		return AgentSession{}, nil, err
	}
	return sessions[0], rows, nil
}

// sessionStart is when the session holding a request at ts began: the last
// gap at or before it. It scans one conversation's rows, which is one small
// index range.
func (s *Store) sessionStart(ctx context.Context, orgID, key string, ts time.Time,
	gap time.Duration) (time.Time, error) {
	var start time.Time
	err := s.pool.QueryRow(ctx, `
		WITH ev AS (
		    SELECT ts, EXTRACT(EPOCH FROM (ts - lag(ts) OVER (ORDER BY ts, id))) AS gap
		    FROM usage_events
		    WHERE org_id = $1 AND session_key = $2 AND ts <= $3
		)
		SELECT ts FROM ev WHERE gap IS NULL OR gap > $4 ORDER BY ts DESC LIMIT 1`,
		orgID, key, ts, gap.Seconds()).Scan(&start)
	return start, err
}

// agentSessionRequests reads the requests of the session beginning at start,
// oldest first, because a task is read from beginning to end.
//
// Every row before start is excluded, so the session to keep is run 1.
func (s *Store) agentSessionRequests(ctx context.Context, orgID, key string,
	start time.Time, gap time.Duration) ([]Request, error) {
	rows, err := s.pool.Query(ctx, `
		WITH ev AS (
		    SELECT *, EXTRACT(EPOCH FROM (ts - lag(ts) OVER (ORDER BY ts, id))) AS gap
		    FROM usage_events
		    WHERE org_id = $1 AND session_key = $2 AND ts >= $3
		),
		runs AS (
		    SELECT *, count(*) FILTER (WHERE gap IS NULL OR gap > $4)
		                OVER (ORDER BY ts, id
		                      ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS run
		    FROM ev
		)
		SELECT `+requestColumns+` FROM runs WHERE run = 1 ORDER BY ts, id`,
		orgID, key, start, gap.Seconds())
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRequest)
}
