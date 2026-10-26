package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestResponsesDecodeBuildsTheConversation(t *testing.T) {
	req := decodeAs(t, responsesShape{}, `{"model":"keera-code","instructions":"be brief",
		"max_output_tokens":512,"input":[
		{"role":"developer","content":"use tabs"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"fix it"}]},
		{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"looking"}]},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"c1","output":"main.go"},
		{"type":"function_call_output","call_id":"c2","output":"/src"}]}`)

	msgs := messagesOf(t, req)
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i], _ = m["role"].(string)
	}
	// The reasoning item is dropped. The two calls of one turn join the
	// assistant message before them, the only shape a chat template accepts.
	want := "system system user assistant tool tool"
	if got := strings.Join(roles, " "); got != want {
		t.Fatalf("roles = %q, want %q", got, want)
	}
	if msgs[0]["content"] != "be brief" || msgs[1]["content"] != "use tabs" {
		t.Errorf("instructions then developer message, got %v and %v", msgs[0]["content"], msgs[1]["content"])
	}
	calls, _ := msgs[3]["tool_calls"].([]any)
	if msgs[3]["content"] != "looking" || len(calls) != 2 {
		t.Errorf("assistant = %v, want its text and both calls", msgs[3])
	}
	if msgs[4]["tool_call_id"] != "c1" || msgs[5]["content"] != "/src" {
		t.Errorf("tool outputs = %v, %v", msgs[4], msgs[5])
	}
	if req["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want max_output_tokens carried over", req["max_tokens"])
	}
}

func TestResponsesDecodeTranslatesToolsAndFormat(t *testing.T) {
	req := decodeAs(t, responsesShape{}, `{"model":"keera-code","input":"hi",
		"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}},
			{"type":"web_search"}],
		"tool_choice":{"type":"function","name":"shell"},
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":true}}}`)

	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want only the function; web search runs on OpenAI's servers", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" || fn["parameters"] == nil {
		t.Errorf("function = %v", fn)
	}
	tc := req["tool_choice"].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "shell" {
		t.Errorf("tool_choice = %v", tc)
	}
	rf := req["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["name"] != "out" {
		t.Errorf("response_format = %v", rf)
	}
}

// codexTools is how Codex declares tools: MCP servers and subagents in
// namespaces, and apply_patch as a custom tool with a grammar.
const codexTools = `"tools":[
	{"type":"function","name":"exec_command","parameters":{"type":"object"}},
	{"type":"namespace","name":"mcp__demo__","description":"Demo MCP server","tools":[
		{"type":"function","name":"get_weather","description":"Weather","parameters":{"type":"object"}}]},
	{"type":"namespace","name":"multi_agent_v1","description":"Subagents","tools":[
		{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]},
	{"type":"custom","name":"apply_patch","description":"Edit files.",
		"format":{"type":"grammar","syntax":"lark","definition":"start: begin_patch"}},
	{"type":"web_search"}]`

func TestResponsesDecodeDeclaresNamespacesAndCustomTools(t *testing.T) {
	// The chat shape has neither, but dropping them would break Codex.
	req := decodeAs(t, responsesShape{}, `{"model":"keera-code","input":"hi",`+codexTools+`,
		"tool_choice":{"type":"custom","name":"apply_patch"}}`)

	fns := map[string]map[string]any{}
	for _, tool := range req["tools"].([]any) {
		fn := tool.(map[string]any)["function"].(map[string]any)
		fns[fn["name"].(string)] = fn
	}
	for _, name := range []string{
		"exec_command", "mcp__demo__get_weather",
		"multi_agent_v1__spawn_agent", "apply_patch",
	} {
		if fns[name] == nil {
			t.Errorf("no tool %q in %v", name, req["tools"])
		}
	}
	if len(fns) != 4 {
		t.Errorf("%d tools, want 4; web search runs on OpenAI's servers", len(fns))
	}
	patch := fns["apply_patch"]
	if props := patch["parameters"].(map[string]any)["properties"].(map[string]any); props["input"] == nil {
		t.Errorf("apply_patch parameters = %v, want its text as 'input'", patch["parameters"])
	}
	if d, _ := patch["description"].(string); !strings.Contains(d, "Edit files.") ||
		!strings.Contains(d, "lark grammar:\nstart: begin_patch") {
		t.Errorf("apply_patch description = %q, want the grammar in it", d)
	}
	tc := req["tool_choice"].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "apply_patch" {
		t.Errorf("tool_choice = %v", tc)
	}
}

func TestResponsesDecodeReplaysNamespacedAndCustomCalls(t *testing.T) {
	req := decodeAs(t, responsesShape{}, `{"model":"keera-code","input":[
		{"role":"user","content":"weather, then fix it"},
		{"type":"function_call","call_id":"c1","namespace":"mcp__demo__","name":"get_weather",
			"arguments":"{\"city\":\"Bern\"}"},
		{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"},
		{"type":"function_call_output","call_id":"c1","output":"sunny"},
		{"type":"custom_tool_call_output","call_id":"c2","output":"Done!"}]}`)

	msgs := messagesOf(t, req)
	calls := msgs[1]["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("assistant = %v, want both calls", msgs[1])
	}
	first := calls[0].(map[string]any)["function"].(map[string]any)
	second := calls[1].(map[string]any)["function"].(map[string]any)
	if first["name"] != "mcp__demo__get_weather" {
		t.Errorf("namespaced call = %v", first)
	}
	if second["name"] != "apply_patch" || second["arguments"] != `{"input":"*** Begin Patch"}` {
		t.Errorf("custom call = %v, want its text as the one argument", second)
	}
	if msgs[2]["tool_call_id"] != "c1" || msgs[3]["tool_call_id"] != "c2" || msgs[3]["content"] != "Done!" {
		t.Errorf("tool outputs = %v, %v", msgs[2], msgs[3])
	}
}

func TestResponsesNameCallsAsTheClientDeclaredThem(t *testing.T) {
	// Codex finds a tool by namespace and name, and runs a custom tool on its
	// text, so calls must come back as declared.
	backend := jsonBackend(`{"choices":[{"message":{"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"mcp__demo__get_weather","arguments":"{\"city\":\"Bern\"}"}},
		{"id":"call_2","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}},
		{"id":"call_3","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	h := newHarness(t, backend, nil, nil)

	resp := h.post(t, "/v1/responses", `{"model":"keera-code","input":"hi",`+codexTools+`}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Output) != 3 {
		t.Fatalf("output = %s", body)
	}
	weather, patch, shell := out.Output[0], out.Output[1], out.Output[2]
	if weather["type"] != "function_call" || weather["name"] != "get_weather" ||
		weather["namespace"] != "mcp__demo__" || weather["call_id"] != "call_1" {
		t.Errorf("namespaced call = %v", weather)
	}
	if patch["type"] != "custom_tool_call" || patch["name"] != "apply_patch" ||
		patch["input"] != "*** Begin Patch" || patch["arguments"] != nil {
		t.Errorf("custom call = %v", patch)
	}
	if _, ok := shell["namespace"]; ok || shell["name"] != "exec_command" {
		t.Errorf("plain call = %v", shell)
	}
}

func TestResponsesStreamNamesCallsAsTheClientDeclaredThem(t *testing.T) {
	_, sh, err := responsesShape{}.decodeBody([]byte(`{"model":"keera-code","input":"hi",` + codexTools + `}`))
	if err != nil {
		t.Fatal(err)
	}
	events := pipeAs(t, sh,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" Patch\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"mcp__demo__get_weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if got := names(events); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	if added := events[2].payload["item"].(map[string]any); added["type"] != "custom_tool_call" {
		t.Errorf("added = %v", added)
	}
	if first := events[3].payload; first["delta"] != "*** Begin" {
		t.Errorf("first delta = %v, want the text as it arrived", first)
	}
	if done := events[5].payload; done["input"] != "*** Begin Patch" {
		t.Errorf("input done = %v", done)
	}
	patch := events[6].payload["item"].(map[string]any)
	if patch["input"] != "*** Begin Patch" || patch["status"] != "completed" ||
		patch["id"] != events[2].payload["item"].(map[string]any)["id"] {
		t.Errorf("finished custom call = %v", patch)
	}
	if weather := events[10].payload["item"].(map[string]any); weather["name"] != "get_weather" ||
		weather["namespace"] != "mcp__demo__" || weather["arguments"] != "{}" {
		t.Errorf("finished namespaced call = %v", weather)
	}
	output := events[11].payload["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "custom_tool_call" {
		t.Errorf("completed output = %v", output)
	}
}

// A custom tool's text is streamed as its arguments arrive, however they are
// split. A split escape waits for the rest.
func TestInputStreamReadsTheTextAsItArrives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pieces []string
		want   []string
	}{
		{"plain", []string{`{"input":"ab`, `cd"}`}, []string{"ab", "cd"}},
		{"spaced opening, cut in two", []string{`{ "inp`, `ut" : "x`, `y"}`}, []string{"", "x", "y"}},
		{"escape cut", []string{`{"input":"a\`, `nb"}`}, []string{"a", "\nb"}},
		{"surrogate pair cut", []string{`{"input":"\ud83d`, `\ude00!"}`}, []string{"", "\U0001f600!"}},
		{"another shape", []string{`{"patch":"x"}`}, []string{""}},
		{"bare text", []string{`*** Begin Patch`}, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in inputStream
			for i, p := range tc.pieces {
				if got := in.read(p); got != tc.want[i] {
					t.Errorf("piece %d: read %q, want %q", i, got, tc.want[i])
				}
			}
		})
	}
}

func TestResponsesEncodeBuildsItems(t *testing.T) {
	out, status := responsesShape{}.encode([]byte(`{"choices":[{"message":{"content":"done",
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]},
		"finish_reason":"length"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}`),
		"keera-code", 200)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	var resp struct {
		Object            string            `json:"object"`
		Status            string            `json:"status"`
		Model             string            `json:"model"`
		IncompleteDetails map[string]string `json:"incomplete_details"`
		Output            []map[string]any  `json:"output"`
		Usage             responsesUsage    `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" || resp.Model != "keera-code" {
		t.Errorf("object %q model %q", resp.Object, resp.Model)
	}
	if resp.Status != "incomplete" || resp.IncompleteDetails["reason"] != "max_output_tokens" {
		t.Errorf("status = %q %v, want incomplete for a length stop", resp.Status, resp.IncompleteDetails)
	}
	if len(resp.Output) != 2 || resp.Output[0]["type"] != "message" ||
		resp.Output[1]["type"] != "function_call" || resp.Output[1]["call_id"] != "call_1" {
		t.Errorf("output = %v", resp.Output)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.InputTokensDetails.CachedTokens != 4 ||
		resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestResponsesStreamEmitsTheEventSequence(t *testing.T) {
	src := strings.NewReader(sseStream(
		`{"choices":[{"delta":{"content":"he"}}]}`,
		`{"choices":[{"delta":{"content":"llo"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\"a\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
	))
	var dst bytes.Buffer
	stats, err := responsesShape{}.pipe(&dst, func() {}, src, "keera-code", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}

	events := readEvents(t, dst.String())
	for i, ev := range events {
		if ev.payload["sequence_number"] != float64(i) {
			t.Errorf("event %d numbered %v", i, ev.payload["sequence_number"])
		}
	}
	types := names(events)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("events =\n%v\nwant\n%v", types, want)
	}

	// Clients read finished items from the closing events, so those carry
	// the whole text and arguments.
	if item := events[13].payload["item"].(map[string]any); item["arguments"] != `{"a":1}` || item["call_id"] != "call_1" {
		t.Errorf("finished call = %v", item)
	}
	done := events[14].payload["response"].(map[string]any)
	output := done["output"].([]any)
	if len(output) != 2 || done["status"] != "completed" {
		t.Errorf("completed response = %v", done)
	}
	if usage := done["usage"].(map[string]any); usage["input_tokens"] != float64(7) {
		t.Errorf("usage = %v", usage)
	}
	if stats.usage == nil || stats.usage.InputTokens != 7 || stats.deltas != 4 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestResponsesStreamFailsAFailedStream(t *testing.T) {
	// A cut-off stream closed as completed would read as a finished turn.
	for name, upstream := range map[string]string{
		"cut off": "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1",` +
			`"function":{"name":"write","arguments":"{\"content\":\"half"}}]}}]}` + "\n\n",
		"error chunk": sseStream(`{"choices":[{"delta":{"content":"done"}}]}`,
			`{"choices":[{"delta":{"content":"half"}}]}`,
			`{"error":{"message":"CUDA out of memory","type":"InternalServerError","code":500}}`),
		"empty": sseStream(),
	} {
		var buf bytes.Buffer
		_, err := responsesShape{}.pipe(&buf, func() {}, strings.NewReader(upstream),
			"keera-code", true, DefaultMaxResponseBytes)
		if err == nil {
			t.Errorf("%s: pipe reported no failure, so the row would look like a whole answer", name)
		}
		events := readEvents(t, buf.String())
		last := events[len(events)-1]
		if last.name != "response.failed" {
			t.Errorf("%s: the stream ends with %q, want response.failed", name, last.name)
			continue
		}
		resp := last.payload["response"].(map[string]any)
		e, _ := resp["error"].(map[string]any)
		if resp["status"] != "failed" || e["code"] != "server_error" {
			t.Errorf("%s: response = %v, want status failed and a server_error", name, resp)
		}
		// The item under way is not whole, so it is neither closed nor output.
		if out := resp["output"].([]any); len(out) != 0 {
			t.Errorf("%s: output = %v, want nothing", name, out)
		}
		for _, ev := range events {
			if ev.name == "response.completed" || ev.name == "response.output_item.done" {
				t.Errorf("%s: the failed stream carried %s", name, ev.name)
			}
		}
		if name == "error chunk" && !strings.Contains(e["message"].(string), "CUDA out of memory") {
			t.Errorf("%s: message = %q, want the upstream's", name, e["message"])
		}
	}
}

func TestResponsesSurfaceTranslatesForALocalModel(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500}}`), nil, guarded("obey the org"))

	resp := h.post(t, "/v1/responses", `{"model":"keera-code","instructions":"be brief",`+
		`"max_output_tokens":4096,"input":"hi"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var sent struct {
		MaxTokens int `json:"max_tokens"`
		Messages  []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MaxTokens != 4096 || len(sent.Messages) != 3 || sent.Messages[0].Content != "obey the org" {
		t.Errorf("sent = %+v, want max_output_tokens carried over and the guardrail first", sent)
	}
	if !strings.Contains(string(body), `"object":"response"`) {
		t.Errorf("answer = %s", body)
	}
	ev := h.sink.last(t)
	if ev.InputTokens != 1000 || ev.OutputTokens != 500 || ev.CostMicros != 3000 {
		t.Errorf("row = %d in, %d out, %d micros", ev.InputTokens, ev.OutputTokens, ev.CostMicros)
	}
}

func TestResponsesGoToOpenAIInTheirOwnAPI(t *testing.T) {
	answer := `{"id":"resp_1","object":"response","model":"gpt-5.5","status":"completed",` +
		`"output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}],` +
		`"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":800},"output_tokens":50}}`
	h, seen := providerHarness(t, "openai", "gpt-5.5", jsonBackend(answer), guarded("obey the org"))

	resp := h.post(t, "/v1/responses", `{"model":"keera-frontier","instructions":"be brief",`+
		`"temperature":0.2,"include":["reasoning.encrypted_content"],"input":"hi"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	got := nextSeen(t, seen)
	if got.path != "/v1/responses" || got.header.Get("Authorization") != "Bearer sk-upstream" {
		t.Errorf("forwarded to %s with %q, want OpenAI's own /v1/responses and a bearer token",
			got.path, got.header.Get("Authorization"))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["instructions"] != "obey the org\n\nbe brief" {
		t.Errorf("instructions = %v, want the guardrail first", sent["instructions"])
	}
	// A reasoning model refuses a temperature.
	if _, ok := sent["temperature"]; ok {
		t.Error("temperature was forwarded to a reasoning model")
	}
	if sent["include"] == nil || sent["model"] != "gpt-5.5" {
		t.Errorf("the body lost what only OpenAI reads: %v", sent)
	}

	if !strings.Contains(string(body), `"encrypted_content"`) ||
		!strings.Contains(string(body), `"model":"keera-frontier"`) {
		t.Errorf("answer = %s, want OpenAI's items under the alias", body)
	}
	ev := h.sink.last(t)
	// 200 at 1 micro, 800 cached at a tenth, 50 out at 4.
	if ev.InputTokens != 1000 || ev.CachedInputTokens != 800 || ev.CostMicros != 480 {
		t.Errorf("row = %d in (%d cached), %d micros", ev.InputTokens, ev.CachedInputTokens, ev.CostMicros)
	}
}

func TestNativeResponsesStreamReadsTheUsage(t *testing.T) {
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"response.created","sequence_number":0,"response":{"id":"r","model":"gpt-5.5","status":"in_progress"}}`,
			`{"type":"response.output_text.delta","sequence_number":1,"delta":"hi"}`,
			`{"type":"response.completed","sequence_number":2,"response":{"id":"r","model":"gpt-5.5",` +
				`"status":"completed","usage":{"input_tokens":30,"output_tokens":2}}}`,
		} {
			_, _ = io.WriteString(w, "event: x\ndata: "+e+"\n\n")
		}
	}
	h, _ := providerHarness(t, "openai", "gpt-5.5", backend, nil)

	resp := h.post(t, "/v1/responses", `{"model":"keera-frontier","stream":true,"input":"hi"}`)
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "gpt-5.5") {
		t.Errorf("the stream names the backend model: %s", body)
	}
	ev := h.sink.last(t)
	if ev.InputTokens != 30 || ev.OutputTokens != 2 || ev.Estimated || ev.Error != "" {
		t.Errorf("row = %d in, %d out, estimated %v, error %q", ev.InputTokens, ev.OutputTokens,
			ev.Estimated, ev.Error)
	}
}

// The 200 is sent before the provider fails, so only the row's error marks
// the stream as failed.
func TestAFailedNativeStreamIsRecordedAsFailed(t *testing.T) {
	for _, tc := range []struct{ provider, model, path, body, events, want string }{
		{
			"openai", "gpt-5.5", "/v1/responses", `{"model":"keera-frontier","stream":true,"input":"hi"}`,
			`{"type":"response.created","response":{"id":"r","status":"in_progress"}}` + "\n\n" +
				`{"type":"response.failed","response":{"id":"r","status":"failed",` +
				`"error":{"code":"server_error","message":"it broke"}}}`,
			"it broke",
		},
		{
			"openai", "gpt-5.5", "/v1/responses", `{"model":"keera-frontier","stream":true,"input":"hi"}`,
			`{"type":"response.output_text.delta","delta":"hi"}`, "stopped before",
		},
		{
			"anthropic", "claude-opus-5", "/v1/messages", `{"model":"keera-frontier","max_tokens":64,` +
				`"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			`{"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n" +
				`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"Overloaded",
		},
	} {
		backend := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for e := range strings.SplitSeq(tc.events, "\n\n") {
				_, _ = io.WriteString(w, "data: "+e+"\n\n")
			}
		}
		h, _ := providerHarness(t, tc.provider, tc.model, backend, nil)
		resp := h.post(t, tc.path, tc.body)
		_, _ = io.ReadAll(resp.Body)
		if ev := h.sink.last(t); !strings.Contains(ev.Error, tc.want) {
			t.Errorf("%s: error = %q, want it to say %q", tc.provider, ev.Error, tc.want)
		}
	}
}

func TestAStoredConversationOnlyGoesToOpenAI(t *testing.T) {
	h, seen := providerHarness(t, "openai", "gpt-5.5",
		jsonBackend(`{"model":"gpt-5.5","usage":{"input_tokens":1,"output_tokens":1}}`), nil)

	// No other model can read what OpenAI stored, so this is refused.
	resp := h.post(t, "/v1/responses", `{"model":"keera-code","previous_response_id":"resp_1","input":"and?"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "previous_response_id") {
		t.Errorf("status = %d: %s, want a 400 naming the field", resp.StatusCode, body)
	}

	resp = h.post(t, "/v1/responses", `{"model":"keera-frontier","previous_response_id":"resp_1","input":"and?"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want OpenAI's own model to take it", resp.StatusCode)
	}
	if got := nextSeen(t, seen); !strings.Contains(got.body, `"previous_response_id"`) {
		t.Errorf("forwarded = %s", got.body)
	}

	// An SDK may send the field empty, which continues nothing.
	for _, empty := range []string{`null`, `""`} {
		resp = h.post(t, "/v1/responses", `{"model":"keera-code","previous_response_id":`+empty+
			`,"conversation":null,"input":"hi"}`)
		if resp.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("an empty previous_response_id (%s) was refused: %s", empty, body)
		}
	}
}

func TestAFilterReadsACustomToolsOutput(t *testing.T) {
	// For Codex, file contents travel in apply_patch's output. A request
	// OpenAI gets in its own shape is filtered in that shape.
	answer := `{"id":"resp_1","object":"response","model":"gpt-5.5","status":"completed",` +
		`"output":[],"usage":{"input_tokens":10,"output_tokens":5}}`
	h, seen := providerHarness(t, "openai", "gpt-5.5", jsonBackend(answer), nil)
	h.src.resolved[testKey].Filters = []string{"redact"}
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilter(policy.FilterRule{Pattern: `\bhunter2\b`, Replace: "[CREDENTIAL]"}),
	}

	resp := h.post(t, "/v1/responses", `{"model":"keera-frontier","input":[
		{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"password = hunter2"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if sent := nextSeen(t, seen); strings.Contains(sent.body, "hunter2") ||
		!strings.Contains(sent.body, "[CREDENTIAL]") {
		t.Errorf("sent = %s, want the output redacted", sent.body)
	}
}

func TestAnItemThatFitsNoFieldIsDropped(t *testing.T) {
	// A tool search call's arguments are an object, not a string. That must
	// not fail the request.
	in := `{"model":"m","input":[{"type":"tool_search_call","call_id":"ts","arguments":{}},` +
		`{"role":"user","content":"hi"}]}`
	msgs := messagesOf(t, decodeAs(t, responsesShape{}, in))
	if len(msgs) != 1 || msgs[0]["content"] != "hi" {
		t.Errorf("messages = %v, want the user's message alone", msgs)
	}
}

func TestImagesFromToolsFollowAllTheResults(t *testing.T) {
	// Codex's view_image answers with an image, which the tool role cannot
	// carry. A user turn between two results would split the second from its
	// call.
	in := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"a","name":"view_image","arguments":"{}"},` +
		`{"type":"function_call","call_id":"b","name":"view_image","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"see"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},` +
		`{"type":"function_call_output","call_id":"b","output":"done"}]}`
	msgs := messagesOf(t, decodeAs(t, responsesShape{}, in))
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if !slices.Equal(roles, []string{"assistant", "tool", "tool", "user"}) {
		t.Fatalf("roles = %v, want the call, both results, then the image", roles)
	}
	if msgs[1]["content"] != "see" {
		t.Errorf("first result = %v, want its text", msgs[1]["content"])
	}
	parts, _ := msgs[3]["content"].([]any)
	if len(parts) != 1 || !strings.Contains(fmt.Sprint(parts[0]), "data:image/png;base64,AAAA") {
		t.Errorf("image turn = %v, want the tool's image", msgs[3]["content"])
	}
}

func TestALocalShellCallStaysInTheConversation(t *testing.T) {
	in := `{"model":"m","input":[{"role":"user","content":"x"},` +
		`{"type":"local_shell_call","call_id":"c","status":"completed","action":{"type":"exec","command":["ls"]}},` +
		`{"type":"local_shell_call_output","call_id":"c","output":"a b"}]}`
	msgs := messagesOf(t, decodeAs(t, responsesShape{}, in))
	if len(msgs) != 3 || msgs[2]["role"] != "tool" || msgs[2]["content"] != "a b" {
		t.Fatalf("messages = %v, want the call and its output after the user", msgs)
	}
	calls, _ := msgs[1]["tool_calls"].([]any)
	if len(calls) != 1 || !strings.Contains(fmt.Sprint(calls[0]), `"command":["ls"]`) {
		t.Errorf("call = %v, want the command as its arguments", msgs[1]["tool_calls"])
	}
}

func TestATextFileReachesTheModel(t *testing.T) {
	in := `{"model":"m","input":[{"role":"user","content":[` +
		`{"type":"input_file","filename":"a.txt","file_data":"data:text/plain;base64,aGVsbG8="},` +
		`{"type":"input_file","filename":"b.pdf","file_data":"data:application/pdf;base64,JVBERi0="},` +
		`{"type":"input_text","text":"what is in it?"}]}]}`
	msgs := messagesOf(t, decodeAs(t, responsesShape{}, in))
	if len(msgs) != 1 || msgs[0]["content"] != "a.txt\n\nhello\n\nwhat is in it?" {
		t.Errorf("messages = %v, want the text file and the question", msgs)
	}
}
