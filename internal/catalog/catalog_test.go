package catalog

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestParseAcceptsACompleteCatalogue(t *testing.T) {
	const in = `
models:
  - alias: keera-code
    kind: chat
    backends: ["http://keera-code:8000/v1"]
    backend_model: keera-code
    max_context: 65536
    input_micros_per_mtok: 500000
    output_micros_per_mtok: 2000000
  - alias: keera-embed
    kind: embedding
    backends: ["http://keera-embed:8000/v1"]
    backend_model: keera-embed
  - alias: keera-legacy
    backends: ["http://old:8000/v1"]
    backend_model: old
    disabled: true
`
	models, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3", len(models))
	}
	if models[0].Kind != policy.KindChat || models[0].MaxContext != 65536 {
		t.Errorf("models[0] = %+v", models[0])
	}
	if models[1].Kind != policy.KindEmbedding {
		t.Errorf("models[1].Kind = %q, want embedding", models[1].Kind)
	}
	// An unstated kind is a chat model, which is what almost every entry is.
	if models[2].Kind != policy.KindChat {
		t.Errorf("models[2].Kind = %q, want chat by default", models[2].Kind)
	}
	if models[2].Enabled {
		t.Error("a disabled entry came back enabled")
	}
	if !models[0].Enabled {
		t.Error("an entry that says nothing about being disabled must be enabled")
	}
}

func TestParseRejectsWhatWouldFailSilentlyAtRuntime(t *testing.T) {
	tests := []struct {
		name, in, wantErr string
	}{
		{
			name:    "no models",
			in:      "models: []",
			wantErr: "no models",
		},
		{
			name:    "no alias",
			in:      "models:\n  - backends: [\"http://x/v1\"]\n    backend_model: m",
			wantErr: "alias is required",
		},
		{
			name:    "no backend",
			in:      "models:\n  - alias: a\n    backend_model: m",
			wantErr: "backend is required",
		},
		{
			// Forgetting this is the mistake that makes vLLM answer 404 for
			// every request, so it is worth naming --served-model-name in the
			// error rather than saying "required".
			name:    "no backend model",
			in:      "models:\n  - alias: a\n    backends: [\"http://x/v1\"]",
			wantErr: "served-model-name",
		},
		{
			name:    "a duplicate alias",
			in:      "models:\n  - alias: a\n    backends: [\"http://x/v1\"]\n    backend_model: m\n  - alias: a\n    backends: [\"http://y/v1\"]\n    backend_model: n",
			wantErr: "declared twice",
		},
		{
			// The alias reaches a developer's client config unquoted, so a
			// name that could close a JSON string or a shell word is refused
			// where it is declared rather than where it is rendered.
			name:    "an alias that would break a client config",
			in:      "models:\n  - alias: \"a\\\", \\\"x\\\": \\\"\"\n    backends: [\"http://x/v1\"]\n    backend_model: m",
			wantErr: "lowercase letters",
		},
		{
			name:    "an unknown kind",
			in:      "models:\n  - alias: a\n    kind: rerank\n    backends: [\"http://x/v1\"]\n    backend_model: m",
			wantErr: "unknown kind",
		},
		{
			name:    "not YAML at all",
			in:      "models: [[[",
			wantErr: "parse catalogue",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			if err == nil {
				t.Fatal("Parse accepted a catalogue that would fail at runtime")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}
