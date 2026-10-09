package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// ----------------------------------------------------------------- servers

const mcpColumns = `SELECT org_id, alias, url, description, auth_header, api_key_ct, enabled`

func scanMCPServer(r row) (policy.MCPServer, error) {
	var m policy.MCPServer
	if err := r.Scan(&m.OrgID, &m.Alias, &m.URL, &m.Description, &m.AuthHeader,
		&m.APIKeyCiphertext, &m.Enabled); err != nil {
		return policy.MCPServer{}, err
	}
	m.HasAPIKey = len(m.APIKeyCiphertext) > 0
	return m, nil
}

// LoadMCPServers reads every organisation's MCP servers.
func (s *Store) LoadMCPServers(ctx context.Context) ([]policy.MCPServer, error) {
	return queryAll(ctx, s.pool, scanMCPServer, mcpColumns+" FROM mcp_servers ORDER BY org_id, alias")
}

// ListMCPServers reads one organisation's MCP servers.
func (s *Store) ListMCPServers(ctx context.Context, orgID string) ([]policy.MCPServer, error) {
	return queryAll(ctx, s.pool, scanMCPServer, mcpColumns+" FROM mcp_servers WHERE org_id = $1 ORDER BY alias", orgID)
}

// MCPServer reads one of an organisation's servers, or ErrNotFound.
func (s *Store) MCPServer(ctx context.Context, orgID, alias string) (policy.MCPServer, error) {
	m, err := scanMCPServer(s.pool.QueryRow(ctx,
		mcpColumns+" FROM mcp_servers WHERE org_id = $1 AND alias = $2", orgID, alias))
	if err != nil {
		return policy.MCPServer{}, notFound(err)
	}
	return m, nil
}

// UpsertMCPServer creates or replaces one of an organisation's servers. Like
// UpsertModel it leaves the stored credential alone.
func (s *Store) UpsertMCPServer(ctx context.Context, m policy.MCPServer) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO mcp_servers (org_id, alias, url, description,
		auth_header, enabled, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6, now())
		ON CONFLICT (org_id, alias) DO UPDATE SET url = EXCLUDED.url,
			description = EXCLUDED.description, auth_header = EXCLUDED.auth_header,
			enabled = EXCLUDED.enabled, updated_at = now()`,
		m.OrgID, m.Alias, m.URL, m.Description, m.AuthHeader, m.Enabled)
	return err
}

// SetMCPCredential stores the sealed credential for one server. Nil clears it.
func (s *Store) SetMCPCredential(ctx context.Context, orgID, alias string, sealed []byte) error {
	return s.execOne(ctx,
		"UPDATE mcp_servers SET api_key_ct = $3, updated_at = now() WHERE org_id = $1 AND alias = $2",
		orgID, alias, sealed)
}

// DeleteMCPServer removes one of an organisation's servers.
func (s *Store) DeleteMCPServer(ctx context.Context, orgID, alias string) error {
	return s.execOne(ctx, "DELETE FROM mcp_servers WHERE org_id = $1 AND alias = $2", orgID, alias)
}

// -------------------------------------------------------------- tool calls

// ToolOutcome is what came of one tool call.
type ToolOutcome string

// The outcomes a tool call can have. See the tool_calls table.
const (
	ToolOK ToolOutcome = "ok"
	// ToolFailed is a result the tool itself marked as a failure.
	ToolFailed ToolOutcome = "tool_error"
	// ToolNoResult is a call that got no result, from the server or the gateway.
	ToolNoResult ToolOutcome = "error"
	ToolDenied   ToolOutcome = "denied"
	ToolRefused  ToolOutcome = "refused"
	// ToolInputRequired is a server asking for the user's input before it
	// answers. The client sends the call again with it.
	ToolInputRequired ToolOutcome = "input_required"
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
	batch.Queue(`INSERT INTO tool_calls (ts, org_id, project_id, user_id, key_id, server, tool,
		outcome, latency_ms, arg_bytes, result_bytes, cost_micros, error, session_key, client)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		e.TS, e.OrgID, nullable(e.ProjectID), nullable(e.UserID), nullable(e.KeyID),
		t.Server, t.Tool, string(t.Outcome), e.Latency.Milliseconds(), t.ArgBytes, t.ResultBytes,
		e.CostMicros, nullable(truncate(e.Error, maxErrorBytes)), nullable(e.SessionKey),
		nullable(e.Client))
	queueFilterRuns(batch, e, t.Server+"/"+t.Tool)
}

// ToolCallRow is one tool call as the control API returns it.
type ToolCallRow struct {
	ID          int64       `json:"id"`
	TS          time.Time   `json:"ts"`
	ProjectID   string      `json:"project_id,omitempty"`
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

// ToolCallQuery narrows a list of tool calls. OrgID is required; the other
// empty fields narrow nothing.
type ToolCallQuery struct {
	OrgID     string
	ProjectID string
	KeyID     string
	UserID    string
	Server    string
	Tool      string
	From      time.Time
	To        time.Time
	Limit     int
	// Before pages backwards by id, as the request log does.
	Before int64
}

// toolWhere is the condition every tool-call query shares, on $1 to $8. The
// organisation is always named: the log is read one organisation at a time.
const toolWhere = ` WHERE org_id = $1 AND ts >= $2 AND ts < $3
	AND ($4 = '' OR project_id = $4) AND ($5 = '' OR key_id = $5)
	AND ($6 = '' OR user_id = $6) AND ($7 = '' OR server = $7) AND ($8 = '' OR tool = $8)`

func (q ToolCallQuery) args() []any {
	return []any{q.OrgID, q.From, q.To, q.ProjectID, q.KeyID, q.UserID, q.Server, q.Tool}
}

// ListToolCalls reads tool calls, newest first.
func (s *Store) ListToolCalls(ctx context.Context, q ToolCallQuery) ([]ToolCallRow, error) {
	return queryAll(ctx, s.pool, scanToolCall, toolCallColumns+toolWhere+`
		AND ($9 = 0 OR id < $9) ORDER BY id DESC LIMIT $10`,
		append(q.args(), q.Before, pageLimit(q.Limit, 100, 5000))...)
}

// toolCallColumns reads what scanToolCall scans.
const toolCallColumns = `SELECT id, ts, COALESCE(project_id, ''), COALESCE(user_id, ''),
	COALESCE(key_id, ''), server, tool, outcome, latency_ms, arg_bytes, result_bytes,
	cost_micros, COALESCE(error, ''), COALESCE(session_key, ''), COALESCE(client, '')
	FROM tool_calls`

func scanToolCall(r row) (ToolCallRow, error) {
	var t ToolCallRow
	err := r.Scan(&t.ID, &t.TS, &t.ProjectID, &t.UserID, &t.KeyID, &t.Server, &t.Tool, &t.Outcome,
		&t.LatencyMS, &t.ArgBytes, &t.ResultBytes, &t.CostMicros, &t.Error, &t.SessionKey,
		&t.Client)
	return t, err
}

// SessionToolCalls reads the tool calls of one session, oldest first: those
// between its first request and its last. A session the client named has its
// calls named too. Any other is matched by key, which on a key running two
// tasks at once includes both tasks' calls. The time bound applies to both: a
// client that reuses its session id after the idle gap starts a new session.
func (s *Store) SessionToolCalls(ctx context.Context, orgID string, a AgentSession) ([]ToolCallRow, error) {
	return queryAll(ctx, s.pool, scanToolCall, toolCallColumns+`
		WHERE ($1 = '' OR org_id = $1)
		  AND CASE WHEN $2 THEN session_key = $3 ELSE key_id = $4 END
		  AND ts >= $5 AND ts <= $6
		ORDER BY ts, id LIMIT 1000`,
		orgID, a.Stated, a.Key, a.KeyID, a.StartedAt, a.EndedAt)
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
	return queryAll(ctx, s.pool, scanToolSummary, `SELECT server, tool, count(*),
		`+countOf("outcome", ToolFailed, ToolNoResult)+`,
		`+countOf("outcome", ToolDenied)+`,
		`+countOf("outcome", ToolRefused)+`,
		COALESCE(avg(latency_ms)::bigint, 0), COALESCE(sum(arg_bytes), 0),
		COALESCE(sum(result_bytes), 0)
		FROM tool_calls`+toolWhere+` GROUP BY server, tool ORDER BY count(*) DESC, server, tool`,
		q.args()...)
}

func scanToolSummary(r row) (ToolSummary, error) {
	var t ToolSummary
	err := r.Scan(&t.Server, &t.Tool, &t.Calls, &t.Failed, &t.Denied, &t.Refused, &t.AvgMS,
		&t.ArgBytes, &t.ResultBytes)
	return t, err
}
