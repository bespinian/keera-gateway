package store

import (
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestMCPCatalogueRoundTrip(t *testing.T) {
	st, ctx := db(t)
	anOrg(t, st, ctx, "org_1")
	anOrg(t, st, ctx, "org_2")

	m := policy.MCPServer{
		OrgID: "org_1", Alias: "github", URL: "https://api.githubcopilot.com/mcp/", Description: "Issues.",
		AuthHeader: "X-Api-Key", Enabled: true,
	}
	if err := st.UpsertMCPServer(ctx, m); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	if err := st.SetMCPCredential(ctx, "org_1", "github", []byte("sealed")); err != nil {
		t.Fatalf("SetMCPCredential: %v", err)
	}
	// An edit that does not mention the credential keeps it.
	if err := st.UpsertMCPServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := st.MCPServer(ctx, "org_1", "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	same := got.Alias == m.Alias && got.URL == m.URL && got.Description == m.Description &&
		got.AuthHeader == m.AuthHeader && got.Enabled == m.Enabled
	if !same || !got.HasAPIKey {
		t.Errorf("MCPServer = %+v, want %+v with its credential", got, m)
	}
	// Another organisation has none, and can have its own of the same alias.
	if _, err := st.MCPServer(ctx, "org_2", "github"); err != ErrNotFound {
		t.Errorf("org_2 reads org_1's server: %v", err)
	}
	if list, _ := st.ListMCPServers(ctx, "org_2"); len(list) != 0 {
		t.Errorf("org_2 lists %v, want none", list)
	}
	if err := st.DeleteMCPServer(ctx, "org_1", "github"); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.LoadMCPServers(ctx); len(list) != 0 {
		t.Errorf("servers after delete = %v", list)
	}
}

// A server is named in a tool allow-list by its alias, alone or before one of
// its tools, and either counts as a use.
func TestMCPServerUsersFindsEveryEntryForm(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	if users, err := st.MCPServerUsers(ctx, f.orgID, "jira"); err != nil || len(users) != 0 {
		t.Fatalf("MCPServerUsers = %+v, %v, want none", users, err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID,
		policy.Limits{AllowedTools: []string{"jira"}}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID,
		policy.Limits{AllowedTools: []string{"jira/search", "jirafake"}}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	users, err := st.MCPServerUsers(ctx, f.orgID, "jira")
	if err != nil {
		t.Fatalf("MCPServerUsers: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("MCPServerUsers = %+v, want the organisation and the project", users)
	}
	if users, _ := st.MCPServerUsers(ctx, f.orgID, "jir"); len(users) != 0 {
		t.Errorf("MCPServerUsers(jir) = %+v, want none: a prefix is not a name", users)
	}
}

func TestAKeyResolvesItsToolGuardrails(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	yes := true
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, f.orgID, policy.Limits{
		AllowedTools: []string{"github"}, BlockHostedTools: &yes,
	}); err != nil {
		t.Fatalf("PutGuardrail: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, f.projectID, policy.Limits{
		AllowedTools: []string{"github/search_code"},
	}); err != nil {
		t.Fatal(err)
	}
	lim, err := st.GetGuardrail(ctx, policy.ScopeOrg, f.orgID)
	if err != nil || !slices.Equal(lim.AllowedTools, []string{"github"}) || lim.BlockHostedTools == nil {
		t.Errorf("GetGuardrail = %+v, %v", lim, err)
	}
	res, err := st.LookupKey(ctx, f.hash)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if !slices.Equal(res.AllowedTools, []string{"github/search_code"}) || !res.BlockHostedTools {
		t.Errorf("resolved tools = %v, block %v", res.AllowedTools, res.BlockHostedTools)
	}
}

func TestToolCallsAreWrittenAndRead(t *testing.T) {
	st, ctx := db(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	call := func(ts time.Time, tool string, outcome ToolOutcome, session string, cost int64) Event {
		return Event{
			TS: ts, OrgID: "org_1", ProjectID: "project_1", KeyID: "key_1", Latency: 40 * time.Millisecond,
			SessionKey: session, CostMicros: cost,
			Scopes: []policy.Scope{{Type: policy.ScopeOrg, ID: "org_1", Period: policy.PeriodMonth}},
			FilterRuns: []FilterRun{{Filter: "redact", Mode: policy.FilterModePattern,
				Outcome: FilterPass}},
			Tool: &ToolCall{Server: "github", Tool: tool, Outcome: outcome, ArgBytes: 10, ResultBytes: 20},
		}
	}
	if err := st.WriteEvents(ctx, []Event{
		call(now.Add(-2*time.Minute), "search_code", ToolOK, "", 0),
		call(now.Add(-time.Minute), "create_issue", ToolDenied, "", 0),
		call(now, "search_code", ToolFailed, StatedSessionKeyFor("key_1", "t"), 7),
	}); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}

	q := ToolCallQuery{OrgID: "org_1", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	rows, err := st.ListToolCalls(ctx, q)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(rows) != 3 || rows[0].Outcome != ToolFailed || rows[0].LatencyMS != 40 || rows[0].ArgBytes != 10 {
		t.Fatalf("rows = %+v", rows)
	}
	// The log pages backwards by id, as the request log does.
	older := q
	older.Before = rows[0].ID
	older.Limit = 1
	next, err := st.ListToolCalls(ctx, older)
	if err != nil || len(next) != 1 || next[0].ID != rows[1].ID {
		t.Fatalf("the page before %d = %+v, %v; want %d", rows[0].ID, next, err, rows[1].ID)
	}
	// Nothing about a tool call goes into the request log.
	var requests int
	_ = st.pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&requests)
	if requests != 0 {
		t.Errorf("%d tool calls were written as requests", requests)
	}
	var runs int
	_ = st.pool.QueryRow(ctx, "SELECT count(*) FROM filter_runs WHERE alias = 'github/search_code'").Scan(&runs)
	if runs != 2 {
		t.Errorf("filter runs against the tool = %d, want 2", runs)
	}
	var spent int64
	_ = st.pool.QueryRow(ctx, "SELECT COALESCE(sum(micros), 0) FROM spend").Scan(&spent)
	if spent != 7 {
		t.Errorf("spend = %d, want the filters' 7 micros", spent)
	}

	sum, err := st.SummarizeToolCalls(ctx, q)
	if err != nil {
		t.Fatalf("SummarizeToolCalls: %v", err)
	}
	if len(sum) != 2 || sum[0].Tool != "search_code" || sum[0].Calls != 2 || sum[0].Failed != 1 ||
		sum[1].Denied != 1 {
		t.Errorf("summary = %+v", sum)
	}

	byTime, err := st.SessionToolCalls(ctx, "org_1", AgentSession{
		KeyID: "key_1", StartedAt: now.Add(-90 * time.Second), EndedAt: now,
	})
	if err != nil {
		t.Fatalf("SessionToolCalls: %v", err)
	}
	if len(byTime) != 2 || byTime[0].Tool != "create_issue" {
		t.Errorf("calls matched by time = %+v", byTime)
	}
	named, _ := st.SessionToolCalls(ctx, "org_1", AgentSession{
		Stated: true, Key: StatedSessionKeyFor("key_1", "t"), KeyID: "key_1",
		StartedAt: now.Add(-time.Second), EndedAt: now,
	})
	if len(named) != 1 || named[0].Outcome != ToolFailed {
		t.Errorf("calls matched by session = %+v", named)
	}
	// The same id stated again after the idle gap is a new session, and the
	// old one's calls are not part of it.
	later, _ := st.SessionToolCalls(ctx, "org_1", AgentSession{
		Stated: true, Key: StatedSessionKeyFor("key_1", "t"), KeyID: "key_1",
		StartedAt: now.Add(time.Hour), EndedAt: now.Add(2 * time.Hour),
	})
	if len(later) != 0 {
		t.Errorf("a later session with the same id got the earlier one's calls: %+v", later)
	}

	gone, err := st.PurgeUsage(ctx, now.Add(time.Hour))
	if err != nil || gone == 0 {
		t.Fatalf("PurgeUsage = %d, %v", gone, err)
	}
	if rows, _ := st.ListToolCalls(ctx, q); len(rows) != 0 {
		t.Errorf("tool calls survived retention: %v", rows)
	}
}
