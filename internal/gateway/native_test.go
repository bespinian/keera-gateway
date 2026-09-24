package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// seenRequest is what a backend was sent.
type seenRequest struct {
	path   string
	header http.Header
	body   string
}

// providerHarness is a gateway with a hosted model, keera-frontier, declared
// against provider, next to the local keera-code. Both are served by backend,
// and every request it receives is put on the returned channel.
func providerHarness(t *testing.T, provider, backendModel string, backend http.HandlerFunc,
	res *policy.Resolved,
) (*harness, chan seenRequest) {
	t.Helper()
	seen := make(chan seenRequest, 8)
	record := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen <- seenRequest{path: r.URL.Path, header: r.Header.Clone(), body: string(raw)}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		backend(w, r)
	}
	models := map[string]policy.Model{
		"keera-frontier": {
			Alias: "keera-frontier", Kind: policy.KindChat, Provider: provider,
			BackendModel: backendModel, APIKey: "sk-upstream", Enabled: true,
			// 1 micro per input token, a tenth of that cached, 4 per output token.
			InputMicrosPerMTok: 1_000_000, CachedInputMicrosPerMTok: 100_000,
			OutputMicrosPerMTok: 4_000_000, MaxContext: 200_000,
		},
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name",
			Enabled: true, InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000,
		},
	}
	return newHarness(t, record, models, res), seen
}

// nextSeen is the next request the backend received. It fails the test rather
// than hang when none comes.
func nextSeen(t *testing.T, seen chan seenRequest) seenRequest {
	t.Helper()
	select {
	case r := <-seen:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the backend received no request")
		return seenRequest{}
	}
}

// guarded is a key with a standing system prompt and an output ceiling.
func guarded(prompt string, ceiling int) *policy.Resolved {
	return policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&policy.Limits{SystemPrompt: &prompt, MaxOutputTokens: &ceiling}, nil, nil)
}

const anthropicAnswer = `{"id":"msg_1","type":"message","role":"assistant",` +
	`"model":"claude-opus-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":100,"cache_read_input_tokens":900,` +
	`"cache_creation_input_tokens":0,"output_tokens":50}}`

func TestMessagesGoToAnthropicInTheirOwnAPI(t *testing.T) {
	// Translating for Anthropic loses prompt caching and thinking, and on a
	// coding agent's session the cache is most of the bill. So the request goes
	// as it came, with the fields the translation would have dropped.
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer), nil)

	req, _ := http.NewRequest(http.MethodPost, h.url("/v1/messages"), strings.NewReader(
		`{"model":"keera-frontier","max_tokens":1024,"thinking":{"type":"adaptive"},`+
			`"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	got := nextSeen(t, seen)
	if got.path != "/v1/messages" {
		t.Errorf("forwarded to %s, want Anthropic's own /v1/messages", got.path)
	}
	for _, kept := range []string{`"cache_control"`, `"thinking"`, `"claude-opus-5"`} {
		if !strings.Contains(got.body, kept) {
			t.Errorf("the forwarded body lost %s: %s", kept, got.body)
		}
	}
	if got.header.Get("X-Api-Key") != "sk-upstream" || got.header.Get("Authorization") != "" {
		t.Errorf("credential sent as x-api-key=%q authorization=%q, want only x-api-key",
			got.header.Get("X-Api-Key"), got.header.Get("Authorization"))
	}
	if got.header.Get("Anthropic-Version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want the default", got.header.Get("Anthropic-Version"))
	}
	if got.header.Get("Anthropic-Beta") != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q, want the client's flags passed on",
			got.header.Get("Anthropic-Beta"))
	}

	var answer struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Model != "keera-frontier" {
		t.Errorf("answer names model %q, want the alias: %s", answer.Model, body)
	}

	// Anthropic counts cached tokens apart from the input; the row counts them
	// inside it, and charges them at the cached rate: 100 + 900/10 + 50*4.
	ev := h.sink.last(t)
	if ev.InputTokens != 1000 || ev.CachedInputTokens != 900 || ev.OutputTokens != 50 {
		t.Errorf("tokens = %d in (%d cached), %d out; want 1000 (900), 50",
			ev.InputTokens, ev.CachedInputTokens, ev.OutputTokens)
	}
	if ev.CostMicros != 390 {
		t.Errorf("cost = %d, want 390", ev.CostMicros)
	}
}

func TestNativeMessagesStillPassTheGuardrails(t *testing.T) {
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer),
		guarded("obey the org", 2048))

	resp := h.post(t, "/v1/messages",
		`{"model":"keera-frontier","max_tokens":32000,`+
			`"thinking":{"type":"enabled","budget_tokens":16000},`+
			`"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var sent struct {
		MaxTokens int `json:"max_tokens"`
		Thinking  struct {
			BudgetTokens int `json:"budget_tokens"`
		} `json:"thinking"`
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(nextSeen(t, seen).body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want the ceiling of 2048", sent.MaxTokens)
	}
	// A budget at or above max_tokens is refused by Anthropic.
	if sent.Thinking.BudgetTokens >= 2048 {
		t.Errorf("thinking budget = %d, want it below the ceiling", sent.Thinking.BudgetTokens)
	}
	if len(sent.System) != 2 || sent.System[0].Text != "obey the org" {
		t.Fatalf("system = %+v, want the guardrail first", sent.System)
	}
	if sent.System[1].Text != "be brief" || len(sent.System[1].CacheControl) == 0 {
		t.Errorf("system[1] = %+v, want the client's block with its cache marker", sent.System[1])
	}
}

func TestAFilterRewritesANativeRequestInItsOwnShape(t *testing.T) {
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(anthropicAnswer), nil)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilterFor("org_1", "redact",
			policy.FilterRule{Pattern: `\bhunter2\b`, Replace: "[CREDENTIAL]"}),
	}
	h.src.resolved[testKey].Filters = []string{"redact"}

	resp := h.post(t, "/v1/messages",
		`{"model":"keera-frontier","max_tokens":64,"system":"the password is hunter2",`+
			`"messages":[{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"t1","content":"found hunter2 in .env"},`+
			`{"type":"text","text":"is hunter2 safe?","cache_control":{"type":"ephemeral"}}]}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	got := nextSeen(t, seen)
	if strings.Contains(got.body, "hunter2") {
		t.Errorf("the secret reached Anthropic: %s", got.body)
	}
	if strings.Count(got.body, "[CREDENTIAL]") != 3 {
		t.Errorf("want the system prompt, the tool result and the text redacted: %s", got.body)
	}
	if !strings.Contains(got.body, `"cache_control"`) {
		t.Errorf("the rewrite dropped the client's cache marker: %s", got.body)
	}
}

func TestNativeMessagesStreamPassesThrough(t *testing.T) {
	events := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1",` +
			`"model":"claude-opus-5","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":1}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,` +
			`"content_block":{"type":"thinking","thinking":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"thinking_delta","thinking":"hm"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"text_delta","text":"hi"}}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
			`"usage":{"output_tokens":20}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e+"\n\n")
		}
	}
	h, _ := providerHarness(t, "anthropic", "claude-opus-5", backend, nil)

	resp := h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":64,"stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	// The thinking block is Anthropic's own and reaches the client untouched.
	if !strings.Contains(out, `"thinking_delta"`) {
		t.Errorf("the thinking block was lost: %s", out)
	}
	if strings.Contains(out, "claude-opus-5") || !strings.Contains(out, `"model":"keera-frontier"`) {
		t.Errorf("the stream names the backend model rather than the alias: %s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Errorf("the stream was not forwarded to its end: %s", out)
	}

	ev := h.sink.last(t)
	if ev.InputTokens != 100 || ev.CachedInputTokens != 90 || ev.OutputTokens != 20 || ev.Estimated {
		t.Errorf("tokens = %d in (%d cached), %d out, estimated %v; want 100 (90), 20, reported",
			ev.InputTokens, ev.CachedInputTokens, ev.OutputTokens, ev.Estimated)
	}
}

func TestANativeStreamCutShortKeepsItsInputCount(t *testing.T) {
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"model":"m",`+
			`"usage":{"input_tokens":400,"output_tokens":1}}}`+"\n\n")
		for range 3 {
			_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,`+
				`"delta":{"type":"text_delta","text":"x"}}`+"\n\n")
		}
	}
	h, _ := providerHarness(t, "anthropic", "claude-opus-5", backend, nil)

	resp := h.post(t, "/v1/messages", `{"model":"keera-frontier","max_tokens":64,"stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	_, _ = io.ReadAll(resp.Body)

	ev := h.sink.last(t)
	if ev.InputTokens != 400 || ev.OutputTokens != 3 || !ev.Estimated {
		t.Errorf("tokens = %d in, %d out, estimated %v; want the exact 400 in, 3 out counted "+
			"from the deltas, and the row marked as an estimate",
			ev.InputTokens, ev.OutputTokens, ev.Estimated)
	}
}

func TestAFallbackFromAnthropicToALocalModelIsTranslated(t *testing.T) {
	backend := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonBackend(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)(w, r)
	}
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", backend, guarded("obey the org", 256))
	h.src.routers = map[string]policy.Router{"org_1/ha": {
		OrgID: "org_1", Alias: "ha", Mode: policy.RouterModeFallback,
		Destinations: []string{"keera-frontier", "keera-code"},
	}}

	resp := h.post(t, "/v1/messages",
		`{"model":"ha","max_tokens":1024,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the local model: %s", resp.StatusCode, body)
	}
	if first := nextSeen(t, seen); first.path != "/v1/messages" {
		t.Errorf("first attempt went to %s, want Anthropic's own API", first.path)
	}
	second := nextSeen(t, seen)
	if second.path != "/v1/chat/completions" || !strings.Contains(second.body, "obey the org") {
		t.Errorf("the local model was sent %s %s, want a translated body with the guardrail",
			second.path, second.body)
	}
	if !strings.Contains(string(body), `"type":"message"`) {
		t.Errorf("the local answer was not translated back: %s", body)
	}
}

func TestCountTokensIsAnsweredWithoutForwarding(t *testing.T) {
	// Forwarding would send the prompt to Anthropic past its filters.
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", jsonBackend(`{}`), nil)

	resp := h.post(t, "/v1/messages/count_tokens",
		`{"model":"keera-frontier","messages":[{"role":"user","content":"`+strings.Repeat("a", 3000)+`"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.InputTokens != 1000 {
		t.Errorf("input_tokens = %d, want 1000: %s", out.InputTokens, body)
	}
	if len(seen) != 0 {
		t.Error("count_tokens reached a backend")
	}

	resp = h.post(t, "/v1/messages/count_tokens", `{"model":"nope","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status for an unknown model = %d, want 404", resp.StatusCode)
	}
}
