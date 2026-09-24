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
