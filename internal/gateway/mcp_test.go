package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/store"
)

// fakeMCP is a stand-in MCP server with two tools. It answers over SSE when
// sse is set, and records what it was sent.
type fakeMCP struct {
	mu     sync.Mutex
	bodies []string
	auth   []string
	sse    bool
	status int
}

func (f *fakeMCP) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(raw))
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	sse, status := f.sse, f.status
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	var msg rpcMessage
	_ = json.Unmarshal(raw, &msg)
	var result string
	switch msg.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-1")
		result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`
	case "tools/list":
		result = `{"tools":[{"name":"search_code","inputSchema":{"type":"object"}},` +
			`{"name":"create_issue","inputSchema":{"type":"object"}}]}`
	case "tools/call":
		var p struct {
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		text, _ := json.Marshal("got " + string(p.Arguments))
		result = `{"content":[{"type":"text","text":` + string(text) + `}]}`
	default:
		w.WriteHeader(http.StatusAccepted)
		return
	}
	answer := `{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":` + result + `}`
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`+"\n\n")
		_, _ = io.WriteString(w, "event: message\ndata: "+answer+"\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, answer)
}

func (f *fakeMCP) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// mcpHarness is a gateway with one MCP server, github, whose credential is
// "gh-token", in front of fake.
func mcpHarness(t *testing.T, res *policy.Resolved) (*harness, *fakeMCP) {
	t.Helper()
	fake := &fakeMCP{}
	upstream := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(upstream.Close)
	if res == nil {
		res = policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"}, nil, nil, nil)
	}
	h := &harness{budgets: &fakeBudgets{}, sink: &fakeSink{}, metrics: metrics.New()}
	h.src = &fakeSource{
		resolved: map[string]*policy.Resolved{testKey: res},
		mcp: map[string]policy.MCPServer{"github": {
			Alias: "github", URL: upstream.URL + "/mcp", APIKey: "gh-token", Enabled: true,
		}},
	}
	h.srv = New(h.src, h.budgets, ratelimit.New(), h.sink, h.metrics, Options{},
		slog.New(slog.DiscardHandler))
	h.gw = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.gw.Close)
	return h, fake
}

// rpc posts one JSON-RPC message to the github server through the gateway.
func (h *harness) rpc(t *testing.T, body string, header ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url("/mcp/github"), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

// allowing is a key allowed exactly these tool entries.
func allowing(entries ...string) *policy.Resolved {
	return policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&policy.Limits{AllowedTools: entries}, nil, nil)
}

// lastToolCall is the most recent tool-call row.
func lastToolCall(t *testing.T, h *harness) store.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range slicesReverse(h.sink.all()) {
			if ev.Tool != nil {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no tool call was recorded")
	return store.Event{}
}

func slicesReverse(in []store.Event) []store.Event {
	out := make([]store.Event, len(in))
	for i, e := range in {
		out[len(in)-1-i] = e
	}
	return out
}

func TestMCPProxyPresentsTheServersCredentialNotTheKey(t *testing.T) {
	h, fake := mcpHarness(t, nil)

	resp, body := h.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "protocolVersion") {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Errorf("the server's session id was not passed back")
	}
	if got := fake.auth[0]; got != "Bearer gh-token" {
		t.Errorf("server was sent Authorization %q, want the server's own credential", got)
	}
}

func TestMCPListShowsOnlyTheToolsTheKeyMayCall(t *testing.T) {
	h, _ := mcpHarness(t, allowing("github/search_code"))

	_, body := h.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(body, "search_code") || strings.Contains(body, "create_issue") {
		t.Errorf("tools/list = %s, want only search_code", body)
	}
}

func TestMCPListIsTrimmedInAStreamToo(t *testing.T) {
	h, fake := mcpHarness(t, allowing("github/search_code"))
	fake.sse = true

	resp, body := h.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "notifications/progress") {
		t.Errorf("the server's notification was not passed on: %s", body)
	}
	if strings.Contains(body, "create_issue") {
		t.Errorf("the stream listed a tool the key may not call: %s", body)
	}
}

func TestMCPCallIsForwardedAndRecorded(t *testing.T) {
	h, fake := mcpHarness(t, nil)

	_, body := h.rpc(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call",`+
		`"params":{"name":"search_code","arguments":{"q":"TODO"}}}`,
		"X-Keera-Session", "task-7")
	if !strings.Contains(body, `got {\"q\":\"TODO\"}`) {
		t.Errorf("answer = %s", body)
	}
	if n := len(fake.received()); n != 1 {
		t.Fatalf("server received %d requests", n)
	}
	ev := lastToolCall(t, h)
	if ev.Tool.Server != "github" || ev.Tool.Tool != "search_code" || ev.Tool.Outcome != store.ToolOK {
		t.Errorf("row = %+v", ev.Tool)
	}
	if ev.Tool.ArgBytes == 0 || ev.Tool.ResultBytes == 0 || ev.KeyID != "key_1" {
		t.Errorf("row = %+v, key %q; want the sizes and the key", ev.Tool, ev.KeyID)
	}
	if ev.SessionKey != store.StatedSessionKeyFor("key_1", "task-7") {
		t.Errorf("session = %q, want the one the client named", ev.SessionKey)
	}
}

func TestMCPCallInAStreamIsRecorded(t *testing.T) {
	h, fake := mcpHarness(t, nil)
	fake.sse = true

	_, body := h.rpc(t, `{"jsonrpc":"2.0","id":"a","method":"tools/call",`+
		`"params":{"name":"search_code","arguments":{}}}`)
	if !strings.Contains(body, `"id":"a"`) {
		t.Errorf("answer = %s", body)
	}
	if ev := lastToolCall(t, h); ev.Tool.Outcome != store.ToolOK {
		t.Errorf("outcome = %q", ev.Tool.Outcome)
	}
}

func TestMCPCallToAToolTheKeyMayNotCallIsNotForwarded(t *testing.T) {
	h, fake := mcpHarness(t, allowing("github/search_code"))

	_, body := h.rpc(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call",`+
		`"params":{"name":"create_issue","arguments":{"title":"x"}}}`)
	var answer rpcMessage
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Error == nil ||
		answer.Error.Code != rpcInvalidParams || string(answer.ID) != "4" {
		t.Errorf("answer = %s, want a JSON-RPC error for id 4", body)
	}
	if n := len(fake.received()); n != 0 {
		t.Errorf("the server received %d requests", n)
	}
	if ev := lastToolCall(t, h); ev.Tool.Outcome != store.ToolDenied || ev.Tool.Tool != "create_issue" {
		t.Errorf("row = %+v", ev.Tool)
	}
}

func TestMCPCallIsCheckedAsTheServerWillReadIt(t *testing.T) {
	// A repeated key is read as its last value here. A server that took the
	// first would run a tool nobody checked, so what it is sent is the call
	// as the gateway read it.
	h, fake := mcpHarness(t, allowing("github/search_code"))

	h.rpc(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call",`+
		`"params":{"name":"create_issue","name":"search_code","arguments":{}}}`)
	got := fake.received()
	if len(got) != 1 || strings.Contains(got[0], "create_issue") {
		t.Errorf("server was sent %v, want only the checked call", got)
	}
}

func TestFiltersReadAToolCallsArguments(t *testing.T) {
	res := allowing("github")
	res.Filters = []string{"redact"}
	h, fake := mcpHarness(t, res)
	h.src.filters = map[string]policy.Filter{"org_1/redact": patternFilterFor("org_1", "redact",
		policy.FilterRule{Pattern: `\bhunter2\b`, Replace: "[CREDENTIAL]"},
		policy.FilterRule{Pattern: `BEGIN PRIVATE KEY`, Refuse: true, Reason: "a private key"})}

	h.rpc(t, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"create_issue",`+
		`"arguments":{"title":"login fails","body":{"steps":["use hunter2"]}}}}`)
	got := fake.received()
	if len(got) != 1 || strings.Contains(got[0], "hunter2") || !strings.Contains(got[0], "[CREDENTIAL]") {
		t.Errorf("server was sent %v, want the secret taken out", got)
	}

	_, body := h.rpc(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"create_issue",`+
		`"arguments":{"body":"-----BEGIN PRIVATE KEY-----"}}}`)
	if !strings.Contains(body, `"isError":true`) || !strings.Contains(body, "a private key") {
		t.Errorf("answer = %s, want a failed tool result saying why", body)
	}
	if n := len(fake.received()); n != 1 {
		t.Errorf("the refused call reached the server")
	}
	ev := lastToolCall(t, h)
	if ev.Tool.Outcome != store.ToolRefused || len(ev.FilterRuns) != 1 {
		t.Errorf("row = %+v, filter runs %v", ev.Tool, ev.FilterRuns)
	}
}

func TestAToolCallCannotHideInABatch(t *testing.T) {
	h, fake := mcpHarness(t, nil)
	_, body := h.rpc(t, `[{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"search_code"}}]`)
	if !strings.Contains(body, `"error"`) || len(fake.received()) != 0 {
		t.Errorf("answer = %s, want the batch refused and nothing forwarded", body)
	}
}

func TestAServerRefusingTheGatewaysCredentialIsNotTheClientsFault(t *testing.T) {
	h, fake := mcpHarness(t, nil)
	fake.status = http.StatusUnauthorized

	resp, _ := h.rpc(t, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"search_code"}}`)
	// A 401 would read as a bad Keera key, and send the client off to sign in
	// to the server itself.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if ev := lastToolCall(t, h); ev.Tool.Outcome != store.ToolError {
		t.Errorf("outcome = %q", ev.Tool.Outcome)
	}
}

func TestAServerTheKeyMayNotUseDoesNotExist(t *testing.T) {
	h, _ := mcpHarness(t, allowing("jira"))
	resp, _ := h.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, h.url("/mcp/github"), strings.NewReader(`{}`))
	unauth, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Errorf("status without a key = %d, want 401", unauth.StatusCode)
	}
}

func TestHostedToolsAreTakenOutWhenBlocked(t *testing.T) {
	yes := true
	res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&policy.Limits{BlockHostedTools: &yes}, nil, nil)
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer), res)

	resp := h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":64,`+
		`"tools":[{"name":"read_file","input_schema":{"type":"object"}},`+
		`{"type":"web_search_20250305","name":"web_search"},`+
		`{"type":"bash_20250124","name":"bash"}],`+
		`"tool_choice":{"type":"tool","name":"web_search"},`+
		`"mcp_servers":[{"type":"url","url":"https://evil.example/mcp","name":"x"}],`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	got := nextSeen(t, seen).body
	for _, gone := range []string{"web_search", "mcp_servers", "evil.example", "tool_choice"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s reached Anthropic: %s", gone, got)
		}
	}
	for _, kept := range []string{"read_file", "bash_20250124"} {
		if !strings.Contains(got, kept) {
			t.Errorf("the client's own tool %s was taken out: %s", kept, got)
		}
	}
	if h := resp.Header.Get("X-Keera-Removed-Tools"); h != "web_search, mcp_servers" {
		t.Errorf("X-Keera-Removed-Tools = %q", h)
	}
}

func TestHostedToolsAreTakenOutOfAResponsesRequest(t *testing.T) {
	yes := true
	res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&policy.Limits{BlockHostedTools: &yes}, nil, nil)
	h, seen := providerHarness(t, "openai", "gpt-5.5",
		jsonBackend(`{"model":"gpt-5.5","usage":{"input_tokens":1,"output_tokens":1}}`), res)

	h.post(t, "/v1/responses", `{"model":"keera-frontier","input":"hi",`+
		`"tools":[{"type":"function","name":"shell","parameters":{}},{"type":"web_search"},`+
		`{"type":"mcp","server_label":"x","server_url":"https://evil.example/mcp"}],`+
		`"tool_choice":{"type":"web_search"}}`)
	got := nextSeen(t, seen).body
	if strings.Contains(got, "web_search") || strings.Contains(got, "evil.example") ||
		!strings.Contains(got, `"shell"`) {
		t.Errorf("forwarded = %s", got)
	}
}

func TestAToolCallWithoutAnIDIsNotForwarded(t *testing.T) {
	// Sent as a notification, a call would get no answer to trim or record,
	// and a lenient server would run it unchecked.
	h, fake := mcpHarness(t, allowing("github/search_code"))
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_issue"}}`,
		`[{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_issue"}}]`,
	} {
		_, answer := h.rpc(t, body)
		if !strings.Contains(answer, `"error"`) {
			t.Errorf("%s: answer = %s, want a refusal", body, answer)
		}
	}
	if n := len(fake.received()); n != 0 {
		t.Errorf("the server received %d requests", n)
	}
}

func TestAResumedStreamStillHidesToolsTheKeyMayNotCall(t *testing.T) {
	// An answer can arrive on a stream the client resumed, where the request
	// it answers was made on another connection.
	h, _ := mcpHarness(t, allowing("github/search_code"))
	x := &mcpExchange{s: h.srv, res: h.src.resolved[testKey],
		srv: h.src.mcp["github"], pending: map[string]*pendingRPC{}}
	out := x.serverMessage([]byte(`{"jsonrpc":"2.0","id":9,"result":{"tools":[` +
		`{"name":"search_code"},{"name":"create_issue"}]}}`))
	if out == nil || strings.Contains(string(out), "create_issue") {
		t.Errorf("resumed tools/list = %s, want create_issue taken out", out)
	}
}

func TestANativeEventLargerThanTheLimitEndsTheStream(t *testing.T) {
	src := strings.NewReader("data: " + strings.Repeat("x", 4096) + "\n\n")
	var dst strings.Builder
	_, err := pipeNative(&dst, func() {}, src, mcpEvents{&mcpExchange{}}, 1024)
	if err == nil || dst.Len() != 0 {
		t.Errorf("err = %v, wrote %d bytes; want the stream ended before the event", err, dst.Len())
	}
}
