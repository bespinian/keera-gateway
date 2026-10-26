package policy

import (
	"strings"
	"testing"
)

func TestFilterRuleValid(t *testing.T) {
	cases := []struct {
		name string
		rule FilterRule
		want string // a fragment of the refusal, or "" for a rule that is fine
	}{
		{"a replacement", FilterRule{Pattern: `\bsk-\w+\b`, Replace: "[CREDENTIAL]"}, ""},
		{"a refusal", FilterRule{Pattern: `\bexport\b`, Refuse: true, Reason: "why"}, ""},
		{
			"a replacement with nothing to put back",
			FilterRule{Pattern: `\bsk-\w+\b`},
			"",
		},
		{"no expression", FilterRule{Replace: "x"}, "needs an expression"},
		{
			"an expression that will not compile",
			FilterRule{Pattern: `([`, Replace: "x"},
			"not a valid regular expression",
		},
		{
			"both at once",
			FilterRule{Pattern: `x`, Replace: "y", Refuse: true},
			"both replaces and refuses",
		},
		{
			"a reason on a rule that replaces",
			FilterRule{Pattern: `x`, Replace: "y", Reason: "because"},
			"carries a reason and does not refuse",
		},
		{
			"an expression longer than the limit",
			FilterRule{Pattern: strings.Repeat("a", maxFilterPatternLen+1), Replace: "x"},
			"the limit is",
		},
		{
			"a reason longer than the limit",
			FilterRule{
				Pattern: `x`, Refuse: true,
				Reason: strings.Repeat("a", MaxRefusalReasonBytes+1),
			},
			"the limit is",
		},
		// It matches at every position, so a replacing rule built on it puts
		// its replacement between every character of the request.
		{
			"an expression that matches the empty string",
			FilterRule{Pattern: `x*`, Replace: "[X]"},
			"matches the empty string",
		},
		{
			"an optional group that matches nothing",
			FilterRule{Pattern: `(foo)?`, Replace: "[X]"},
			"matches the empty string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Valid()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Valid() = %v, want no error", err)
			case tc.want == "":
			case err == nil:
				t.Errorf("Valid() = nil, want an error about %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("Valid() = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestFilterModePredicates(t *testing.T) {
	// A pattern filter rewrites but runs no model, and most of the program
	// branches on that.
	if !FilterModePattern.Rewrites() {
		t.Error("a pattern filter can replace what it matched, so it rewrites")
	}
	if FilterModePattern.UsesModel() {
		t.Error("a pattern filter runs no model")
	}
	for _, m := range []FilterMode{"", FilterModeRewrite, FilterModeGate} {
		if !m.UsesModel() {
			t.Errorf("%q is a model and an instruction", m)
		}
	}
	if !FilterModePattern.Valid() {
		t.Error("pattern is a mode a filter may run in")
	}
}

func TestRouterCeilings(t *testing.T) {
	rt := Route{
		Mode:         RouteModeSize,
		Destinations: []string{"small", "medium", "big"},
		Ceilings:     map[string]int{"small": 4000, "medium": 32000},
	}
	if n, bounded := rt.Ceiling("small"); !bounded || n != 4000 {
		t.Errorf("Ceiling(small) = %d, %v; want 4000, true", n, bounded)
	}
	if _, bounded := rt.Ceiling("big"); bounded {
		t.Error("a destination with no entry has no ceiling")
	}
	// A zero is the same as no entry, so it must not read as a ceiling of
	// nothing.
	zeroed := Route{Destinations: []string{"a"}, Ceilings: map[string]int{"a": 0}}
	if _, bounded := zeroed.Ceiling("a"); bounded {
		t.Error("a ceiling of zero is no ceiling")
	}
	if got := rt.Unbounded(); len(got) != 1 || got[0] != "big" {
		t.Errorf("Unbounded() = %v, want [big]", got)
	}
}

func TestRouteModePredicates(t *testing.T) {
	// A size route step reads the request and no model, so it is not one of the
	// modes that decide, and its order is not a measurement either.
	if RouteModeSize.Decides() {
		t.Error("a size step asks no model anything")
	}
	if RouteModeSize.Measures() {
		t.Error("a size step's order is not a reading of this process")
	}
	if !RouteModeSize.Sizes() {
		t.Error("a size step sizes")
	}
	for _, m := range []RouteMode{
		"", RouteModeInstruction, RouteModeFallback,
		RouteModeLatency, RouteModeLeastBusy,
	} {
		if m.Sizes() {
			t.Errorf("%q does not choose by size", m)
		}
	}
	if !RouteModeSize.Valid() {
		t.Error("size is a mode a router may run in")
	}
}
