package gateway

import (
	"encoding/json"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestFitProviderFixesWhatOpenAIRefuses(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		path     string
		in       string
		want     map[string]any
	}{{
		name:     "a reasoning model gets max_completion_tokens and no sampling",
		provider: "openai", model: "gpt-5.5", path: "/chat/completions",
		in:   `{"model":"gpt-5.5","max_tokens":64,"temperature":0,"top_p":0.9}`,
		want: map[string]any{"model": "gpt-5.5", "max_completion_tokens": float64(64)},
	}, {
		name:     "an older model keeps its temperature",
		provider: "openai", model: "gpt-4.1", path: "/chat/completions",
		in:   `{"max_tokens":64,"temperature":0}`,
		want: map[string]any{"max_completion_tokens": float64(64), "temperature": float64(0)},
	}, {
		name:     "a stated max_completion_tokens wins",
		provider: "openai", model: "gpt-5.5", path: "/chat/completions",
		in:   `{"max_tokens":64,"max_completion_tokens":32}`,
		want: map[string]any{"max_completion_tokens": float64(32)},
	}, {
		name:     "the Responses API has its own ceiling field",
		provider: "openai", model: "gpt-5.5", path: "/responses",
		in:   `{"max_output_tokens":64,"temperature":1}`,
		want: map[string]any{"max_output_tokens": float64(64)},
	}, {
		name:     "another provider is left alone",
		provider: "infomaniak", model: "gpt-5.5", path: "/chat/completions",
		in:   `{"max_tokens":64,"temperature":0}`,
		want: map[string]any{"max_tokens": float64(64), "temperature": float64(0)},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := policy.Model{Provider: tc.provider, BackendModel: tc.model}
			var got map[string]any
			if err := json.Unmarshal(fitProvider(m, tc.path, []byte(tc.in)), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}
