package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
)

// The benchmarks send what a coding agent sends late in a session: about
// 600 KB of conversation and 30 tools, answered by a 300-event stream.

const benchTurns = 30

// benchCode is about 14 KB of source, read once per turn.
func benchCode(turn int) string {
	var b strings.Builder
	for i := range 120 {
		fmt.Fprintf(&b, "func handler%d_%d(w http.ResponseWriter, r *http.Request) {\n"+
			"\tif err := do(r.Context(), \"value\"); err != nil {\n\t\thttp.Error(w, err.Error(), 500)\n\t}\n}\n",
			turn, i)
	}
	return b.String()
}

func benchTools(anthropic bool) []any {
	tools := make([]any, 0, 30)
	for i := range 30 {
		name, desc := fmt.Sprintf("tool_%d", i), strings.Repeat("does a thing. ", 20)
		schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
		if anthropic {
			tools = append(tools, map[string]any{"name": name, "description": desc, "input_schema": schema})
		} else {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": name, "description": desc, "parameters": schema,
			}})
		}
	}
	return tools
}

func benchChat(b *testing.B) []byte {
	msgs := []map[string]any{{"role": "system", "content": "You are a coding agent."}}
	for i := range benchTurns {
		msgs = append(msgs, map[string]any{"role": "user", "content": benchCode(i)},
			map[string]any{"role": "assistant", "content": "ok"})
	}
	return benchJSON(b, map[string]any{
		"model": "keera-code", "messages": msgs, "tools": benchTools(false), "stream": true,
	})
}

func benchMessages(b *testing.B) []byte {
	msgs := []map[string]any{}
	for i := range benchTurns {
		msgs = append(msgs, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": benchCode(i)},
		}}, map[string]any{"role": "assistant", "content": "ok"})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "go on"})
	return benchJSON(b, map[string]any{
		"model": "keera-code", "system": "You are a coding agent.", "max_tokens": 1000,
		"messages": msgs, "tools": benchTools(true), "stream": true,
	})
}

func benchJSON(b *testing.B, v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

// benchRules are what a deployment guards against: keys and card numbers.
var benchRules = []policy.FilterRule{
	{Pattern: `(?i)\bsk-[a-z0-9]{8,}\b`, Replace: "[CREDENTIAL]"},
	{Pattern: `\b\d{4}[ -]?\d{4}[ -]?\d{4}[ -]?\d{4}\b`, Replace: "[CARD]"},
}

func benchGateway(b *testing.B, filters ...string) http.Handler {
	chunks := make([]string, 0, 302)
	for range 300 {
		chunks = append(chunks, deltaChunk)
	}
	stream := sseStream(append(chunks, stopChunk, usageChunk)...)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	b.Cleanup(backend.Close)

	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", ProjectID: "project_1"}, nil, nil, nil)
	resolved.Filters = filters
	src := &fakeSource{
		resolved: map[string]*policy.Resolved{testKey: resolved},
		models: map[string]policy.Model{"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, Enabled: true,
			Backends: []string{backend.URL + "/v1"}, BackendModel: "served-name",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000, MaxContext: 1 << 20,
		}},
		filters: map[string]policy.Filter{"org_1/redact": patternFilter(benchRules...)},
	}
	return New(src, &fakeBudgets{}, ratelimit.New(), &fakeSink{}, metrics.New(), Options{},
		slog.New(slog.DiscardHandler)).Handler()
}

func benchServe(b *testing.B, h http.Handler, path string, body []byte) {
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPost, httpx.InferencePrefix+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			b.Fatalf("status %d: %s", w.Code, w.Body)
		}
	}
}

func BenchmarkChat(b *testing.B) {
	benchServe(b, benchGateway(b), "/v1/chat/completions", benchChat(b))
}

// BenchmarkChatPatternFilter resends the same conversation, as an agent does
// every turn, so it measures segments remembered clean.
func BenchmarkChatPatternFilter(b *testing.B) {
	benchServe(b, benchGateway(b, "redact"), "/v1/chat/completions", benchChat(b))
}

// BenchmarkPatternFirstScan is what the rules cost on text never seen before.
func BenchmarkPatternFirstScan(b *testing.B) {
	c := compileRules(benchRules)
	texts := make([]string, benchTurns)
	n := 0
	for i := range texts {
		texts[i] = benchCode(i)
		n += len(texts[i])
	}
	b.SetBytes(int64(n))
	for b.Loop() {
		if _, _, err := runPattern(c, texts); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMessages(b *testing.B) {
	benchServe(b, benchGateway(b), "/v1/messages", benchMessages(b))
}
