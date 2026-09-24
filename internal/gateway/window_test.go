package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// windowHarness serves keera-code with a context of 1,000 tokens.
func windowHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":1,"completion_tokens":1}}`), map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name",
			MaxContext: 1000, Enabled: true,
		},
		"keera-embed": {
			Alias: "keera-embed", Kind: policy.KindEmbedding, BackendModel: "embed-served",
			MaxContext: 1000, Enabled: true,
		},
	}, nil)
}

func TestARequestTooLongForTheModelIsRefusedBeforeItIsSent(t *testing.T) {
	h := windowHarness(t)
	long := strings.Repeat("a", 6000) // at least 1,200 tokens

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"`+long+`"}]}`)
	body := decodedBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "context_length_exceeded") ||
		!strings.Contains(body, "prompt is too long: 1200 tokens > 1000 maximum") {
		t.Errorf("body = %s, want the code and the wording clients act on", body)
	}
	if len(h.upstreamBodies) != 0 {
		t.Error("the request reached the backend")
	}

	// Claude Code compacts on this sentence, so it survives the Messages shape.
	resp = h.post(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":10,"messages":[{"role":"user","content":"`+long+`"}]}`)
	body = decodedBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "prompt is too long: 1200 tokens > 1000 maximum") {
		t.Errorf("messages: status = %d: %s", resp.StatusCode, body)
	}
}

// decodedBody is the code and the message of an error answer, in either
// shape, as a client reads them.
func decodedBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	var answer struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("answer is not JSON: %s", raw)
	}
	return answer.Error.Code + " " + answer.Error.Message
}

func TestARequestThatMayFitIsForwarded(t *testing.T) {
	h := windowHarness(t)
	// 4,000 bytes can be up to 1,333 tokens, but need not be more than 800: the
	// backend decides.
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"`+strings.Repeat("a", 4000)+`"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the request forwarded", resp.StatusCode)
	}
}

func TestEachEmbeddingInputFitsOnItsOwn(t *testing.T) {
	h := windowHarness(t)
	input := `"` + strings.Repeat("a", 4000) + `"`
	resp := h.post(t, "/v1/embeddings",
		`{"model":"keera-embed","input":[`+input+`,`+input+`,`+input+`]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want a batch of inputs that each fit forwarded", resp.StatusCode)
	}
}

func TestAModelWithNoDeclaredContextTakesAnything(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}]}`),
		map[string]policy.Model{"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name", Enabled: true,
		}}, nil)
	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"`+strings.Repeat("a", 60000)+`"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
