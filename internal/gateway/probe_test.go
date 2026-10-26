package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

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

// The failure the check exists for: a 200 with prose and empty `tool_calls`,
// the sign of a mismatched vLLM tool-call parser.
func TestCheckModelCatchesAToolCallReturnedAsText(t *testing.T) {
	upstream := httptest.NewServer(sseBackend(
		`{"model":"keera-code","choices":[{"delta":{"content":"<tool_call>{\"name\": \"get_build_status\""}}]}`,
		`{"model":"keera-code","choices":[{"delta":{"content":", \"arguments\": {}}</tool_call>"}}]}`))
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
	upstream := httptest.NewServer(sseBackend(`{"model":"keera-code","choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"function":{"name":"get_build_status","arguments":"{}"}}]}}]}`))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if !p.OK {
		t.Fatalf("check failed on a well-formed tool call: %s", p.Error)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", p.Warnings)
	}
}

// Prose with no tool call is not a parser problem: the model just will not
// drive an agent.
func TestCheckModelReportsAModelThatCallsNoTool(t *testing.T) {
	upstream := httptest.NewServer(sseBackend(`{"model":"keera-code","choices":[{"delta":{"content":"I cannot know that."}}]}`))
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

// vLLM without tool calling refuses every request with tools. Clients that
// send none still work, so the check passes with a note.
func TestCheckModelWarnsWhenTheBackendRefusesTools(t *testing.T) {
	plain := sseBackend(`{"model":"keera-code","choices":[{"delta":{"content":"Hello"}}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools"`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"\"auto\" tool choice requires `+
				`--enable-auto-tool-choice and --tool-call-parser to be set"}}`)
			return
		}
		plain(w, r)
	}))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if !p.OK {
		t.Fatalf("a backend that answers without tools should pass: %s", p.Error)
	}
	if !p.ToolsOff || p.ToolCalls {
		t.Errorf("tools_off=%v tool_calls=%v, want true and false", p.ToolsOff, p.ToolCalls)
	}
	if !anyContains(p.Warnings, "--enable-auto-tool-choice") {
		t.Errorf("warnings = %v, want one naming --enable-auto-tool-choice", p.Warnings)
	}
}

// A buffered answer is usable but wrong for an editor, so it passes with a
// note.
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
	if !anyContains(p.Warnings, "did not stream") {
		t.Errorf("warnings = %v, want one about streaming", p.Warnings)
	}
}

// A backend_model the plane does not serve fails silently: vLLM answers as
// whatever it serves, and only the reply says so.
func TestCheckModelWarnsWhenTheBackendServesAnotherModel(t *testing.T) {
	upstream := httptest.NewServer(sseBackend(`{"model":"qwen-2.5-coder","choices":[{"delta":{"tool_calls":` +
		`[{"index":0,"function":{"name":"get_build_status"}}]}}]}`))
	defer upstream.Close()

	p := probeServer(t).CheckModel(context.Background(), chatAlias(upstream.URL))
	if !p.OK {
		t.Fatalf("check failed: %s", p.Error)
	}
	if p.Served != "qwen-2.5-coder" {
		t.Errorf("served_model = %q, want the name the backend answered with", p.Served)
	}
	if !anyContains(p.Warnings, "served-model-name") {
		t.Errorf("warnings = %v, want one naming --served-model-name", p.Warnings)
	}
}

// A refusal from the plane is reported in its own words.
func TestCheckModelSurfacesTheBackendsOwnError(t *testing.T) {
	for _, kind := range []policy.Kind{policy.KindChat, policy.KindCompletion, policy.KindEmbedding} {
		for _, body := range []string{
			`{"error":{"message":"The model does not exist."}}`,
			`{"detail":"The model does not exist."}`,
		} {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, body)
			}))
			m := chatAlias(upstream.URL)
			m.Kind = kind
			p := probeServer(t).CheckModel(context.Background(), m)
			upstream.Close()
			switch {
			case p.OK:
				t.Fatalf("%s: a 404 from the backend must not pass", kind)
			case !p.Reachable:
				t.Errorf("%s: reachable = false; the backend answered, it just refused", kind)
			case p.Status != http.StatusNotFound:
				t.Errorf("%s: status = %d, want 404", kind, p.Status)
			case !strings.Contains(p.Error, "The model does not exist."):
				t.Errorf("%s, %s: error = %q, want the backend's own message", kind, body, p.Error)
			}
		}
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

// An embedding model passes when a vector comes back.
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
	// The check asks for no stream, so a missing one is not worth a warning.
	if len(p.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", p.Warnings)
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

// The check sends the model's own credential, as real traffic does.
func TestCheckModelPresentsTheModelCredential(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		sseBackend(`{"choices":[{"delta":{"tool_calls":[{"index":0,`+
			`"function":{"name":"get_build_status"}}]}}]}`)(w, r)
	}))
	defer upstream.Close()

	m := chatAlias(upstream.URL)
	m.APIKey = "sk-ant-upstream"
	srv := New(&fakeSource{}, &fakeBudgets{}, ratelimit.New(), &fakeSink{}, metrics.New(),
		Options{}, slog.New(slog.DiscardHandler))

	if p := srv.CheckModel(context.Background(), m); !p.OK {
		t.Fatalf("check failed: %s", p.Error)
	}
	if got := <-seen; got != "Bearer sk-ant-upstream" {
		t.Errorf("Authorization = %q, want the model's own credential", got)
	}
}

func TestSampleNeverCutsACharacterInHalf(t *testing.T) {
	// 399 bytes, then a two-byte character across the 400-byte cut.
	got := sample(strings.Repeat("a", 399) + "é and more")
	if !utf8.ValidString(got) {
		t.Fatalf("the sample is not valid UTF-8: %q", got[390:])
	}
	if want := strings.Repeat("a", 399) + "…"; got != want {
		t.Errorf("got %q, want the text cut before the character", got[390:])
	}
}
