package store

import (
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestMCPCatalogueRoundTrip(t *testing.T) {
	st, ctx := db(t)

	m := policy.MCPServer{
		Alias: "github", URL: "https://api.githubcopilot.com/mcp/", Description: "Issues.",
		AuthHeader: "X-Api-Key", APIKeyEnv: "GITHUB_TOKEN", Enabled: true, Managed: true,
	}
	if err := st.UpsertMCPServer(ctx, m); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	if err := st.SetMCPCredential(ctx, "github", []byte("sealed")); err != nil {
		t.Fatalf("SetMCPCredential: %v", err)
	}
	// Applying the file again must not erase a credential set by hand.
	if err := st.UpsertMCPServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := st.MCPServer(ctx, "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if !got.SameDeclaration(m) || !got.Managed || !got.HasAPIKey {
		t.Errorf("MCPServer = %+v, want %+v with its credential", got, m)
	}
	if err := st.UnmanageMCPServers(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MCPServer(ctx, "github"); got.Managed {
		t.Error("a server the file no longer names is still managed")
	}
	if err := st.DeleteMCPServer(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.LoadMCPServers(ctx); len(list) != 0 {
		t.Errorf("servers after delete = %v", list)
	}
}

func TestAKeyResolvesItsToolGuardrails(t *testing.T) {
	st, ctx := db(t)
	f := newFixture(t, st, ctx)
	yes := true
	if err := st.PutPolicy(ctx, policy.ScopeOrg, f.orgID, policy.Limits{
		AllowedTools: []string{"github"}, BlockHostedTools: &yes,
	}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	if err := st.PutPolicy(ctx, policy.ScopeTeam, f.teamID, policy.Limits{
		AllowedTools: []string{"github/search_code"},
	}); err != nil {
		t.Fatal(err)
	}
	lim, err := st.GetPolicy(ctx, policy.ScopeOrg, f.orgID)
	if err != nil || !slices.Equal(lim.AllowedTools, []string{"github"}) || lim.BlockHostedTools == nil {
		t.Errorf("GetPolicy = %+v, %v", lim, err)
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
			TS: ts, OrgID: "org_1", TeamID: "team_1", KeyID: "key_1", Latency: 40 * time.Millisecond,
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
	})
	if len(named) != 1 || named[0].Outcome != ToolFailed {
		t.Errorf("calls matched by session = %+v", named)
	}

	gone, err := st.PurgeUsage(ctx, now.Add(time.Hour))
	if err != nil || gone == 0 {
		t.Fatalf("PurgeUsage = %d, %v", gone, err)
	}
	if rows, _ := st.ListToolCalls(ctx, q); len(rows) != 0 {
		t.Errorf("tool calls survived retention: %v", rows)
	}
}
