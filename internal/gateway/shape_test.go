package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An OpenAI SDK reads only error.message, so an upstream error in any other
// envelope reaches the client wrapped, and one already in it is left alone.
func TestOpenAIErrorBody(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		status  int
		message string
		typ     string
	}{
		{`{"error":{"message":"bad","type":"invalid_request_error","code":"x"}}`, 400, "bad", "invalid_request_error"},
		{`{"detail":"The model does not exist."}`, 404, "The model does not exist.", "invalid_request_error"},
		{`{"message":"too long","object":"error"}`, 400, "too long", "invalid_request_error"},
		{`upstream connect error`, http.StatusBadGateway, "upstream connect error", "server_error"},
		// Typed as the gateway types its own refusals, so a client retries a
		// rate limit whichever side set it.
		{`{"detail":"slow down"}`, http.StatusTooManyRequests, "slow down", "rate_limit_error"},
		{`{"detail":"not for you"}`, http.StatusForbidden, "not for you", "permission_error"},
	} {
		var got struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(openAIErrorBody([]byte(tc.raw), tc.status), &got); err != nil {
			t.Fatalf("%s: not JSON: %v", tc.raw, err)
		}
		if got.Error.Message != tc.message || got.Error.Type != tc.typ {
			t.Errorf("%s: got %q (%s), want %q (%s)", tc.raw, got.Error.Message, got.Error.Type, tc.message, tc.typ)
		}
	}
}

// Codex and the OpenAI SDKs know a context overflow by its code, which
// neither vLLM nor llama.cpp sends.
func TestABackendContextOverflowGetsOpenAIsCode(t *testing.T) {
	for _, tc := range []struct{ raw, message string }{
		{
			`{"object":"error","message":"This model's maximum context length is 16384 tokens. However, ` +
				`you requested 20000 tokens (12000 in the messages, 8000 in the completion).","type":"BadRequestError","code":400}`,
			"input length and `max_tokens` exceed context limit: 12000 + 8000 > 16384, decrease input length or `max_tokens` and try again",
		},
		{
			`{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it",` +
				`"type":"exceed_context_size_error","n_prompt_tokens":20000,"n_ctx":16384}}`,
			"prompt is too long: 20000 tokens > 16384 maximum",
		},
	} {
		var got struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(openAIErrorBody([]byte(tc.raw), http.StatusBadRequest), &got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Code != "context_length_exceeded" || got.Error.Message != tc.message {
			t.Errorf("%s: got %q (%s), want %q (context_length_exceeded)", tc.raw, got.Error.Message,
				got.Error.Code, tc.message)
		}
	}
	// OpenAI's own already has it, and passes as it came.
	own := `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`
	if got := string(openAIErrorBody([]byte(own), http.StatusBadRequest)); got != own {
		t.Errorf("OpenAI's own overflow was rewritten: %s", got)
	}
}
