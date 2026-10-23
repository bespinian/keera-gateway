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

func TestCountProblem(t *testing.T) {
	const big = policy.MaxPerMinute + 1
	rates := func(r ...policy.RateLimit) policy.Limits { return policy.Limits{RateLimits: r} }
	budgets := func(b ...policy.Budget) policy.Limits { return policy.Limits{Budgets: b} }
	for _, tc := range []struct {
		name string
		lim  policy.Limits
		want string
	}{
		{"nothing set", policy.Limits{}, ""},
		{"per minute", rates(policy.RateLimit{Requests: 60}), ""},
		{"a large count per month", rates(policy.RateLimit{Tokens: big, Period: policy.PeriodMonth}), ""},
		{"one per period", rates(policy.RateLimit{Requests: 60},
			policy.RateLimit{Requests: 1000, Period: policy.PeriodDay}), ""},
		{"a negative count", rates(policy.RateLimit{Requests: -1}), "needs a positive"},
		{"no count", rates(policy.RateLimit{Period: policy.PeriodDay}), "needs a positive"},
		{"too large for a minute", rates(policy.RateLimit{Tokens: big}), "day or month"},
		{"an unknown period", rates(policy.RateLimit{Requests: 1, Period: "week"}), "use minute, day or month"},
		{"two per minute", rates(policy.RateLimit{Requests: 60},
			policy.RateLimit{Tokens: 1000, Period: policy.PeriodMinute}), "two rate limits count per minute"},
		{"a budget per day and per month", budgets(policy.Budget{BudgetMicros: 1},
			policy.Budget{BudgetMicros: 1, Period: policy.PeriodDay}), ""},
		{"two budgets per month", budgets(policy.Budget{BudgetMicros: 1},
			policy.Budget{BudgetMicros: 2, Period: policy.PeriodMonth}), "two budgets reset per month"},
		{"a budget of nothing", budgets(policy.Budget{}), "positive 'budget_micros'"},
		{"a budget per minute", budgets(policy.Budget{BudgetMicros: 1, Period: policy.PeriodMinute}),
			"use day or month"},
	} {
		got := countProblem(&tc.lim)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
