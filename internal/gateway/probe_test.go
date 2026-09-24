package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
)

func probeServer(t *testing.T) *Server {
	t.Helper()
	return New(&fakeSource{}, &fakeBudgets{}, ratelimit.New(), &fakeSink{}, metrics.New(),
		Options{}, slog.New(slog.DiscardHandler))
}

func chatAlias(url string) policy.Model {
	return policy.Model{
		Alias: "keera-code", Kind: policy.KindChat,
		Backends: []string{url + "/v1"}, BackendModel: "keera-code", Enabled: true,
	}
}

// sse writes a server-sent event stream of the given data payloads.
func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, c := range chunks {
		_, _ = io.WriteString(w, "data: "+c+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// This is the failure the check exists for. The endpoint answers 200, the model
// produces prose, and `tool_calls` stays empty - which is what a vLLM tool-call
// parser that does not match the model looks like. Nothing logs an error and it
// reads to developers as "the model is bad", so a check that called this
// healthy would be worse than no check at all.
func TestCheckModelCatchesAToolCallReturnedAsText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`{"model":"keera-code","choices":[{"delta":{"content":"<tool_call>{\"name\": \"get_build_status\""}}]}`,
			`{"model":"keera-code","choices":[{"delta":{"content":", \"arguments\": {}}</tool_call>"}}]}`)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if p.OK {
		t.Fatal("a tool call returned as text must not pass the check")
	}
	if !p.ToolCallAsText {
		t.Errorf("tool_call_as_text = false, want true (error: %q)", p.Error)
	}
	if !strings.Contains(p.Error, "tool-call parser") {
		t.Errorf("the error should name the parser as the thing to fix, got %q", p.Error)
	}
	if !p.Reachable || !p.Streamed {
		t.Errorf("reachable=%v streamed=%v, want both true", p.Reachable, p.Streamed)
	}
}

func TestCheckModelPassesOnARealToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"model":"keera-code","choices":[{"delta":{"tool_calls":[`+
			`{"index":0,"function":{"name":"get_build_status","arguments":"{}"}}]}}]}`)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if !p.OK {
		t.Fatalf("check failed on a well-formed tool call: %s", p.Error)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", p.Warnings)
	}
}

// Answering with prose and no tool call at all is a different diagnosis from a
// mis-parsed one, and has to read differently: nothing here is misconfigured,
// the model simply will not drive an agent.
func TestCheckModelReportsAModelThatCallsNoTool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"model":"keera-code","choices":[{"delta":{"content":"I cannot know that."}}]}`)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	switch {
	case p.OK:
		t.Fatal("a model that called no tool must not pass")
	case p.ToolCallAsText:
		t.Error("nothing was mis-parsed here; this is not a parser problem")
	case !strings.Contains(p.Error, "called no tool"):
		t.Errorf("error = %q, want it to say no tool was called", p.Error)
	}
}

// A buffered answer is usable but wrong for an editor, so it passes with a note
// rather than failing. The distinction matters: an operator who is told this is
// broken will go looking for a fault that is not there.
func TestCheckModelWarnsWhenTheBackendDoesNotStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"keera-code","choices":[{"message":{"tool_calls":`+
			`[{"function":{"name":"get_build_status"}}],"content":""}}]}`)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if !p.OK {
		t.Fatalf("a buffered but correct answer should pass: %s", p.Error)
	}
	if p.Streamed {
		t.Error("streamed = true for a JSON response")
	}
	if !hasWarning(p.Warnings, "did not stream") {
		t.Errorf("warnings = %v, want one about streaming", p.Warnings)
	}
}

// A model whose backend_model names something the inference plane does not
// serve is the other silent misconfiguration: vLLM answers as whatever it does
// serve, and only the reply says so.
func TestCheckModelWarnsWhenTheBackendServesAnotherModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"model":"qwen-2.5-coder","choices":[{"delta":{"tool_calls":`+
			`[{"index":0,"function":{"name":"get_build_status"}}]}}]}`)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if !p.OK {
		t.Fatalf("check failed: %s", p.Error)
	}
	if p.Served != "qwen-2.5-coder" {
		t.Errorf("served_model = %q, want the name the backend answered with", p.Served)
	}
	if !hasWarning(p.Warnings, "served-model-name") {
		t.Errorf("warnings = %v, want one naming --served-model-name", p.Warnings)
	}
}

// A refusal from the inference plane has to arrive as what the inference plane
// said, not as "check failed".
func TestCheckModelSurfacesTheBackendsOwnError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"The model does not exist."}}`)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	switch {
	case p.OK:
		t.Fatal("a 404 from the backend must not pass")
	case !p.Reachable:
		t.Error("reachable = false; the backend answered, it just refused")
	case p.Status != http.StatusNotFound:
		t.Errorf("status = %d, want 404", p.Status)
	case !strings.Contains(p.Error, "The model does not exist."):
		t.Errorf("error = %q, want the backend's own message", p.Error)
	}
}

func TestCheckModelReportsABackendThatCannotBeReached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := upstream.URL
	upstream.Close() // nothing listens here now

	p := probeServer(t).CheckModel(context.Background(), chatAlias(url))
	if p.OK || p.Reachable {
		t.Fatal("a closed backend must be reported as unreachable")
	}
	if !strings.Contains(p.Error, "could not be reached") {
		t.Errorf("error = %q", p.Error)
	}
}

func TestCheckModelRefusesAModelWithNoBackend(t *testing.T) {
	m := policy.Model{Alias: "keera-code", Kind: policy.KindChat, Enabled: true}
	p := probeServer(t).CheckModel(context.Background(), m)
	if p.OK || p.Reachable {
		t.Fatal("a model with no backend cannot be healthy")
	}
	if !strings.Contains(p.Error, "no backend") {
		t.Errorf("error = %q", p.Error)
	}
}

// An embedding model has no tools to call, so it is checked on the only thing
// that matters about one: that a vector comes back.
func TestCheckModelChecksAnEmbeddingModelOnItsOwnTerms(t *testing.T) {
	var path string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"bge-m3","data":[{"embedding":[0.1,0.2,0.3]}]}`)
	}))
	defer upstream.Close()

	m := chatAlias(upstream.URL)
	m.Kind = policy.KindEmbedding
	m.BackendModel = "bge-m3"
	p := probeServer(t).CheckModel(context.Background(), m)
	if !p.OK {
		t.Fatalf("check failed: %s", p.Error)
	}
	if path != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", path)
	}
	if p.Sample != "3 dimensions" {
		t.Errorf("sample = %q, want the vector's size", p.Sample)
	}
}

func TestCheckModelFailsAnEmbeddingModelThatReturnsNoVector(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"bge-m3","data":[]}`)
	}))
	defer upstream.Close()

	m := chatAlias(upstream.URL)
	m.Kind = policy.KindEmbedding
	if p := probeServer(t).CheckModel(context.Background(), m); p.OK {
		t.Fatal("200 with no embedding must not pass")
	}
}

// The check presents the model's own credential, exactly as the data plane
// would. A check that reached the backend unauthenticated would go green
// against an endpoint every real request then gets a 401 from.
func TestCheckModelPresentsTheModelCredential(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		sse(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,`+
			`"function":{"name":"get_build_status"}}]}}]}`)
	}))
	defer upstream.Close()

	m := chatAlias(upstream.URL)
	m.APIKeyEnv = "ANTHROPIC_API_KEY"
	srv := New(&fakeSource{}, &fakeBudgets{}, ratelimit.New(), &fakeSink{}, metrics.New(),
		Options{APIKeys: func(string) string { return "sk-ant-upstream" }},
		slog.New(slog.DiscardHandler))

	if p := srv.CheckModel(context.Background(), m); !p.OK {
		t.Fatalf("check failed: %s", p.Error)
	}
	if got := <-seen; got != "Bearer sk-ant-upstream" {
		t.Errorf("Authorization = %q, want the model's own credential", got)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
