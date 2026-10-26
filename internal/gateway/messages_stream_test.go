package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// sseEvent is one event read back off a Messages or Responses stream.
type sseEvent struct {
	name    string
	payload map[string]any
}

// readEvents parses a Messages or Responses stream into its events. It is
// strict about framing: an event with no name or unparseable data is a stream
// a client's SDK would reject.
func readEvents(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for block := range strings.SplitSeq(strings.TrimSpace(raw), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var ev sseEvent
		for line := range strings.SplitSeq(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				if err := json.Unmarshal([]byte(data), &ev.payload); err != nil {
					t.Fatalf("event data is not valid JSON: %v\n%s", err, data)
				}
			}
		}
		if ev.name == "" {
			t.Fatalf("an event arrived with no name; an SDK cannot dispatch it:\n%s", block)
		}
		// The type in the payload and the name in the frame have to agree:
		// clients dispatch on one or the other and there is no telling which.
		if got, ok := ev.payload["type"].(string); !ok || got != ev.name {
			t.Errorf("event %q carries type %v; the two must agree", ev.name, ev.payload["type"])
		}
		out = append(out, ev)
	}
	return out
}

func names(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.name)
	}
	return out
}

// pipeAs runs a chat completion stream through sh's translation.
func pipeAs(t *testing.T, sh shape, chunks ...string) []sseEvent {
	t.Helper()
	var buf bytes.Buffer
	_, err := sh.pipe(&buf, func() {},
		strings.NewReader(sseStream(chunks...)), "keera-code", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	return readEvents(t, buf.String())
}

func TestMessagesStreamOpensAndClosesTheMessage(t *testing.T) {
	// A client waits for message_stop before it considers the turn over, so
	// every stream has to produce the full envelope.
	events := pipeAs(t, anthropicShape{},
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
	)

	want := []string{
		"message_start", "content_block_start",
		"content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := names(events); !slices.Equal(got, want) {
		t.Fatalf("events = %v,\nwant %v", got, want)
	}

	msg := events[0].payload["message"].(map[string]any)
	if msg["model"] != "keera-code" {
		t.Errorf("message_start names model %v, want the alias", msg["model"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("message_start role = %v, want assistant", msg["role"])
	}

	var text strings.Builder
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		delta := e.payload["delta"].(map[string]any)
		if delta["type"] != "text_delta" {
			t.Errorf("delta type = %v, want text_delta", delta["type"])
		}
		text.WriteString(delta["text"].(string))
	}
	if got := text.String(); got != "Hello" {
		t.Errorf("the stream delivered %q, want %q", got, "Hello")
	}

	delta := events[len(events)-2]
	if got := delta.payload["delta"].(map[string]any)["stop_reason"]; got != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", got)
	}
	usage := delta.payload["usage"].(map[string]any)
	if usage["input_tokens"] != float64(9) || usage["output_tokens"] != float64(2) {
		t.Errorf("message_delta usage = %v, want 9 in and 2 out", usage)
	}
}

func TestMessagesStreamNeverForwardsTheUsageChunk(t *testing.T) {
	// The gateway asks for a usage record the client never requested. Its
	// numbers belong in message_delta: a chunk with no choices is not an event
	// this protocol has.
	events := pipeAs(t, anthropicShape{},
		`{"choices":[{"delta":{"content":"x"}}]}`, stopChunk,
		`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`,
	)
	for _, e := range events {
		if e.name == "content_block_delta" {
			if _, leaked := e.payload["usage"]; leaked {
				t.Errorf("a usage record reached the client as a content event: %v", e.payload)
			}
		}
	}
	if n := len(events); events[n-1].name != "message_stop" {
		t.Errorf("the stream ends with %q, want message_stop", events[n-1].name)
	}
}

func TestMessagesStreamBuildsToolUseBlocks(t *testing.T) {
	// Argument fragments leave whole, as one input_json_delta inside an open
	// tool_use block that closes before the message ends.
	events := pipeAs(t, anthropicShape{},
		`{"choices":[{"delta":{"content":"looking"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
			`"function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`,
	)

	// The text block must be closed before the tool block opens: blocks are
	// sequential and a client indexes them in the order they arrive.
	want := []string{
		"message_start", "content_block_start", "content_block_delta", // text
		"content_block_stop",
		"content_block_start", "content_block_delta", // tool
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := names(events); !slices.Equal(got, want) {
		t.Fatalf("events = %v,\nwant %v", got, want)
	}

	toolStart := events[4].payload
	if toolStart["index"] != float64(1) {
		t.Errorf("the tool block opened at index %v, want 1", toolStart["index"])
	}
	block := toolStart["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "read" {
		t.Errorf("content_block = %v, want a tool_use for read", block)
	}

	var args strings.Builder
	for _, e := range events[5:6] {
		delta := e.payload["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("delta type = %v, want input_json_delta", delta["type"])
		}
		args.WriteString(delta["partial_json"].(string))
	}
	if got, want := args.String(), `{"path":"a.go"}`; got != want {
		t.Errorf("the arguments arrived as %q, want %q", got, want)
	}
	if got := events[len(events)-2].payload["delta"].(map[string]any)["stop_reason"]; got != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", got)
	}
}

func TestMessagesStreamSeparatesParallelToolCalls(t *testing.T) {
	// Two tool calls in one turn are two blocks. Sharing one would merge two
	// argument documents into an unparseable third.
	events := pipeAs(t, anthropicShape{},
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
			`"function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function",`+
			`"function":{"name":"b","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	var ids []string
	for _, e := range events {
		if e.name != "content_block_start" {
			continue
		}
		ids = append(ids, e.payload["content_block"].(map[string]any)["id"].(string))
	}
	if !slices.Equal(ids, []string{"call_1", "call_2"}) {
		t.Errorf("tool blocks = %v, want one per call", ids)
	}
	// Two opens, two closes, and no block left open at the end.
	starts, stops := 0, 0
	for _, e := range events {
		switch e.name {
		case "content_block_start":
			starts++
		case "content_block_stop":
			stops++
		}
	}
	if starts != 2 || stops != 2 {
		t.Errorf("%d block starts and %d stops, want 2 and 2", starts, stops)
	}
}

func TestMessagesStreamEndsAFailedStreamWithAnError(t *testing.T) {
	// Closed with message_stop, a broken stream would read as a finished turn,
	// with a tool call cut short in it. An error event ends it and tells the
	// client to try again.
	for name, upstream := range map[string]string{
		"cut off": "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1",` +
			`"function":{"name":"write","arguments":"{\"content\":\"half"}}]}}]}` + "\n\n",
		"error chunk": sseStream(`{"choices":[{"delta":{"content":"half"}}]}`,
			`{"error":{"message":"CUDA out of memory","type":"InternalServerError","code":500}}`),
		"empty": sseStream(),
	} {
		var buf bytes.Buffer
		_, err := anthropicShape{}.pipe(&buf, func() {}, strings.NewReader(upstream),
			"keera-code", true, DefaultMaxResponseBytes)
		if err == nil {
			t.Errorf("%s: pipe reported no failure, so the row would look like a whole answer", name)
		}
		events := readEvents(t, buf.String())
		last := events[len(events)-1]
		if last.name != "error" {
			t.Errorf("%s: the stream ends with %q, want error", name, last.name)
			continue
		}
		e := last.payload["error"].(map[string]any)
		if last.payload["type"] != "error" || e["type"] != "overloaded_error" {
			t.Errorf("%s: error event = %v, want an overloaded_error envelope", name, last.payload)
		}
		for _, ev := range events {
			if ev.name == "message_delta" || ev.name == "message_stop" {
				t.Errorf("%s: the failed stream carried %s", name, ev.name)
			}
		}
		if name == "error chunk" && !strings.Contains(e["message"].(string), "CUDA out of memory") {
			t.Errorf("%s: message = %q, want the upstream's", name, e["message"])
		}
	}
}

func TestMessagesStreamIsWellFormedWithNoContentAtAll(t *testing.T) {
	// An upstream that finished without a word still has to produce a message
	// a client can parse and discard.
	events := pipeAs(t, anthropicShape{}, stopChunk)
	if got := names(events); !slices.Equal(got, []string{"message_start", "message_delta", "message_stop"}) {
		t.Errorf("events = %v, want an empty but complete message", got)
	}
}

func TestMessagesStreamReportsUsageForBilling(t *testing.T) {
	var buf bytes.Buffer
	stats, err := anthropicShape{}.pipe(&buf, func() {}, strings.NewReader(sseStream(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`, stopChunk,
		`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":2,"total_tokens":42}}`,
	)), "keera-code", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if stats.usage == nil {
		t.Fatal("no usage was captured; the request would be billed as free")
	}
	if stats.usage.InputTokens != 40 || stats.usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want 40/2", *stats.usage)
	}
	// deltas is the fallback estimate for a stream the client abandoned.
	if stats.deltas != 2 {
		t.Errorf("deltas = %d, want 2", stats.deltas)
	}
	if stats.firstAt.IsZero() {
		t.Error("time to first token was not recorded")
	}
}

func TestATranslatedStreamCountsReasoningAsTheFirstToken(t *testing.T) {
	// The translation drops reasoning, so the client's first event is the
	// text. The model started earlier, and that is what the first token is.
	start := time.Now()
	stats, err := anthropicShape{}.pipe(io.Discard, func() {}, paused(pause,
		"data: "+`{"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n",
		"data: "+`{"choices":[{"delta":{"reasoning_content":"hm"}}]}`+"\n\n",
		sseStream(`{"choices":[{"delta":{"content":"a"}}]}`, stopChunk),
	), "keera-code", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	got := stats.firstAt.Sub(start)
	if got < pause {
		t.Errorf("first token after %v, want at least %v: the role chunk was counted", got, pause)
	}
	if got >= 2*pause {
		t.Errorf("first token after %v, want under %v: the reasoning was not counted", got, 2*pause)
	}
	// A client that hangs up is charged on the deltas, so reasoning counts
	// here as it does on the chat API.
	if stats.deltas != 2 {
		t.Errorf("deltas = %d, want 2: the reasoning and the text", stats.deltas)
	}
}

func TestMessagesSurfaceEnforcesTheSamePolicyAsChat(t *testing.T) {
	// The point of translating rather than proxying: an Anthropic-shaped
	// client passes the same guardrail and accounting as every other client.
	res := policy.Resolve(
		policy.Key{ID: "key_1", OrgID: "org_1", ProjectID: "project_1"},
		&policy.Limits{
			SystemPrompt: func() *string { p := "obey the org"; return &p }(),
		}, nil, nil)
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"},`+
		`"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`), nil, res)

	resp := h.post(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":4096,"system":"be brief",`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var sent struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "served-name" {
		t.Errorf("the inference plane was asked for %q, want the backend model", sent.Model)
	}
	if sent.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want the client's 4096", sent.MaxTokens)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("upstream messages = %#v, want guardrail, client system, turn", sent.Messages)
	}
	if sent.Messages[0].Content != "obey the org" {
		t.Errorf("messages[0] = %q, want the org guardrail first", sent.Messages[0].Content)
	}
	if sent.Messages[1].Content != "be brief" {
		t.Errorf("messages[1] = %q, want the client's own system prompt kept", sent.Messages[1].Content)
	}

	ev := h.sink.last(t)
	if ev.InputTokens != 1000 || ev.OutputTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", ev.InputTokens, ev.OutputTokens)
	}
	if want := int64(1000 + 2000); ev.CostMicros != want {
		t.Errorf("cost = %d, want %d", ev.CostMicros, want)
	}
	if h.budgets.total() != ev.CostMicros {
		t.Errorf("charged %d, want %d", h.budgets.total(), ev.CostMicros)
	}
}

func TestMessagesSurfaceStreamsEndToEnd(t *testing.T) {
	h := newHarness(t, sseBackend(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":2,"total_tokens":32}}`,
	), nil, nil)

	resp := h.post(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":100,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want an event stream", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want \"no\" - an ingress buffers the stream without it", got)
	}
	events := readEvents(t, string(body))
	if len(events) == 0 || events[0].name != "message_start" {
		t.Fatalf("the stream does not open a message:\n%s", body)
	}
	if events[len(events)-1].name != "message_stop" {
		t.Fatalf("the stream does not close the message:\n%s", body)
	}

	// The gateway asked for the usage the client had no way to request.
	if !strings.Contains(string(<-h.upstreamBodies), `"include_usage":true`) {
		t.Error("include_usage was not added, so the stream would have been billed as free")
	}
	ev := h.sink.last(t)
	if !ev.Stream {
		t.Error("the event does not record that this was a stream")
	}
	if ev.InputTokens != 30 || ev.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 30/2", ev.InputTokens, ev.OutputTokens)
	}
	if ev.Estimated {
		t.Error("a reported usage record must not be marked as an estimate")
	}
}

func TestMessagesSurfaceRefusesInItsOwnErrorShape(t *testing.T) {
	// A refusal a client cannot parse reaches a developer as a blank failure,
	// so the gateway's own refusals are in the Messages shape too.
	h := newHarness(t, jsonBackend(`{}`), nil, nil)

	resp := h.post(t, "/v1/messages",
		`{"model":"nope","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the refusal is not valid JSON: %v\n%s", err, body)
	}
	if out.Type != "error" || out.Error.Type != "not_found_error" {
		t.Errorf("refusal = %s, want a Messages error envelope naming not_found_error", body)
	}
	if out.Error.Message == "" {
		t.Error("the refusal carries no message; a developer has nothing to act on")
	}
	// A refusal is still a request that happened, and still the one a
	// developer asks about afterwards.
	if ev := h.sink.last(t); ev.Status != http.StatusNotFound {
		t.Errorf("the refusal was recorded with status %d, want 404", ev.Status)
	}
}

func TestMessagesSurfaceCannotReachANonChatModel(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), map[string]policy.Model{
		"keera-embed": {
			Alias: "keera-embed", Kind: policy.KindEmbedding,
			BackendModel: "embed", Enabled: true,
		},
	}, nil)

	resp := h.post(t, "/v1/messages",
		`{"model":"keera-embed","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404; an embedding model is not a conversation", resp.StatusCode)
	}
}

func TestMessagesSurfaceAcceptsTheApiKeyHeader(t *testing.T) {
	// Which header carries the credential is a client setting, not part of
	// the key. Picking the other one must not give a 401 nobody can explain.
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"},`+
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil, nil)

	req, err := http.NewRequest(http.MethodPost, h.url("/v1/messages"),
		strings.NewReader(`{"model":"keera-code","max_tokens":1,`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
}

func TestUnauthenticatedMessagesRequestIsRefusedInTheMessagesShape(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	resp := h.postWithKey(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "error" || out.Error.Type != "authentication_error" {
		t.Errorf("refusal = %s, want an authentication_error envelope", body)
	}
}

func TestMessagesSurfaceTranslatesAnUpstreamRefusal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"prompt is too long",`+
			`"type":"invalid_request_error","code":"context_length_exceeded"}}`)
	}, nil, nil)

	resp := h.post(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("body = %s, want a Messages error envelope", body)
	}
	// The wording is what a client matches on to recover from a refusal.
	if !strings.Contains(string(body), "prompt is too long") {
		t.Errorf("body = %s, want the upstream wording carried through", body)
	}
}

func TestConnectionWarmingProbeIsAnswered(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	req, err := http.NewRequest(http.MethodHead, h.url("/api/hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMessagesStreamPingsWhileTheUpstreamIsSilent(t *testing.T) {
	// A client aborts a stream that goes quiet, and the long quiet is before
	// the first token, while a big prompt is read. Pings fill that gap.
	restore := anthropicPingInterval
	anthropicPingInterval = 5 * time.Millisecond
	t.Cleanup(func() { anthropicPingInterval = restore })

	// A source that stalls before it says anything, then answers.
	src := io.MultiReader(
		&slowReader{delay: 60 * time.Millisecond},
		strings.NewReader(sseStream(`{"choices":[{"delta":{"content":"x"}}]}`, stopChunk)),
	)

	var buf syncBuffer
	if _, err := (anthropicShape{}).pipe(&buf, func() {}, src, "keera-code", true, DefaultMaxResponseBytes); err != nil {
		t.Fatalf("pipe: %v", err)
	}

	events := readEvents(t, buf.String())
	if len(events) == 0 || events[0].name != "ping" {
		t.Fatalf("the silence carried no ping; the client would see a dead connection:\n%v",
			names(events))
	}
	// The pings must not displace the turn: it still opens, delivers and closes.
	var turn []string
	for _, e := range events {
		if e.name != "ping" {
			turn = append(turn, e.name)
		}
	}
	want := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if !slices.Equal(turn, want) {
		t.Errorf("events without the pings = %v,\nwant %v", turn, want)
	}
}

// slowReader blocks once, the way an inference plane reading a long prompt
// does, and then reports EOF.
type slowReader struct {
	delay time.Duration
	done  bool
}

func (s *slowReader) Read([]byte) (int, error) {
	if !s.done {
		s.done = true
		time.Sleep(s.delay)
	}
	return 0, io.EOF
}

// syncBuffer is a bytes.Buffer the race detector is happy to see written by the
// pipe and read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMessagesStreamReadsAToolCallLargerThanTheReadBuffer(t *testing.T) {
	// A real tool call's arguments (a file, a patch) can be a line longer than
	// one read of the upstream. Reading part of it as a whole chunk would hand
	// the client broken JSON.
	args := `{"content":"` + strings.Repeat("a", 3*readBuffer) + `"}`
	quoted, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	events := pipeAs(t, anthropicShape{},
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1",`+
			`"function":{"name":"write_file","arguments":`+string(quoted)+`}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)

	var got strings.Builder
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		delta, ok := e.payload["delta"].(map[string]any)
		if !ok {
			t.Fatalf("content_block_delta carries no delta: %v", e.payload)
		}
		if partial, ok := delta["partial_json"].(string); ok {
			got.WriteString(partial)
		}
	}
	if got.String() != args {
		t.Errorf("the tool call's arguments arrived as %d bytes, want %d", len(got.String()), len(args))
	}
}

func TestMessagesStreamWritesNothingOnceTheClientIsGone(t *testing.T) {
	// Pings come from a timer, not from the goroutine reading the stream. A
	// ping after the pipe returned would write to a response the handler has
	// already finished with.
	restore := anthropicPingInterval
	anthropicPingInterval = time.Millisecond
	t.Cleanup(func() { anthropicPingInterval = restore })

	dst := &failingWriter{after: 2}
	_, err := (anthropicShape{}).pipe(dst, func() {},
		strings.NewReader(sseStream(deltaChunk, deltaChunk, deltaChunk)), "keera-code", true, DefaultMaxResponseBytes)
	if err == nil {
		t.Fatal("pipe reported success to a client that was gone")
	}
	written := dst.count()
	time.Sleep(20 * anthropicPingInterval)
	if dst.count() != written {
		t.Errorf("%d writes after the pipe returned, want none", dst.count()-written)
	}
}

// failingWriter fails every write after the first few, as a closed connection
// does.
type failingWriter struct {
	mu     sync.Mutex
	after  int
	writes int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes > w.after {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (w *failingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func TestMessagesStreamSendsArgumentsThatAreNotJSONAsEmpty(t *testing.T) {
	// Small models do write broken arguments. A buffered answer gets {} for
	// them, and so does a stream: an Anthropic SDK fails the whole answer on
	// arguments it cannot parse.
	events := pipeAs(t, anthropicShape{},
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1",`+
			`"function":{"name":"Bash","arguments":"{not "}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"json"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	for _, e := range events {
		if e.name == "content_block_delta" {
			t.Errorf("broken arguments reached the client: %v", e.payload)
		}
	}
}

func TestMessagesStreamCutOffSendsNoPartOfAToolCall(t *testing.T) {
	// A call cut short could still read as a command, a shorter one.
	var buf bytes.Buffer
	_, err := anthropicShape{}.pipe(&buf, func() {}, strings.NewReader(sseStream(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1",`+
			`"function":{"name":"Bash","arguments":"{\"command\":\"rm -rf /tmp/x"}}]}}]}`)),
		"keera-code", true, DefaultMaxResponseBytes)
	if err == nil {
		t.Fatal("a stream with no finish reason was taken as whole")
	}
	events := readEvents(t, buf.String())
	for _, e := range events {
		if e.name == "content_block_delta" {
			t.Errorf("part of a cut-off call reached the client: %v", e.payload)
		}
	}
	if last := events[len(events)-1].name; last != "error" {
		t.Errorf("the stream ends with %q, want error", last)
	}
}
