package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// sseEvent is one event read back off a Messages stream.
type sseEvent struct {
	name    string
	payload map[string]any
}

// readMessagesStream parses a Messages stream into its events. It is
// deliberately strict about the framing - an event with no name or unparseable
// data is a stream a client's SDK would reject.
func readMessagesStream(t *testing.T, raw string) []sseEvent {
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

// pipeMessages runs a chat completion stream through the translation.
func pipeMessages(t *testing.T, chunks ...string) []sseEvent {
	t.Helper()
	var buf bytes.Buffer
	_, err := anthropicShape{}.pipe(&buf, func() {},
		strings.NewReader(sseStream(chunks...)), "keera-code", true)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	return readMessagesStream(t, buf.String())
}

func TestMessagesStreamOpensAndClosesTheMessage(t *testing.T) {
	// A chat completion stream is a series of deltas against one implicit
	// message; a Messages stream is an event protocol with an explicit start
	// and end. A client waits for message_stop before it considers the turn
	// over, so every stream has to produce the full envelope.
	events := pipeMessages(t,
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
	if got := names(events); !equalStrings(got, want) {
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
	// The gateway asks the inference plane for a usage record the client never
	// requested. In this shape there is nowhere to forward it to: the numbers
	// belong in message_delta, and a stray chunk with an empty choices array
	// is not an event the protocol has.
	events := pipeMessages(t,
		`{"choices":[{"delta":{"content":"x"}}]}`,
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
	// Tool arguments arrive as fragments of a JSON string and leave as
	// input_json_delta fragments against an open tool_use block. The block has
	// to open before the first fragment and close before the message ends, or
	// the client has nowhere to accumulate them.
	events := pipeMessages(t,
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
		"content_block_start", "content_block_delta", "content_block_delta", // tool
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := names(events); !equalStrings(got, want) {
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
	for _, e := range events[5:7] {
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
	events := pipeMessages(t,
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
	if !equalStrings(ids, []string{"call_1", "call_2"}) {
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

func TestMessagesStreamClosesATruncatedStream(t *testing.T) {
	// The inference plane died mid-generation. A client left waiting for
	// message_stop hangs on a turn that is already over, which is worse than a
	// turn that ended short.
	var buf bytes.Buffer
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n"
	if _, err := (anthropicShape{}).pipe(&buf, func() {},
		strings.NewReader(partial), "keera-code", true); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	events := readMessagesStream(t, buf.String())
	last := events[len(events)-1]
	if last.name != "message_stop" {
		t.Errorf("the stream ends with %q, want message_stop", last.name)
	}
}

func TestMessagesStreamIsWellFormedWithNoContentAtAll(t *testing.T) {
	// An upstream that answered with an empty stream still has to produce a
	// message a client can parse and discard.
	events := pipeMessages(t)
	if got := names(events); !equalStrings(got, []string{"message_start", "message_delta", "message_stop"}) {
		t.Errorf("events = %v, want an empty but complete message", got)
	}
}

func TestMessagesStreamReportsUsageForBilling(t *testing.T) {
	var buf bytes.Buffer
	stats, err := anthropicShape{}.pipe(&buf, func() {}, strings.NewReader(sseStream(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":2,"total_tokens":42}}`,
	)), "keera-code", true)
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

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ------------------------------------------------------- through the gateway

func TestMessagesSurfaceEnforcesTheSamePolicyAsChat(t *testing.T) {
	// The point of translating rather than proxying: an Anthropic-shaped
	// client passes the guardrail, the output ceiling and the accounting that
	// every other client passes. A surface that went round them would be a
	// hole in the only thing this product enforces.
	res := policy.Resolve(
		policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&policy.Limits{
			SystemPrompt:    func() *string { p := "obey the org"; return &p }(),
			MaxOutputTokens: func() *int { n := 64; return &n }(),
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
	if sent.MaxTokens != 64 {
		t.Errorf("max_tokens = %d, want the guardrail ceiling of 64", sent.MaxTokens)
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
	events := readMessagesStream(t, string(body))
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
	// A refusal a client cannot parse reaches a developer as a blank failure.
	// The Messages shape names an error differently from the OpenAI one, so
	// every refusal on this surface has to be translated too - including the
	// ones the gateway generates before it ever reaches the inference plane.
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
	// Which header a client sends its credential in is a configuration
	// setting, not a property of the key. A developer who picked the other
	// variable would otherwise get a 401 whose cause is invisible from inside
	// their editor.
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
	// A client aborts a stream that goes quiet, and the quiet stretch that
	// matters is before the first token - a long prompt is still being read and
	// there is nothing to forward. Without a ping in that gap a large context
	// would look like a dead connection.
	restore := anthropicPingInterval
	anthropicPingInterval = 5 * time.Millisecond
	t.Cleanup(func() { anthropicPingInterval = restore })

	// A source that stalls before it says anything, then answers.
	src := io.MultiReader(
		&slowReader{delay: 60 * time.Millisecond},
		strings.NewReader(sseStream(`{"choices":[{"delta":{"content":"x"}}]}`)),
	)

	var buf syncBuffer
	if _, err := (anthropicShape{}).pipe(&buf, func() {}, src, "keera-code", true); err != nil {
		t.Fatalf("pipe: %v", err)
	}

	events := readMessagesStream(t, buf.String())
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
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if !equalStrings(turn, want) {
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
	// The arguments of a real tool call - a file to write, a patch to apply -
	// arrive as a line longer than one read of the upstream stream. Reading
	// part of one as a whole chunk would hand the client a fragment of JSON to
	// assemble, and the tool call would fail inside the agent rather than here.
	args := strings.Repeat("a", 3*readBuffer)
	quoted, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	events := pipeMessages(t,
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
