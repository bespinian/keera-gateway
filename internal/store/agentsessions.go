package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"time"
)

// The session log reads the usage table as tasks rather than calls. A session
// is a run of one conversation's requests (same session_key) with no long idle
// gap.
//
// Nothing is stored per session. Runs are cut at read time, which keeps writes
// to a single insert, lets the gap change later, and works across replicas.

// DefaultSessionGap is how long a conversation may go quiet before its next
// request starts a new session. Shorter splits interrupted tasks; longer joins
// separate ones.
const DefaultSessionGap = 30 * time.Minute

// A session key is a hash with a one-character prefix saying where it came
// from. It is a prefix, not a column, to keep the largest table small.
const (
	statedPrefix  = "c" // the client named it
	derivedPrefix = "p" // derived from the conversation's opening prompt
)

// statedSessionKey builds the key for a session the client named.
func statedSessionKey(hash string) string { return statedPrefix + hash }

// DerivedSessionKey builds the key for a session derived from the opening
// prompt.
func DerivedSessionKey(hash string) string { return derivedPrefix + hash }

// MaxSessionInput bounds how much of a payload is hashed into a session key,
// so a huge field does not make hashing slow. Conversations that only differ
// later are still told apart by the idle gap.
const MaxSessionInput = 128 << 10

// SessionHash derives a session hash. The gateway and sandboxes (through
// StatedSessionKeyFor) share it, so the two cannot drift apart.
//
// The kind is mixed in so a stated id and an opening prompt with the same bytes
// still differ. The zero byte separator appears in no id or JSON document.
// 12 bytes of SHA-256 avoid collisions within a tenant and stay short.
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

// The session kinds SessionHash mixes in.
const (
	SessionKindStated  = "stated"
	SessionKindOpening = "opening"
)

// StatedSessionKeyFor is the session key on the usage rows of requests made
// with keyID whose session header names stated.
func StatedSessionKeyFor(keyID, stated string) string {
	return statedSessionKey(SessionHash(keyID, SessionKindStated, []byte(stated)))
}

// StatedSession reports whether the client named this session itself.
func StatedSession(key string) bool { return strings.HasPrefix(key, statedPrefix) }

// AgentSession is one run of requests that carried out one task. Session
// already names a browser sign-in.
type AgentSession struct {
	// ID is the id of the session's first request, since sessions have no
	// stored id.
	ID int64 `json:"id"`
	// SessionKey is the conversation, as the gateway hashed it.
	SessionKey string `json:"session_key"`
	// Stated is true when the client named the session rather than it being
	// inferred, so the grouping can be trusted more.
	Stated bool `json:"stated"`

	StartedAt time.Time `json:"started_at"`
	// EndedAt is when the last request finished, not when it started.
	EndedAt time.Time `json:"ended_at"`

	Requests    int64 `json:"requests"`
	OK          int64 `json:"ok"`
	Failed      int64 `json:"failed"`
	Refused     int64 `json:"refused"`
	Interrupted int64 `json:"interrupted"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	TTFTMedianMS int64 `json:"ttft_median_ms"`

	Models []string `json:"models"`

	ProjectID string `json:"project_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	KeyID     string `json:"key_id,omitempty"`

	// LastStatus and LastError are the final request's status and error.
	LastStatus int    `json:"last_status"`
	LastError  string `json:"last_error,omitempty"`
}

func (a AgentSession) Duration() time.Duration { return a.EndedAt.Sub(a.StartedAt) }

// Sort is the order the request log or session list is read in. A sort a list
// does not offer reads as SortRecent.
type Sort string

const (
	// SortRecent is newest first.
	SortRecent Sort = ""
	// SortCost is the most expensive first.
	SortCost Sort = "cost"
	// SortTokens is the most tokens first, read and written together.
	SortTokens Sort = "tokens"
	// SortRequests is the most requests first. Sessions only.
	SortRequests Sort = "requests"
	// SortDuration is the longest-running first. Sessions only.
	SortDuration Sort = "duration"
	// SortFirstToken is the slowest first token first. Requests only.
	SortFirstToken Sort = "ttft"
	// sortOldest lets AgentSessionAt find one session with the same query that
	// lists them all.
	sortOldest Sort = "oldest"
)

// sessionOrder maps a sort to its ORDER BY, keeping the caller's string out of
// the SQL.
var sessionOrder = map[Sort]string{
	SortRecent:   "started_at DESC, id DESC",
	sortOldest:   "started_at ASC, id ASC",
	SortCost:     "cost_micros DESC, started_at DESC",
	SortTokens:   "input_tokens + output_tokens DESC, started_at DESC",
	SortRequests: "requests DESC, started_at DESC",
	SortDuration: "(ended_at - started_at) DESC, started_at DESC",
}

func SessionSorts() []Sort {
	return []Sort{SortRecent, SortCost, SortTokens, SortRequests, SortDuration}
}

// AgentSessionQuery narrows the session report.
type AgentSessionQuery struct {
	OrgID string
	// ProjectID, UserID and KeyID apply before runs are cut, which drops whole
	// sessions. Alias applies after, so a session using two models is not split.
	ReportScope
	SessionKey string
	// Holder keeps only one person's sessions, for a member reading the list.
	// Unlike UserID, it also narrows the filter choices.
	Holder string
	// Outcome keeps sessions with at least one request of that outcome.
	// OutcomeOK keeps those where every request was served.
	Outcome Outcome
	// Status and StatusClass match the status of the last request, exactly or
	// by its first digit.
	Status      int
	StatusClass int

	From time.Time
	To   time.Time
	// Gap is the idle threshold. The caller always sets it.
	Gap  time.Duration
	Sort Sort
	// Before pages backwards on the id of the session's first request. It only
	// works under SortRecent, where id order and time order match.
	Before int64
	Limit  int
}

func (q *AgentSessionQuery) setDefaults() {
	q.Limit = pageLimit(q.Limit, 100, 5000)
	if _, ok := sessionOrder[q.Sort]; !ok {
		q.Sort = SortRecent
	}
}

// sessionCTE cuts the usage rows into sessions. `ev` adds each row's idle gap,
// `runs` numbers the sessions by counting gaps, and `grouped` makes one row per
// session.
//
// The window starts one gap early, and sessions that ended in that margin are
// dropped in the HAVING, so a task already running when the window opened is
// not cut short. A longer task is still cut; AgentSessionAt walks back to its
// real start.
//
// Rows are ordered by (ts, id), so ties always cut the same way.
//
// It takes $1..$9: org, from, to, gap seconds, key, project, user, key id and
// alias. matched takes $10 and $11. Later parameters start at $12.
var sessionCTE = `
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
	      AND ($6 = '' OR project_id = $6)
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
	           ` + countOutcome(OutcomeOK) + ` AS ok,
	           ` + countOutcome(OutcomeFailed) + ` AS failed,
	           ` + countOutcome(OutcomeRefused) + ` AS refused,
	           ` + countOutcome(OutcomeInterrupted) + ` AS interrupted,
	           COALESCE(sum(input_tokens), 0) AS input_tokens,
	           COALESCE(sum(output_tokens), 0) AS output_tokens,
	           COALESCE(sum(cost_micros), 0) AS cost_micros,
	           COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms)
	                          FILTER (WHERE ttft_ms > 0)), 0)::bigint AS ttft_median_ms,
	           COALESCE(array_agg(DISTINCT alias) FILTER (WHERE alias <> ''),
	                    '{}'::text[]) AS models,
	           COALESCE(max(project_id), '') AS project_id,
	           COALESCE(max(user_id), '') AS user_id,
	           COALESCE(max(key_id), '') AS key_id,
	           (array_agg(status ORDER BY ts DESC, id DESC))[1] AS last_status,
	           (array_agg(COALESCE(error, '') ORDER BY ts DESC, id DESC))[1] AS last_error
	    FROM runs
	    GROUP BY session_key, run
	    HAVING ($2::timestamptz IS NULL OR max(ts) >= $2)
	       AND bool_or($9 = '' OR alias = $9)
	)`

// sessionOutcomes maps an outcome to its condition on a session. A session
// matches an outcome when any of its requests has it, and is OK when none
// has another.
var sessionOutcomes = map[Outcome]string{
	OutcomeAny:         "TRUE",
	OutcomeOK:          "failed + refused + interrupted = 0",
	OutcomeUnhappy:     "failed + refused + interrupted > 0",
	OutcomeFailed:      "failed > 0",
	OutcomeRefused:     "refused > 0",
	OutcomeInterrupted: "interrupted > 0",
}

// matched is the sessions of q's outcome and status, as one more CTE after
// sessionCTE.
func (q AgentSessionQuery) matched() string {
	cond, ok := sessionOutcomes[q.Outcome]
	if !ok {
		cond = sessionOutcomes[OutcomeAny]
	}
	return `,
	matched AS (SELECT * FROM grouped WHERE ` + cond + `
	    AND ($10 = 0 OR last_status = $10)
	    AND ($11 = 0 OR last_status / 100 = $11))`
}

// args returns the values sessionCTE and matched read, in their order. Holder
// replaces UserID: a member who names anybody else is refused earlier.
func (q AgentSessionQuery) args() []any {
	user := q.UserID
	if q.Holder != "" {
		user = q.Holder
	}
	return []any{
		q.OrgID, nullableTime(q.From), nullableTime(q.To), q.Gap.Seconds(),
		q.SessionKey, q.ProjectID, user, q.KeyID, q.Alias, q.Status, q.StatusClass,
	}
}

func scanAgentSession(r row) (AgentSession, error) {
	var a AgentSession
	if err := r.Scan(&a.SessionKey, &a.ID, &a.StartedAt, &a.EndedAt, &a.Requests,
		&a.OK, &a.Failed, &a.Refused, &a.Interrupted,
		&a.InputTokens, &a.OutputTokens, &a.CostMicros, &a.TTFTMedianMS,
		&a.Models, &a.ProjectID, &a.UserID, &a.KeyID,
		&a.LastStatus, &a.LastError); err != nil {
		return AgentSession{}, err
	}
	a.Stated = StatedSession(a.SessionKey)
	return a, nil
}

// AgentSessions groups the event log into sessions and returns them in the
// order q asks for.
func (s *Store) AgentSessions(ctx context.Context, q AgentSessionQuery) ([]AgentSession, error) {
	q.setDefaults()
	return queryAll(ctx, s.pool, scanAgentSession, sessionCTE+q.matched()+`
		SELECT session_key, id, started_at, ended_at, requests, ok, failed, refused,
		       interrupted, input_tokens, output_tokens, cost_micros,
		       ttft_median_ms, models, project_id, user_id, key_id, last_status, last_error
		FROM matched
		WHERE ($12 = 0 OR id < $12)
		ORDER BY `+sessionOrder[q.Sort]+`
		LIMIT $13`,
		append(q.args(), q.Before, q.Limit)...)
}

// AgentSessionTotals adds up a whole window of sessions, not just one page.
type AgentSessionTotals struct {
	Sessions int64 `json:"sessions"`
	// Unhappy is how many of them hold something that did not deliver.
	Unhappy      int64 `json:"unhappy"`
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	// Medians, not means, so one runaway session does not skew them.
	RequestsMedian   int64 `json:"requests_median"`
	DurationMedianMS int64 `json:"duration_median_ms"`
	CostMedianMicros int64 `json:"cost_median_micros"`
	// LongestRequests and CostliestMicros show the worst session.
	LongestRequests int64 `json:"longest_requests"`
	CostliestMicros int64 `json:"costliest_micros"`
	// Outcomes counts sessions by every outcome, so the screen can show each
	// count. The other totals cover only the outcome asked for.
	Outcomes RequestOutcomes `json:"outcomes"`
}

// AgentSessionSummary counts the sessions in one window. It ignores q's paging
// and sort, so the totals do not change while scrolling.
func (s *Store) AgentSessionSummary(ctx context.Context,
	q AgentSessionQuery,
) (AgentSessionTotals, error) {
	q.setDefaults()
	var t AgentSessionTotals
	o := &t.Outcomes
	err := s.pool.QueryRow(ctx, sessionCTE+q.matched()+`
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
		       COALESCE(max(cost_micros), 0),
		       (SELECT count(*) FROM grouped),
		       (SELECT count(*) FILTER (WHERE `+sessionOutcomes[OutcomeOK]+`) FROM grouped),
		       (SELECT count(*) FILTER (WHERE `+sessionOutcomes[OutcomeFailed]+`) FROM grouped),
		       (SELECT count(*) FILTER (WHERE `+sessionOutcomes[OutcomeRefused]+`) FROM grouped),
		       (SELECT count(*) FILTER (WHERE `+sessionOutcomes[OutcomeInterrupted]+`) FROM grouped)
		FROM matched`, q.args()...).Scan(&t.Sessions, &t.Unhappy, &t.Requests,
		&t.InputTokens, &t.OutputTokens, &t.CostMicros,
		&t.RequestsMedian, &t.DurationMedianMS, &t.CostMedianMicros,
		&t.LongestRequests, &t.CostliestMicros,
		&o.Total, &o.OK, &o.Failed, &o.Refused, &o.Interrupted)
	return t, err
}

// AgentSessionFilters counts the sessions in the window by model, key, project,
// person and last status, like RequestFilters. A session that used two models
// counts under both.
//
// It reads only q's tenant, holder, outcome and time bounds, so the screen
// still offers other values after one is picked.
func (s *Store) AgentSessionFilters(ctx context.Context,
	q AgentSessionQuery,
) (RequestFacets, error) {
	q.setDefaults()
	q.ReportScope = ReportScope{}
	q.Status, q.StatusClass = 0, 0
	return s.facets(ctx, sessionCTE+q.matched()+`
		SELECT 'model' AS facet, m AS value, count(*) FROM matched, unnest(models) AS m
		    GROUP BY m`+holderFacets("matched")+`
		UNION ALL
		SELECT 'status', last_status::text, count(*) FROM matched GROUP BY last_status
		ORDER BY facet, 3 DESC, value`, q.args()...)
}

// AgentSessionAt returns the session holding a request, with all its requests.
// It uses the same query as the list, so both always match.
func (s *Store) AgentSessionAt(ctx context.Context, orgID string, requestID int64,
	gap time.Duration,
) (AgentSession, []Request, error) {
	// A request of another tenant is not found, as with every scoped read.
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

	// Starting at its own start, the first session in ascending order is this one.
	sessions, err := s.AgentSessions(ctx, AgentSessionQuery{
		OrgID: owner, SessionKey: key, From: start, Gap: gap, Sort: sortOldest, Limit: 1,
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
// gap at or before it. It scans one small index range.
func (s *Store) sessionStart(ctx context.Context, orgID, key string, ts time.Time,
	gap time.Duration,
) (time.Time, error) {
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
// oldest first. Rows before start are excluded, so the session is run 1.
func (s *Store) agentSessionRequests(ctx context.Context, orgID, key string,
	start time.Time, gap time.Duration,
) ([]Request, error) {
	return queryAll(ctx, s.pool, scanRequest, `
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
}
