package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// decodeResponses translates a Responses request and returns the chat body.
func decodeResponses(t *testing.T, raw string) map[string]any {
	t.Helper()
	out, err := responsesShape{}.decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestResponsesDecodeBuildsTheConversation(t *testing.T) {
	req := decodeResponses(t, `{"model":"keera-code","instructions":"be brief",
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
	// The reasoning item has no counterpart. The two calls of one turn join the
	// assistant message before them, which is the only shape a chat template
	// accepts.
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
	req := decodeResponses(t, `{"model":"keera-code","input":"hi",
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

// responsesEvents reads a Responses stream into its events' payloads.
func responsesEvents(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for block := range bytes.SplitSeq(raw, []byte("\n\n")) {
		for line := range bytes.SplitSeq(block, []byte("\n")) {
			if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
				var ev map[string]any
				if err := json.Unmarshal(data, &ev); err != nil {
					t.Fatalf("event %s: %v", data, err)
				}
				out = append(out, ev)
			}
		}
	}
	return out
}

func TestResponsesStreamEmitsTheEventSequence(t *testing.T) {
	src := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"he"}}]}`,
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\"a\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n")
	var dst bytes.Buffer
	stats, err := responsesShape{}.pipe(&dst, func() {}, src, "keera-code", true)
	if err != nil {
		t.Fatal(err)
	}

	events := responsesEvents(t, dst.Bytes())
	var types []string
	for i, ev := range events {
		types = append(types, ev["type"].(string))
		if ev["sequence_number"] != float64(i) {
			t.Errorf("event %d numbered %v", i, ev["sequence_number"])
		}
	}
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

	// Clients read the finished items off the closing events, so those carry
	// the whole text and the whole arguments.
	if item := events[13]["item"].(map[string]any); item["arguments"] != `{"a":1}` || item["call_id"] != "call_1" {
		t.Errorf("finished call = %v", item)
	}
	done := events[14]["response"].(map[string]any)
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

func TestResponsesSurfaceTranslatesForALocalModel(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500}}`), nil, guarded("obey the org", 64))

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
	if sent.MaxTokens != 64 || len(sent.Messages) != 3 || sent.Messages[0].Content != "obey the org" {
		t.Errorf("sent = %+v, want the ceiling and the guardrail first", sent)
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
	h, seen := providerHarness(t, "openai", "gpt-5.5", jsonBackend(answer), guarded("obey the org", 256))

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
	if sent["max_output_tokens"] != float64(256) {
		t.Errorf("max_output_tokens = %v, want the ceiling", sent["max_output_tokens"])
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
	if ev.InputTokens != 30 || ev.OutputTokens != 2 || ev.Estimated {
		t.Errorf("row = %d in, %d out, estimated %v", ev.InputTokens, ev.OutputTokens, ev.Estimated)
	}
}

func TestAStoredConversationOnlyGoesToOpenAI(t *testing.T) {
	h, seen := providerHarness(t, "openai", "gpt-5.5",
		jsonBackend(`{"model":"gpt-5.5","usage":{"input_tokens":1,"output_tokens":1}}`), nil)

	// No other model can read what OpenAI stored, so the refusal says so
	// rather than answer without the conversation.
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
}
