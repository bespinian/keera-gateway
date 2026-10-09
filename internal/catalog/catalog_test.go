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
	models, err := parseModelFile([]byte(in))
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
			// The control API refuses it too, so a file cannot seed an
			// organisation with a model nobody could save again.
			name:    "a backend that is not http",
			in:      "models:\n  - alias: a\n    backends: [\"file:///etc/passwd\"]\n    backend_model: m",
			wantErr: "not an http or https address",
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
			name:    "a location that is not a short name",
			in:      "models:\n  - alias: a\n    backends: [\"http://x/v1\"]\n    backend_model: m\n    location: \"on prem\"",
			wantErr: "location",
		},
		{
			name:    "a release date that is not a day",
			in:      "models:\n  - alias: a\n    backends: [\"http://x/v1\"]\n    backend_model: m\n    release_date: March 2026",
			wantErr: "YYYY-MM-DD",
		},
		{
			name:    "not YAML at all",
			in:      "models: [[[",
			wantErr: "parse catalogue",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseModelFile([]byte(tc.in))
			if err == nil {
				t.Fatal("Parse accepted a catalogue that would fail at runtime")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// A field the file shape does not have is refused, so a setting nothing reads
// is not mistaken for one that works.
func TestAnUnknownFieldIsRefused(t *testing.T) {
	_, err := parseModelFile([]byte(`
models:
  - alias: fast
    backends: [http://vllm:8000/v1]
    backend_model: served
    api_key_env: FAST_KEY
`))
	if err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("a model with an unknown field: err = %v, want it named", err)
	}
	_, err = parseSandboxFile([]byte(`
sandboxes:
  - name: standard
    image: example/sandbox:1
    egress: [github.com]
`))
	if err == nil || !strings.Contains(err.Error(), "egress") {
		t.Errorf("a class with an unknown field: err = %v, want it named", err)
	}
	if _, err := parseModelFile([]byte("")); err == nil || !strings.Contains(err.Error(), "no models") {
		t.Errorf("an empty file: err = %v, want 'declares no models'", err)
	}
}

func TestASubscriptionModelIsAnthropicsOwnAPI(t *testing.T) {
	// Each caller's Claude sign-in goes to the backend, so nothing else may
	// stand there.
	ok := Model{Alias: "claude-opus", Provider: "anthropic", BackendModel: "claude-opus-5-5",
		Subscription: true}
	m, err := ParseModel(ok)
	if err != nil {
		t.Fatalf("ParseModel: %v", err)
	}
	if !m.Subscription || m.InputMicrosPerMTok == 0 {
		t.Errorf("parsed %+v, want a subscription model with the API prices kept", m)
	}

	for name, bad := range map[string]Model{
		"another provider": {Alias: "x", Provider: "openai", BackendModel: "gpt-5.5",
			Subscription: true},
		"no provider": {Alias: "x", Backends: []string{"http://vllm:8000/v1"},
			BackendModel: "served", Subscription: true},
		"another address": {Alias: "x", Provider: "anthropic", BackendModel: "claude-opus-5-5",
			Backends: []string{"https://proxy.example.ch/v1"}, Subscription: true},
		"not chat": {Alias: "x", Provider: "anthropic", BackendModel: "claude-opus-5-5",
			Kind: "embedding", Subscription: true},
	} {
		if _, err := ParseModel(bad); err == nil {
			t.Errorf("%s: a subscription model was accepted", name)
		}
	}
}
