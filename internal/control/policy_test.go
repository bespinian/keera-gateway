package control

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestCheckSystemPrompt(t *testing.T) {
	t.Run("an unset prompt stays unset", func(t *testing.T) {
		lim := policy.Limits{}
		if _, ok := checkSystemPrompt(&lim); !ok || lim.SystemPrompt != nil {
			t.Errorf("SystemPrompt = %v, want nil", lim.SystemPrompt)
		}
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		lim := policy.Limits{SystemPrompt: new("  Answer in British English.\n\n")}
		if _, ok := checkSystemPrompt(&lim); !ok {
			t.Fatal("a short prompt was refused")
		}
		if *lim.SystemPrompt != "Answer in British English." {
			t.Errorf("SystemPrompt = %q, want it trimmed", *lim.SystemPrompt)
		}
	})

	t.Run("an emptied field clears the prompt rather than storing blank", func(t *testing.T) {
		lim := policy.Limits{SystemPrompt: new("   \n  ")}
		if _, ok := checkSystemPrompt(&lim); !ok {
			t.Fatal("clearing was refused")
		}
		if lim.SystemPrompt != nil {
			t.Errorf("SystemPrompt = %q, want nil so the scope reads as saying nothing",
				*lim.SystemPrompt)
		}
	})

	t.Run("a prompt past the ceiling is refused with its size", func(t *testing.T) {
		long := strings.Repeat("a", maxSystemPromptBytes+1)
		lim := policy.Limits{SystemPrompt: &long}
		size, ok := checkSystemPrompt(&lim)
		if ok {
			t.Fatal("a prompt over the limit was accepted")
		}
		if size != maxSystemPromptBytes+1 {
			t.Errorf("size = %d, want %d", size, maxSystemPromptBytes+1)
		}
	})
}

func TestSameOrigins(t *testing.T) {
	prev := []string{"https://api.example.ch/v1", "http://10.0.0.7:8000/v1"}
	tests := []struct {
		next []string
		want bool
	}{
		{[]string{"https://api.example.ch/v2"}, true},
		{[]string{"HTTPS://API.example.ch/v1", "http://10.0.0.7:8000"}, true},
		{[]string{"https://api.example.ch:8443/v1"}, false},
		{[]string{"http://api.example.ch/v1"}, false},
		{[]string{"https://api.example.ch/v1", "https://attacker.example/v1"}, false},
		{[]string{"::not a url"}, false},
	}
	for _, tt := range tests {
		if got := sameOrigins(prev, tt.next); got != tt.want {
			t.Errorf("sameOrigins(%v) = %v, want %v", tt.next, got, tt.want)
		}
	}
}

func TestABackendMustBeAWebAddress(t *testing.T) {
	for _, b := range []string{"file:///etc/passwd", "vllm:8000", "http://"} {
		m := policy.Model{Backends: []string{b}, BackendModel: "m", Location: "ch"}
		if msg := normalizeModel(&m); msg == "" {
			t.Errorf("backend %q was accepted", b)
		}
	}
	m := policy.Model{Backends: []string{"http://vllm:8000/v1"}, BackendModel: "m", Location: "ch"}
	if msg := normalizeModel(&m); msg != "" {
		t.Errorf("a plain backend was refused: %s", msg)
	}
}
