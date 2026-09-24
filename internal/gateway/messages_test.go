package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeMessages translates a Messages request and unmarshals the chat
// completion request it became.
func decodeMessages(t *testing.T, in string) map[string]any {
	t.Helper()
	raw, err := anthropicShape{}.decode([]byte(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the translated request is not valid JSON: %v", err)
	}
	return out
}

// messagesOf pulls the translated messages array out as a list of maps.
func messagesOf(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	arr, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("the translated request has no messages array: %#v", req["messages"])
	}
	out := make([]map[string]any, 0, len(arr))
	for _, m := range arr {
		msg, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("a translated message is not an object: %#v", m)
		}
		out = append(out, msg)
	}
	return out
}

// ------------------------------------------------------------------- requests

func TestDecodeMovesTheSystemPromptIntoTheConversation(t *testing.T) {
	// The Messages API carries the system prompt beside the conversation; the
	// OpenAI shape carries it as the first message. A client that sends it as
	// an array of blocks - which is how a client that marks a prompt cache
	// sends it - must reach the model as one prompt.
	req := decodeMessages(t, `{
		"model": "keera-code",
		"max_tokens": 100,
		"system": [{"type":"text","text":"first"},{"type":"text","text":"second"}],
		"messages": [{"role":"user","content":"hi"}]
	}`)

	msgs := messagesOf(t, req)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the system prompt plus the turn", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Errorf("messages[0].role = %v, want system", msgs[0]["role"])
	}
	if got, want := msgs[0]["content"], "first\n\nsecond"; got != want {
		t.Errorf("system content = %q, want %q; the blocks were not joined", got, want)
	}
	if got, want := req["max_tokens"], float64(100); got != want {
		t.Errorf("max_tokens = %v, want %v; the guardrail ceiling clamps this field", got, want)
	}
}

func TestDecodeKeepsTheGuardrailAbleToGoFirst(t *testing.T) {
	// The guardrail's system prompt is prepended to the translated body by serve.
	// This is the property that makes that possible: whatever the client sent,
	// the result is a messages array that prependSystem can splice into.
	req := decodeMessages(t, `{"model":"keera-code","max_tokens":1,
		"messages":[{"role":"user","content":"hi"}]}`)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !b.prependSystem("obey the org") {
		t.Fatal("the guardrail could not be prepended; a Messages request would escape it")
	}
	msgs := messagesOf(t, decodeThrough(t, b.encode()))
	if msgs[0]["content"] != "obey the org" {
		t.Errorf("messages[0] = %v, want the guardrail first", msgs[0]["content"])
	}
}

// decodeThrough unmarshals an already-translated body.
func decodeThrough(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecodeTurnsToolUseIntoToolCalls(t *testing.T) {
	// A tool call is an assistant content block in one API and a tool_calls
	// entry in the other, and the arguments change from a document to a string
	// carrying that document.
	req := decodeMessages(t, `{
		"model": "keera-code",
		"max_tokens": 100,
		"messages": [
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Bern"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"12C"}
			]}
		]
	}`)

	msgs := messagesOf(t, req)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want user, assistant, tool: %#v", len(msgs), msgs)
	}
	if msgs[1]["content"] != "checking" {
		t.Errorf("assistant content = %v, want the text block", msgs[1]["content"])
	}
	calls, ok := msgs[1]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %#v, want one call", msgs[1]["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("tool call id = %v, want toolu_1; the result could not be matched to it", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "weather" {
		t.Errorf("tool call name = %v, want weather", fn["name"])
	}
	if got, want := fn["arguments"], `{"city":"Bern"}`; got != want {
		t.Errorf("arguments = %v, want the string %q", got, want)
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "toolu_1" {
		t.Errorf("messages[2] = %#v, want a tool message for toolu_1", msgs[2])
	}
	if msgs[2]["content"] != "12C" {
		t.Errorf("tool content = %v, want 12C", msgs[2]["content"])
	}
}

func TestDecodePutsToolResultsBeforeTheUserText(t *testing.T) {
	// A Messages turn can carry both a tool result and something the person
	// typed. The tool message has to come first: it answers the assistant turn
	// before it, and a user message in between would break that pairing.
	req := decodeMessages(t, `{
		"model": "keera-code",
		"max_tokens": 100,
		"messages": [{"role":"user","content":[
			{"type":"text","text":"and now this"},
			{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}
		]}]
	}`)

	msgs := messagesOf(t, req)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the tool result and the text: %#v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "tool" {
		t.Errorf("messages[0].role = %v, want tool first", msgs[0]["role"])
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "and now this" {
		t.Errorf("messages[1] = %#v, want the user's text", msgs[1])
	}
}

func TestDecodeMovesAToolResultImageToTheUserTurn(t *testing.T) {
	// A tool that returned a screenshot has nowhere to put it in the OpenAI
	// shape: the tool role carries text only. Dropping it would silently lose
	// the thing the tool was called for.
	req := decodeMessages(t, `{
		"model": "keera-code",
		"max_tokens": 100,
		"messages": [{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"text","text":"here it is"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]}
		]}]
	}`)

	msgs := messagesOf(t, req)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the tool result and the image: %#v", len(msgs), msgs)
	}
	if msgs[0]["content"] != "here it is" {
		t.Errorf("tool content = %v, want the text that came with the image", msgs[0]["content"])
	}
	parts, ok := msgs[1]["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("user content = %#v, want one image part", msgs[1]["content"])
	}
	part := parts[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatalf("part type = %v, want image_url", part["type"])
	}
	url := part["image_url"].(map[string]any)["url"]
	if got, want := url, "data:image/png;base64,AAAA"; got != want {
		t.Errorf("image url = %v, want %q", got, want)
	}
}

func TestDecodeIgnoresFieldsWithNoCounterpart(t *testing.T) {
	// A client sends capabilities the gateway has never heard of as new
	// top-level fields. Rejecting them would break the client on the release
	// that introduced them, so they are dropped and the request still runs.
	req := decodeMessages(t, `{
		"model": "keera-code",
		"max_tokens": 100,
		"thinking": {"type":"adaptive"},
		"context_management": {"edits":[]},
		"output_config": {"effort":"high"},
		"some_field_from_next_year": true,
		"messages": [{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"thinking","thinking":"pondering","signature":"sig"}
		]}]
	}`)

	for _, gone := range []string{"thinking", "context_management", "output_config",
		"some_field_from_next_year"} {
		if _, present := req[gone]; present {
			t.Errorf("%q reached the inference plane; it has no OpenAI counterpart", gone)
		}
	}
	msgs := messagesOf(t, req)
	if len(msgs) != 1 || msgs[0]["content"] != "hi" {
		t.Errorf("messages = %#v, want just the text; the thinking block should be dropped", msgs)
	}
}

func TestDecodeTranslatesToolsAndToolChoice(t *testing.T) {
	req := decodeMessages(t, `{
		"model": "keera-code",
		"max_tokens": 100,
		"messages": [{"role":"user","content":"hi"}],
		"tools": [
			{"name":"read","description":"read a file",
			 "input_schema":{"type":"object","properties":{"path":{"type":"string"}}},
			 "defer_loading": true},
			{"name":"ping"}
		],
		"tool_choice": {"type":"any","disable_parallel_tool_use":true},
		"stop_sequences": ["STOP"],
		"top_k": 40,
		"temperature": 0.2
	}`)

	tools, ok := req["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want two functions", req["tools"])
	}
	first := tools[0].(map[string]any)
	if first["type"] != "function" {
		t.Errorf("tools[0].type = %v, want function", first["type"])
	}
	fn := first["function"].(map[string]any)
	if fn["name"] != "read" || fn["description"] != "read a file" {
		t.Errorf("tools[0].function = %#v, want the declaration carried over", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("tools[0].function.parameters = %#v, want the input schema", fn["parameters"])
	}
	// A tool declared with no schema still has to be callable.
	second := tools[1].(map[string]any)["function"].(map[string]any)
	params, ok := second["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("tools[1].parameters = %#v, want an empty object schema", second["parameters"])
	}

	if req["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want required; 'any' means the model must call one", req["tool_choice"])
	}
	if req["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", req["parallel_tool_calls"])
	}
	stop, ok := req["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "STOP" {
		t.Errorf("stop = %#v, want the stop sequences", req["stop"])
	}
	if req["top_k"] != float64(40) || req["temperature"] != 0.2 {
		t.Errorf("sampling fields = %v/%v, want 40/0.2", req["top_k"], req["temperature"])
	}
}

func TestDecodeNamesAToolChoiceByFunction(t *testing.T) {
	req := decodeMessages(t, `{"model":"keera-code","max_tokens":1,
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"tool","name":"read"}}`)
	choice, ok := req["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice = %#v, want an object naming the function", req["tool_choice"])
	}
	if choice["function"].(map[string]any)["name"] != "read" {
		t.Errorf("tool_choice = %#v, want the named function", choice)
	}
}

func TestDecodeRejectsWhatItCannotTranslate(t *testing.T) {
	for name, in := range map[string]string{
		"no model":      `{"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
		"no messages":   `{"model":"keera-code","max_tokens":1,"messages":[]}`,
		"bad role":      `{"model":"keera-code","max_tokens":1,"messages":[{"role":"tool","content":"x"}]}`,
		"bad content":   `{"model":"keera-code","max_tokens":1,"messages":[{"role":"user","content":42}]}`,
		"not an object": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (anthropicShape{}).decode([]byte(in)); err == nil {
				t.Error("the request was accepted; it should have been refused with a reason")
			}
		})
	}
}

// ------------------------------------------------------------------ responses

func TestEncodeBuildsAMessagesResponse(t *testing.T) {
	body, status := anthropicShape{}.encode([]byte(`{
		"id": "chatcmpl-1",
		"model": "served-name",
		"choices": [{"index":0,"message":{"role":"assistant","content":"hello"},
			"finish_reason":"stop"}],
		"usage": {"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}
	}`), "keera-code", 200)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}

	var out struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the response is not valid JSON: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", out.Type, out.Role)
	}
	if !strings.HasPrefix(out.ID, "msg_") {
		t.Errorf("id = %q, want a msg_ identifier", out.ID)
	}
	// The alias is the API contract; naming the backend model here would hand
	// the client the one detail the alias exists to keep swappable.
	if out.Model != "keera-code" {
		t.Errorf("model = %q, want the alias, not the served name", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "hello" {
		t.Errorf("content = %#v, want one text block", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", out.StopReason)
	}
	if out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want 11 in and 3 out", out.Usage)
	}
}

func TestEncodeBuildsToolUseBlocks(t *testing.T) {
	body, _ := anthropicShape{}.encode([]byte(`{
		"choices": [{"index":0,"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"call_1","type":"function",
				"function":{"name":"read","arguments":"{\"path\":\"a.go\"}"}}]},
			"finish_reason":"tool_calls"}]
	}`), "keera-code", 200)

	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use; a client decides whether to run the tool on this",
			out.StopReason)
	}
	if len(out.Content) != 1 {
		t.Fatalf("content = %#v, want one tool_use block", out.Content)
	}
	block := out.Content[0]
	if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "read" {
		t.Errorf("block = %#v, want a tool_use for read", block)
	}
	// The arguments arrive as a string and leave as a document.
	if got, want := string(block.Input), `{"path":"a.go"}`; got != want {
		t.Errorf("input = %s, want the object %s", got, want)
	}
}

func TestEncodeSurvivesUnparseableToolArguments(t *testing.T) {
	// A model that emitted invalid JSON is a real occurrence. An empty object
	// the client can report a tool failure against beats a response it cannot
	// parse at all.
	body, _ := anthropicShape{}.encode([]byte(`{
		"choices": [{"message":{"tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"read","arguments":"{\"path\": "}}]},
			"finish_reason":"tool_calls"}]
	}`), "keera-code", 200)
	if !json.Valid(body) {
		t.Fatalf("the response is not valid JSON: %s", body)
	}
	if !strings.Contains(string(body), `"input":{}`) {
		t.Errorf("body = %s, want an empty input object", body)
	}
}

func TestEncodeMapsStopReasons(t *testing.T) {
	for finish, want := range map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"": "end_turn", "something_new": "end_turn",
	} {
		if got := stopReason(finish); got != want {
			t.Errorf("stopReason(%q) = %q, want %q", finish, got, want)
		}
	}
}

func TestEncodeTranslatesAnUpstreamError(t *testing.T) {
	// The wording is carried through unchanged: a client recovers from some
	// refusals by matching on it, and rewriting it would break that recovery
	// while keeping the status that looks like it should have worked.
	body, status := anthropicShape{}.encode(
		[]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens",`+
			`"type":"invalid_request_error","code":"context_length_exceeded"}}`),
		"keera-code", 400)
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}

	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "error" || out.Error.Type != "invalid_request_error" {
		t.Errorf("envelope = %#v, want a Messages error envelope", out)
	}
	if !strings.Contains(out.Error.Message, "maximum context length is 8192") {
		t.Errorf("message = %q, want the upstream wording unchanged", out.Error.Message)
	}
}

func TestEncodeReportsAnUnreadableSuccessAsAFailure(t *testing.T) {
	// A 200 carrying something that is not a completion - an ingress error
	// page, a proxy's own JSON - must not reach the client as a success. It
	// would surface as a malformed response with nothing to act on.
	body, status := anthropicShape{}.encode([]byte(`<html>gateway timeout</html>`),
		"keera-code", 200)
	if status != 502 {
		t.Errorf("status = %d, want 502", status)
	}
	if !json.Valid(body) || !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("body = %s, want a Messages error envelope", body)
	}
}

func TestErrorTypesFollowTheStatus(t *testing.T) {
	// A client decides whether to retry, re-authenticate or stop on this
	// value, so each status has to arrive under the name that API gives it.
	for status, want := range map[int]string{
		400: "invalid_request_error", 401: "authentication_error",
		402: "billing_error", 403: "permission_error", 404: "not_found_error",
		413: "request_too_large", 429: "rate_limit_error",
		500: "api_error", 503: "overloaded_error",
	} {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("anthropicErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}
