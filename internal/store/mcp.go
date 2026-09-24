package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// ----------------------------------------------------------------- servers

const mcpColumns = `SELECT alias, url, description, auth_header, api_key_env, api_key_ct,
	enabled, managed`

func scanMCPServer(r row) (policy.MCPServer, error) {
	var m policy.MCPServer
	if err := r.Scan(&m.Alias, &m.URL, &m.Description, &m.AuthHeader, &m.APIKeyEnv,
		&m.APIKeyCiphertext, &m.Enabled, &m.Managed); err != nil {
		return policy.MCPServer{}, err
	}
	m.HasAPIKey = len(m.APIKeyCiphertext) > 0
	return m, nil
}

// LoadMCPServers reads the whole MCP catalogue.
func (s *Store) LoadMCPServers(ctx context.Context) ([]policy.MCPServer, error) {
	rows, err := s.pool.Query(ctx, mcpColumns+" FROM mcp_servers ORDER BY alias")
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMCPServer)
}

// MCPServer reads one entry, or ErrNotFound.
func (s *Store) MCPServer(ctx context.Context, alias string) (policy.MCPServer, error) {
	m, err := scanMCPServer(s.pool.QueryRow(ctx, mcpColumns+" FROM mcp_servers WHERE alias = $1", alias))
	if err != nil {
		return policy.MCPServer{}, notFound(err)
	}
	return m, nil
}

// UpsertMCPServer creates or replaces one entry. Like UpsertModel it leaves
// the stored credential alone.
func (s *Store) UpsertMCPServer(ctx context.Context, m policy.MCPServer) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO mcp_servers (alias, url, description, auth_header,
		api_key_env, enabled, managed, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())
		ON CONFLICT (alias) DO UPDATE SET url = EXCLUDED.url,
			description = EXCLUDED.description, auth_header = EXCLUDED.auth_header,
			api_key_env = EXCLUDED.api_key_env, enabled = EXCLUDED.enabled,
			managed = EXCLUDED.managed, updated_at = now()`,
		m.Alias, m.URL, m.Description, m.AuthHeader, m.APIKeyEnv, m.Enabled, m.Managed)
	return err
}

// UnmanageMCPServers hands back to the operator every file-managed server the
// catalogue file no longer names.
func (s *Store) UnmanageMCPServers(ctx context.Context, except []string) error {
	// A nil list would be sent as NULL, and nothing is unequal to all of NULL.
	if except == nil {
		except = []string{}
	}
	_, err := s.pool.Exec(ctx,
		"UPDATE mcp_servers SET managed = false, updated_at = now() WHERE managed AND alias <> ALL($1)",
		except)
	return err
}

// SetMCPCredential stores the sealed credential for one server. Nil clears it.
func (s *Store) SetMCPCredential(ctx context.Context, alias string, sealed []byte) error {
	return s.execOne(ctx,
		"UPDATE mcp_servers SET api_key_ct = $2, updated_at = now() WHERE alias = $1",
		alias, sealed)
}

// DeleteMCPServer removes one entry.
func (s *Store) DeleteMCPServer(ctx context.Context, alias string) error {
	return s.execOne(ctx, "DELETE FROM mcp_servers WHERE alias = $1", alias)
}

// -------------------------------------------------------------- tool calls

// ToolOutcome is what came of one tool call.
type ToolOutcome string

// The outcomes a tool call can have. See the tool_calls table.
const (
	ToolOK      ToolOutcome = "ok"
	ToolFailed  ToolOutcome = "tool_error"
	ToolError   ToolOutcome = "error"
	ToolDenied  ToolOutcome = "denied"
	ToolRefused ToolOutcome = "refused"
)

// ToolCall is the part of an event that makes it a tool call rather than an
// inference request. Such an event goes to tool_calls, with its filter runs,
// and its cost to the same budgets.
type ToolCall struct {
	Server      string
	Tool        string
	Outcome     ToolOutcome
	ArgBytes    int
	ResultBytes int
}

// queueToolCall adds one tool call's row and its filter runs to the batch.
func queueToolCall(batch *pgx.Batch, e Event) {
	t := e.Tool
	batch.Queue(`INSERT INTO tool_calls (ts, org_id, team_id, user_id, key_id, server, tool,
		outcome, latency_ms, arg_bytes, result_bytes, cost_micros, error, session_key, client)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		e.TS, e.OrgID, nullable(e.TeamID), nullable(e.UserID), nullable(e.KeyID),
		t.Server, t.Tool, string(t.Outcome), e.Latency.Milliseconds(), t.ArgBytes, t.ResultBytes,
		e.CostMicros, nullable(truncate(e.Error, maxErrorBytes)), nullable(e.SessionKey),
		nullable(e.Client))
	queueFilterRuns(batch, e, t.Server+"/"+t.Tool)
}

// ToolCallRow is one tool call as the control API returns it.
type ToolCallRow struct {
	ID          int64       `json:"id"`
	TS          time.Time   `json:"ts"`
	TeamID      string      `json:"team_id,omitempty"`
	UserID      string      `json:"user_id,omitempty"`
	KeyID       string      `json:"key_id,omitempty"`
	Server      string      `json:"server"`
	Tool        string      `json:"tool"`
	Outcome     ToolOutcome `json:"outcome"`
	LatencyMS   int         `json:"latency_ms"`
	ArgBytes    int         `json:"arg_bytes"`
	ResultBytes int         `json:"result_bytes"`
	CostMicros  int64       `json:"cost_micros"`
	Error       string      `json:"error,omitempty"`
	SessionKey  string      `json:"session_key,omitempty"`
	Client      string      `json:"client,omitempty"`
}

// ToolCallQuery narrows a list of tool calls. Empty fields narrow nothing.
type ToolCallQuery struct {
	OrgID  string
	TeamID string
	KeyID  string
	UserID string
	Server string
	Tool   string
	From   time.Time
	To     time.Time
	Limit  int
}

// toolWhere is the condition every tool-call query shares, on $1 to $8. An
// empty organisation is an operator reading every tenant.
const toolWhere = ` WHERE ($1 = '' OR org_id = $1) AND ts >= $2 AND ts < $3
	AND ($4 = '' OR team_id = $4) AND ($5 = '' OR key_id = $5)
	AND ($6 = '' OR user_id = $6) AND ($7 = '' OR server = $7) AND ($8 = '' OR tool = $8)`

func (q ToolCallQuery) args() []any {
	return []any{q.OrgID, q.From, q.To, q.TeamID, q.KeyID, q.UserID, q.Server, q.Tool}
}

// ListToolCalls reads tool calls, newest first.
func (s *Store) ListToolCalls(ctx context.Context, q ToolCallQuery) ([]ToolCallRow, error) {
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, COALESCE(team_id, ''), COALESCE(user_id, ''),
		COALESCE(key_id, ''), server, tool, outcome, latency_ms, arg_bytes, result_bytes,
		cost_micros, COALESCE(error, ''), COALESCE(session_key, ''), COALESCE(client, '')
		FROM tool_calls`+toolWhere+` ORDER BY ts DESC, id DESC LIMIT $9`,
		append(q.args(), limit)...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanToolCall)
}

func scanToolCall(r row) (ToolCallRow, error) {
	var t ToolCallRow
	err := r.Scan(&t.ID, &t.TS, &t.TeamID, &t.UserID, &t.KeyID, &t.Server, &t.Tool, &t.Outcome,
		&t.LatencyMS, &t.ArgBytes, &t.ResultBytes, &t.CostMicros, &t.Error, &t.SessionKey,
		&t.Client)
	return t, err
}

// SessionToolCalls reads the tool calls of one session, oldest first. A
// session the client named has its calls named too. Any other is matched by
// time: the key's calls between its first request and its last, which on a
// key running two tasks at once includes both tasks' calls.
func (s *Store) SessionToolCalls(ctx context.Context, orgID string, a AgentSession) ([]ToolCallRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, ts, COALESCE(team_id, ''), COALESCE(user_id, ''),
		COALESCE(key_id, ''), server, tool, outcome, latency_ms, arg_bytes, result_bytes,
		cost_micros, COALESCE(error, ''), COALESCE(session_key, ''), COALESCE(client, '')
		FROM tool_calls
		WHERE ($1 = '' OR org_id = $1)
		  AND CASE WHEN $2 THEN session_key = $3
		           ELSE key_id = $4 AND ts >= $5 AND ts <= $6 END
		ORDER BY ts, id LIMIT 1000`,
		orgID, a.Stated, a.Key, a.KeyID, a.StartedAt, a.EndedAt)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanToolCall)
}

// ToolSummary is one tool's calls inside a window, added up.
type ToolSummary struct {
	Server      string `json:"server"`
	Tool        string `json:"tool"`
	Calls       int64  `json:"calls"`
	Failed      int64  `json:"failed"`
	Denied      int64  `json:"denied"`
	Refused     int64  `json:"refused"`
	AvgMS       int64  `json:"avg_ms"`
	ArgBytes    int64  `json:"arg_bytes"`
	ResultBytes int64  `json:"result_bytes"`
}

// SummarizeToolCalls adds up the calls of each tool inside a window, most
// called first. Failed counts both a tool's own failures and calls that got
// no result.
func (s *Store) SummarizeToolCalls(ctx context.Context, q ToolCallQuery) ([]ToolSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT server, tool, count(*),
		count(*) FILTER (WHERE outcome IN ('tool_error', 'error')),
		count(*) FILTER (WHERE outcome = 'denied'),
		count(*) FILTER (WHERE outcome = 'refused'),
		COALESCE(avg(latency_ms)::bigint, 0), COALESCE(sum(arg_bytes), 0),
		COALESCE(sum(result_bytes), 0)
		FROM tool_calls`+toolWhere+` GROUP BY server, tool ORDER BY count(*) DESC, server, tool`,
		q.args()...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (ToolSummary, error) {
		var t ToolSummary
		err := r.Scan(&t.Server, &t.Tool, &t.Calls, &t.Failed, &t.Denied, &t.Refused, &t.AvgMS,
			&t.ArgBytes, &t.ResultBytes)
		return t, err
	})
}
