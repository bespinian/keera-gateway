package policy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// routerRefs is an organisation with a local and a hosted model, a model
// that decides, and a filter of each mode.
func routerRefs() RouterRefs {
	return RouterRefs{
		Models: map[string]Model{
			"local":  {Alias: "local", Kind: KindChat},
			"hosted": {Alias: "hosted", Kind: KindChat},
			"embed":  {Alias: "embed", Kind: KindEmbedding},
			"plan":   {Alias: "plan", Kind: KindChat, Subscription: true},
			"picker": {Alias: "picker", Kind: KindChat},
		},
		Filters: map[string]Filter{
			"secrets": {Alias: "secrets", Mode: FilterModePattern},
			"redact":  {Alias: "redact", Mode: FilterModeRewrite},
			"judge":   {Alias: "judge", Mode: FilterModeGate},
			"trial":   {Alias: "trial", Mode: FilterModeRewrite, Shadow: true},
		},
	}
}

// pick is an instruction step between the local and the hosted model.
func pick(id string, branches map[string]*Step) *Step {
	return &Step{
		ID: id, Kind: StepRoute, Mode: RouteModeInstruction, Model: "picker",
		Prompt: "Keep anything about a client local.", Destinations: []string{"local", "hosted"},
		Fallback: "local", Branches: branches,
	}
}

// exampleRouter is the one in docs/routers.md.
func exampleRouter() Router {
	return Router{OrgID: "org_1", Alias: "safe", Start: &Step{
		ID: "secrets", Kind: StepFilter, Ref: "secrets",
		Next: &Step{
			ID: "cap", Kind: StepBudget, BudgetMicros: 50_000_000, Period: PeriodMonth,
			Exceeded: &Step{ID: "spent", Kind: StepModel, Ref: "local"},
			Next: pick("route", map[string]*Step{
				"hosted": {
					ID: "redact", Kind: StepFilter, Ref: "redact",
					Next: &Step{ID: "out", Kind: StepModel, Ref: "hosted"},
				},
			}),
		},
	}}
}

func TestRouterExampleIsValid(t *testing.T) {
	p := exampleRouter()
	if msg := p.Problem(routerRefs()); msg != "" {
		t.Fatalf("Problem() = %q", msg)
	}
	// The budget's fallback, the route step's unbranched destination, and the
	// end of its hosted branch.
	got := p.Reaches()
	slices.Sort(got)
	if want := []string{"hosted", "local"}; !slices.Equal(got, want) {
		t.Errorf("Reaches() = %v, want %v", got, want)
	}
	if w := p.Warnings(routerRefs()); len(w) != 0 {
		t.Errorf("Warnings() = %v; the rewrite comes after the route step", w)
	}
}

func TestRouterSurvivesJSON(t *testing.T) {
	raw, err := json.Marshal(exampleRouter())
	if err != nil {
		t.Fatal(err)
	}
	var back Router
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for s := range back.Steps() {
		ids = append(ids, s.ID)
	}
	// Each step before what follows it: next, then exceeded, then branches.
	if want := []string{"secrets", "cap", "route", "redact", "out", "spent"}; !slices.Equal(ids, want) {
		t.Errorf("steps = %v, want %v", ids, want)
	}
}

func TestRouterProblems(t *testing.T) {
	model := func(id, ref string) *Step { return &Step{ID: id, Kind: StepModel, Ref: ref} }
	tests := []struct {
		name  string
		start *Step
		want  string
	}{
		{"no steps", nil, "needs at least one step"},
		{
			"a filter must lead somewhere",
			&Step{ID: "a", Kind: StepFilter, Ref: "redact"}, "has no 'next' step",
		},
		{
			"an unknown filter",
			&Step{ID: "a", Kind: StepFilter, Ref: "nope", Next: model("b", "local")},
			"not one of this organisation's filters",
		},
		{
			"a branch for a model the route step does not offer",
			pick("a", map[string]*Step{"elsewhere": model("b", "local")}),
			"not one of its destinations",
		},
		{
			"an empty branch",
			pick("a", map[string]*Step{"local": nil}), "empty branch",
		},
		{
			"a step after a route step goes in a branch",
			func() *Step { s := pick("a", nil); s.Next = model("b", "local"); return s }(),
			"in 'branches'",
		},
		{
			"nothing follows a model",
			&Step{ID: "a", Kind: StepModel, Ref: "local", Next: model("b", "hosted")},
			"nothing can follow",
		},
		{
			"an embedding model cannot answer a chat request",
			model("a", "embed"), "a embedding model",
		},
		{
			"nor can a subscription model be sent to",
			model("a", "plan"), "Claude subscription",
		},
		{
			"a rate limit that limits nothing",
			&Step{ID: "a", Kind: StepRateLimit, Next: model("b", "local")}, "positive 'requests'",
		},
		{
			"a budget of nothing",
			&Step{ID: "a", Kind: StepBudget, Next: model("b", "local")}, "positive 'budget_micros'",
		},
		{
			"a rate limit per week",
			&Step{ID: "a", Kind: StepRateLimit, Requests: 1, Period: "week", Next: model("b", "local")},
			"use minute, day or month",
		},
		{
			"a budget per minute",
			&Step{ID: "a", Kind: StepBudget, BudgetMicros: 1, Period: PeriodMinute, Next: model("b", "local")},
			"use day or month",
		},
		{
			"only a limit has a period",
			&Step{ID: "a", Kind: StepFilter, Ref: "redact", Period: PeriodDay, Next: model("b", "local")},
			"only a limit has",
		},
		{
			"a budget per week",
			&Step{ID: "a", Kind: StepBudget, BudgetMicros: 1, Period: "week", Next: model("b", "local")},
			"use day or month",
		},
		{
			"only a limit has an exceeded branch",
			&Step{
				ID: "a", Kind: StepFilter, Ref: "redact", Next: model("b", "local"),
				Exceeded: model("c", "local"),
			},
			"only a limit has",
		},
		{
			"a limit names nothing",
			&Step{ID: "a", Kind: StepRateLimit, Requests: 1, Ref: "redact", Next: model("b", "local")},
			"uses nothing of the organisation's",
		},
		{
			"two steps share an id",
			&Step{ID: "a", Kind: StepFilter, Ref: "redact", Next: model("a", "local")},
			"two steps have the id 'a'",
		},
		{
			"an id with a capital",
			model("A", "local"), "step id 'A' is not usable",
		},
		{
			"a system prompt with nothing to say",
			&Step{ID: "a", Kind: StepSystemPrompt, Prompt: "  ", Next: model("b", "local")},
			"needs a 'prompt'",
		},
		{
			"a system prompt must lead somewhere",
			&Step{ID: "a", Kind: StepSystemPrompt, Prompt: "Be brief."}, "has no 'next' step",
		},
		{
			"a system prompt names nothing",
			&Step{
				ID: "a", Kind: StepSystemPrompt, Prompt: "Be brief.", Ref: "redact",
				Next: model("b", "local"),
			}, "uses nothing of the organisation's",
		},
		{
			"only a system prompt or route step has a prompt",
			&Step{
				ID: "a", Kind: StepFilter, Ref: "redact", Prompt: "Be brief.",
				Next: model("b", "local"),
			}, "only a system prompt or route step has",
		},
		{
			"only a route step has destinations",
			&Step{
				ID: "a", Kind: StepFilter, Ref: "redact", Destinations: []string{"local"},
				Next: model("b", "local"),
			}, "only a route step has",
		},
		{
			"an unknown kind",
			&Step{ID: "a", Kind: "cache"}, "the kind 'cache'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Router{OrgID: "org_1", Alias: "p", Start: tt.start}
			if got := p.Problem(routerRefs()); !strings.Contains(got, tt.want) {
				t.Errorf("Problem() = %q, want it to say %q", got, tt.want)
			}
		})
	}
}

func TestRouterIsBounded(t *testing.T) {
	start := &Step{ID: "s0", Kind: StepModel, Ref: "local"}
	for i := 1; i <= MaxRouterSteps; i++ {
		start = &Step{
			ID:   "s" + string(rune('a'+i%26)) + strings.Repeat("x", i/26),
			Kind: StepRateLimit, Requests: 1, Next: start,
		}
	}
	p := Router{OrgID: "org_1", Alias: "p", Start: start}
	if got := p.Problem(routerRefs()); !strings.Contains(got, "at most") {
		t.Errorf("Problem() = %q, want the step limit", got)
	}
}

func TestRouteStepProblems(t *testing.T) {
	route := func(change func(*Step)) *Step {
		s := pick("r", nil)
		change(s)
		return s
	}
	sized := func(change func(*Step)) *Step {
		return route(func(s *Step) {
			s.Mode, s.Model, s.Prompt, s.Fallback = RouteModeSize, "", "", ""
			s.Ceilings = map[string]int{"local": 4000}
			change(s)
		})
	}
	byHeader := func(change func(*Step)) *Step {
		return route(func(s *Step) {
			s.Mode, s.Model, s.Prompt = RouteModeHeader, "", ""
			s.Header, s.Values = "X-Keera-Route", map[string]string{"hard": "hosted"}
			change(s)
		})
	}
	tests := []struct {
		name  string
		start *Step
		want  string
	}{
		{"an unknown mode", route(func(s *Step) { s.Mode = "random" }), "the mode 'random'"},
		{
			"no destinations", route(func(s *Step) { s.Destinations, s.Fallback = nil, "" }),
			"needs 'destinations'",
		},
		// The step would quietly choose between fewer models than it says.
		{
			"a destination that does not exist",
			route(func(s *Step) { s.Destinations = []string{"local", "huge"} }),
			"'huge', which is not one of this organisation's models",
		},
		{
			"a destination that cannot answer a chat request",
			route(func(s *Step) { s.Destinations = []string{"local", "embed"} }), "embedding model",
		},
		{
			"a subscription model as a destination",
			route(func(s *Step) { s.Destinations = []string{"local", "plan"} }), "Claude subscription",
		},
		{
			"a destination named twice",
			route(func(s *Step) { s.Destinations = []string{"local", "local"} }), "twice",
		},
		{
			"too many destinations",
			route(func(s *Step) {
				s.Destinations = nil
				for i := range MaxDestinations + 1 {
					s.Destinations = append(s.Destinations, "local-"+string(rune('a'+i)))
				}
				s.Fallback = ""
			}), "the limit is 16",
		},
		// The fallback has to be somewhere the step could send anyway.
		{
			"a fallback that is not a destination",
			route(func(s *Step) { s.Fallback = "embed" }), "not one of its destinations",
		},
		{"no deciding model", route(func(s *Step) { s.Model = "" }), "needs a 'model'"},
		{"no instruction", route(func(s *Step) { s.Prompt = " " }), "needs a 'prompt'"},
		{
			"an instruction too long to send on every request",
			route(func(s *Step) { s.Prompt = strings.Repeat("x", MaxRoutePromptBytes+1) }),
			"the limit is 4096",
		},
		{
			"a deciding model on a step that decides nothing",
			route(func(s *Step) { s.Mode, s.Prompt, s.Fallback = RouteModeFallback, "", "" }),
			"has no deciding model",
		},
		{
			"a fallback on a step that tries every destination anyway",
			route(func(s *Step) { s.Mode, s.Model, s.Prompt = RouteModeLatency, "", "" }),
			"put 'local' in 'destinations'",
		},
		{
			"ceilings on a step that does not read sizes",
			route(func(s *Step) { s.Ceilings = map[string]int{"local": 4000} }),
			"only a step with the mode 'size' has",
		},
		{
			"a ceiling for a model that is not a destination",
			sized(func(s *Step) { s.Ceilings["huge"] = 8000 }), "a ceiling for 'huge'",
		},
		{
			"no destination left for the largest requests",
			sized(func(s *Step) { s.Ceilings["hosted"] = 8000 }), "needs one destination without a ceiling",
		},
		{
			"two destinations without a ceiling",
			sized(func(s *Step) { s.Ceilings = nil }), "only one destination",
		},
		{
			"a header on a step that does not read one",
			route(func(s *Step) { s.Header = "X-Keera-Route" }),
			"only a step with the mode 'header' has",
		},
		{
			"a header step without a header",
			byHeader(func(s *Step) { s.Header = "" }), "needs a 'header'",
		},
		{
			"a header name with a space",
			byHeader(func(s *Step) { s.Header = "X Route" }), "not a usable header name",
		},
		// The key would end up in a routing decision.
		{
			"a header step reading the key",
			byHeader(func(s *Step) { s.Header = "authorization" }), "credential",
		},
		{
			"a header step without values",
			byHeader(func(s *Step) { s.Values = nil }), "needs 'values'",
		},
		// Values are compared in lowercase, so this would never match.
		{
			"a value in capitals",
			byHeader(func(s *Step) { s.Values = map[string]string{"Hard": "hosted"} }),
			"the value 'Hard'",
		},
		{
			"a value for a model that is not a destination",
			byHeader(func(s *Step) { s.Values["cheap"] = "picker" }),
			"'picker', which is not one of its destinations",
		},
		{
			"a destination nothing reaches",
			byHeader(func(s *Step) { s.Fallback = "" }), "no request can reach it",
		},
		{
			"a deciding model on a header step",
			byHeader(func(s *Step) { s.Model = "picker" }), "has no deciding model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Router{OrgID: "org_1", Alias: "p", Start: tt.start}
			if got := p.Problem(routerRefs()); !strings.Contains(got, tt.want) {
				t.Errorf("Problem() = %q, want it to say %q", got, tt.want)
			}
		})
	}

	t.Run("one destination is enough", func(t *testing.T) {
		// It gives clients a stable name.
		for _, mode := range routeModes {
			s := &Step{ID: "r", Kind: StepRoute, Mode: mode, Destinations: []string{"local"}}
			if mode.Decides() {
				s.Model, s.Prompt = "picker", "Choose one."
			}
			if mode == RouteModeHeader {
				s.Header, s.Values = "X-Keera-Route", map[string]string{"any": "local"}
			}
			p := Router{OrgID: "org_1", Alias: "p", Start: s}
			if got := p.Problem(routerRefs()); got != "" {
				t.Errorf("%s: Problem() = %q", mode, got)
			}
		}
	})
	t.Run("a header step with a fallback", func(t *testing.T) {
		// The fallback takes a request without the header.
		p := Router{OrgID: "org_1", Alias: "p", Start: byHeader(func(*Step) {})}
		if got := p.Problem(routerRefs()); got != "" {
			t.Errorf("Problem() = %q", got)
		}
	})
	t.Run("a size step with one destination left unbounded", func(t *testing.T) {
		p := Router{OrgID: "org_1", Alias: "p", Start: sized(func(*Step) {})}
		if got := p.Problem(routerRefs()); got != "" {
			t.Errorf("Problem() = %q", got)
		}
	})
}

func TestRouterWarnsOfARewriteBeforeARouteStep(t *testing.T) {
	router := pick("r", nil)
	filter := func(ref string, next *Step) *Step {
		return &Step{ID: "f-" + ref, Kind: StepFilter, Ref: ref, Next: next}
	}
	tests := []struct {
		name  string
		start *Step
		warns bool
	}{
		{"a rewrite before the route step", filter("redact", router), true},
		{"a gate judges but does not change what the step reads", filter("judge", router), false},
		// It removes credentials, which are not what a route step judges by.
		{"a pattern filter before the route step", filter("secrets", router), false},
		{"a shadow rewrite changes nothing", filter("trial", router), false},
		{"a route step alone", router, false},
		// It reads nothing, so it has nothing to be misled by.
		{"a rewrite before a step that reads nothing", filter("redact", &Step{
			ID:   "r",
			Kind: StepRoute, Mode: RouteModeFallback, Destinations: []string{"local", "hosted"},
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Router{OrgID: "org_1", Alias: "p", Start: tt.start}
			if msg := p.Problem(routerRefs()); msg != "" {
				t.Fatalf("Problem() = %q", msg)
			}
			if got := len(p.Warnings(routerRefs())) > 0; got != tt.warns {
				t.Errorf("warns = %v, want %v: %v", got, tt.warns, p.Warnings(routerRefs()))
			}
		})
	}
}

func TestRouterScopeIsItsOwn(t *testing.T) {
	p := exampleRouter()
	var budget *Step
	for s := range p.Steps() {
		if s.ID == "cap" {
			budget = s
		}
	}
	scopes := p.Scopes(budget)
	if len(scopes) != 1 {
		t.Fatalf("a budget counts under %d scopes, want one", len(scopes))
	}
	sc := scopes[0]
	if sc.Type != ScopeRouter || sc.ID != "org_1/safe/cap" || sc.Meter != MeterCost {
		t.Errorf("scope = %s %s %q, want the router's, counting money", sc.Type, sc.ID, sc.Meter)
	}
	if sc.BudgetMicros != 50_000_000 || sc.Period != PeriodMonth {
		t.Errorf("scope budget = %d per %s", sc.BudgetMicros, sc.Period)
	}
	// A refusal names it as the router's, not the key's.
	if got := sc.Type.Possessive(); got != "this router" {
		t.Errorf("Possessive() = %q", got)
	}
}

// A rate limit per minute is token buckets. Per day or month it is counted
// like a budget, one window each for requests and tokens.
func TestRouterRateLimitScopes(t *testing.T) {
	p := Router{OrgID: "org_1", Alias: "safe"}

	minute := p.Scopes(&Step{ID: "burst", Kind: StepRateLimit, Requests: 60, Tokens: 1000})
	if len(minute) != 1 || minute[0].RPM != 60 || minute[0].TPM != 1000 || minute[0].Quota != 0 {
		t.Errorf("per minute = %+v, want one scope of buckets", minute)
	}

	day := p.Scopes(&Step{
		ID: "daily", Kind: StepRateLimit, Requests: 500, Tokens: 2_000_000,
		Period: PeriodDay,
	})
	if len(day) != 2 {
		t.Fatalf("per day = %+v, want a scope for requests and one for tokens", day)
	}
	for i, want := range []struct {
		id    string
		meter Meter
		quota int64
	}{
		{"org_1/safe/daily|requests", MeterRequests, 500},
		{"org_1/safe/daily|tokens", MeterTokens, 2_000_000},
	} {
		sc := day[i]
		if sc.ID != want.id || sc.Meter != want.meter || sc.Quota != want.quota ||
			sc.Period != PeriodDay || sc.RPM != 0 || sc.BudgetMicros != 0 {
			t.Errorf("per day [%d] = %+v, want %+v", i, sc, want)
		}
	}

	// A limit on requests alone counts no tokens.
	if got := p.Scopes(&Step{ID: "m", Kind: StepRateLimit, Requests: 9, Period: PeriodMonth}); len(got) != 1 ||
		got[0].Meter != MeterRequests {
		t.Errorf("requests per month = %+v", got)
	}
}

func TestMeterAmount(t *testing.T) {
	for _, tt := range []struct {
		meter Meter
		want  int64
	}{{MeterCost, 900}, {MeterRequests, 1}, {MeterTokens, 42}} {
		if got := tt.meter.Amount(900, 42); got != tt.want {
			t.Errorf("%q.Amount = %d, want %d", tt.meter, got, tt.want)
		}
	}
}

func TestRouterUses(t *testing.T) {
	p := exampleRouter()
	for _, tt := range []struct {
		kind  StepKind
		alias string
		want  bool
	}{
		{StepFilter, "redact", true},
		{StepFilter, "judge", false},
		{StepModel, "local", true},
		// A route step uses the model it decides with, and its destinations.
		{StepModel, "picker", true},
		{StepModel, "hosted", true},
		{StepModel, "nowhere", false},
		{StepRoute, "redact", false},
	} {
		if got := p.Uses(tt.kind, tt.alias); got != tt.want {
			t.Errorf("Uses(%s, %s) = %v, want %v", tt.kind, tt.alias, got, tt.want)
		}
	}
}
