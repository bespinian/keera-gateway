package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// treeHarness is routeHarness with p as the organisation's only router, and
// two pattern filters it can use: 'redact' takes out a password, 'block'
// refuses one.
func treeHarness(t *testing.T, decide func([]string) string, p policy.Router,
	resolved *policy.Resolved,
) *harness {
	t.Helper()
	h := routeHarness(t, decide, autoRoute(), resolved)
	p.OrgID = "org_1"
	h.src.routers = map[string]policy.Router{"org_1/" + p.Alias: p}
	block := patternFilter(policy.FilterRule{Pattern: `hunter2`, Refuse: true, Reason: "no passwords"})
	block.Alias = "block"
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilter(policy.FilterRule{Pattern: `hunter2`, Replace: "[SECRET]"}),
		"org_1/block":  block,
	}
	return h
}

func modelStep(id, alias string) *policy.Step {
	return &policy.Step{ID: id, Kind: policy.StepModel, Ref: alias}
}

// autoStep is autoRoute as a step of a larger router, with branches.
func autoStep(id string, branches map[string]*policy.Step) *policy.Step {
	s := routerOf(autoRoute()).Start
	s.ID, s.Branches = id, branches
	return s
}

// lastSent is the body the answering model was sent: the last one the
// stand-in received.
func lastSent(h *harness) string {
	sent := drain(h)
	if len(sent) == 0 {
		return ""
	}
	return sent[len(sent)-1]
}

// drain takes every body sent upstream so far, oldest first.
func drain(h *harness) []string {
	var sent []string
	for {
		select {
		case b := <-h.upstreamBodies:
			sent = append(sent, string(b))
		default:
			return sent
		}
	}
}

const secretPrompt = `{"model":"safe","messages":[{"role":"user","content":"deploy with hunter2"}]}`

// The redaction sits on the branch to the hosted model only, which is why a
// router beats a filter on the key: the local model gets the request as
// sent, and the route step decides on it unredacted.
func TestRouterFiltersOnlyTheBranchTheRouterChose(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: autoStep("route", map[string]*policy.Step{
		"keera-large": {
			ID: "redact", Kind: policy.StepFilter, Ref: "redact",
			Next: modelStep("large", "keera-large"),
		},
	})}
	for _, tt := range []struct {
		chosen, served string
		redacted       bool
	}{
		{"keera-large", "large-served", true},
		{"keera-small", "small-served", false},
	} {
		t.Run(tt.chosen, func(t *testing.T) {
			var seen []string
			h := treeHarness(t, func(segments []string) string {
				seen = segments
				return tt.chosen
			}, p, nil)

			resp := h.post(t, "/v1/chat/completions", secretPrompt)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			sent := lastSent(h)
			if !strings.Contains(sent, tt.served) {
				t.Errorf("the request did not go to %s: %s", tt.chosen, sent)
			}
			if got := strings.Contains(sent, "[SECRET]"); got != tt.redacted {
				t.Errorf("redacted = %v, want %v: %s", got, tt.redacted, sent)
			}
			if len(seen) != 1 || seen[0] != "deploy with hunter2" {
				t.Errorf("the route step was shown %q, want the request as sent", seen)
			}
			if got, want := resp.Header.Get("X-Keera-Router"), "safe -> "+tt.chosen; got != want {
				t.Errorf("X-Keera-Router = %q, want %q", got, want)
			}
			ev := h.sink.last(t)
			if ev.Router != "safe" || ev.RouterOutcome != store.RouterChose || ev.Alias != tt.chosen {
				t.Errorf("router/outcome/alias = %q/%q/%q", ev.Router, ev.RouterOutcome, ev.Alias)
			}
		})
	}
}

// A spent budget sends the request down its exceeded branch instead of
// refusing it, and says so on the usage row. Within it, the request is
// charged to the step.
func TestRouterBudgetSendsWhatIsOverItElsewhere(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "cap", Kind: policy.StepBudget, BudgetMicros: 1_000_000, Period: policy.PeriodMonth,
		Next:     modelStep("large", "keera-large"),
		Exceeded: modelStep("small", "keera-small"),
	}}

	h := treeHarness(t, picks("keera-small"), p, nil)
	if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sent := lastSent(h); !strings.Contains(sent, "large-served") {
		t.Errorf("within the budget, the request went elsewhere: %s", sent)
	}
	if !slices.ContainsFunc(h.budgets.chargedTo, func(sc policy.Scope) bool {
		return sc.Type == policy.ScopeRouter && sc.ID == "org_1/safe/cap"
	}) {
		t.Errorf("the answer was charged to %+v, not the router's budget", h.budgets.chargedTo)
	}
	if ev := h.sink.last(t); !slices.ContainsFunc(ev.Scopes, func(sc policy.Scope) bool {
		return sc.ID == "org_1/safe/cap"
	}) {
		t.Errorf("the usage row's scopes %+v leave out the router's budget", ev.Scopes)
	}

	h.budgets.spentType = policy.ScopeRouter
	if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 by way of the exceeded branch", resp.StatusCode)
	}
	if sent := lastSent(h); !strings.Contains(sent, "small-served") {
		t.Errorf("over the budget, the request did not take the exceeded branch: %s", sent)
	}
	if ev := h.sink.last(t); !strings.Contains(ev.Note, "exceeded branch of 'cap'") {
		t.Errorf("note = %q, want it to say why the cheaper model answered", ev.Note)
	}
}

// Without an exceeded branch, a spent budget refuses, and names the router,
// not the key, as what ran out.
func TestRouterBudgetWithoutABranchRefuses(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "cap", Kind: policy.StepBudget, BudgetMicros: 1_000_000, Period: policy.PeriodMonth,
		Next: modelStep("large", "keera-large"),
	}}
	h := treeHarness(t, picks("keera-small"), p, nil)
	h.budgets.spentType = policy.ScopeRouter

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "the router 'safe'") ||
		!strings.Contains(string(body), "this router has spent") {
		t.Errorf("the refusal does not name the router: %s", body)
	}
}

// A rate limit step counts the requests through it, apart from the key's.
func TestRouterRateLimitCountsItsOwnStep(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "burst", Kind: policy.StepRateLimit, Requests: 1,
		Next:     modelStep("large", "keera-large"),
		Exceeded: modelStep("small", "keera-small"),
	}}
	h := treeHarness(t, picks("keera-small"), p, nil)

	for i, want := range []string{"large-served", "small-served"} {
		if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
		if sent := lastSent(h); !strings.Contains(sent, want) {
			t.Errorf("request %d did not go to %s: %s", i, want, sent)
		}
	}
}

// A filter step that refuses says it was the router's, since no guardrail
// on the key had anything to do with it.
func TestRouterFilterRefusalNamesTheRouter(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "block", Kind: policy.StepFilter, Ref: "block", Next: modelStep("large", "keera-large"),
	}}
	h := treeHarness(t, picks("keera-small"), p, nil)

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "the router 'safe' checks every request") {
		t.Errorf("the refusal does not name the router: %s", body)
	}
	if ev := h.sink.last(t); len(ev.FilterRuns) != 1 || ev.FilterRuns[0].Filter != "block" {
		t.Errorf("filter runs = %+v, want the router's one", ev.FilterRuns)
	}
}

// The key's own filters still run after the router, so a router cannot
// be used to step around a guardrail.
func TestTheKeysFiltersStillRunAfterARouter(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact"}
	p := policy.Router{Alias: "safe", Start: modelStep("large", "keera-large")}
	h := treeHarness(t, picks("keera-small"), p, resolved)

	if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sent := lastSent(h); !strings.Contains(sent, "[SECRET]") {
		t.Errorf("the key's filter did not run: %s", sent)
	}
}

// A filter both the router and the key's guardrail name runs once, as a filter
// named at two guardrail levels does: once charged, once logged, once listed.
func TestAFilterTheRouterRanIsNotRunAgainForTheKey(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact"}
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "redact", Kind: policy.StepFilter, Ref: "redact", Next: modelStep("large", "keera-large"),
	}}
	h := treeHarness(t, picks("keera-small"), p, resolved)

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "redact" {
		t.Errorf("X-Keera-Filters = %q, want redact", got)
	}
	if sent := lastSent(h); !strings.Contains(sent, "[SECRET]") {
		t.Errorf("the filter did not run: %s", sent)
	}
	if ev := h.sink.last(t); len(ev.FilterRuns) != 1 {
		t.Errorf("filter runs = %+v, want one", ev.FilterRuns)
	}
}

// Route steps can follow one another. The request goes where the last one
// sends it.
func TestRouterChainsRouteSteps(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: autoStep("first", map[string]*policy.Step{
		"keera-large": {
			ID: "second", Kind: policy.StepRoute, Mode: policy.RouteModeFallback,
			Destinations: []string{"keera-small"},
		},
	})}
	h := treeHarness(t, picks("keera-large"), p, nil)

	if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sent := lastSent(h); !strings.Contains(sent, "small-served") {
		t.Errorf("the second step's choice did not answer: %s", sent)
	}
	ev := h.sink.last(t)
	if ev.Router != "safe" || ev.Alias != "keera-small" {
		t.Errorf("router/alias = %q/%q, want safe/keera-small", ev.Router, ev.Alias)
	}
	// The first step's decision was a generation, and it is still paid for.
	if ev.CostMicros <= answerOnly(t, h) {
		t.Errorf("cost = %d, which leaves out the first step's decision", ev.CostMicros)
	}
}

// A branch can end at a model other than the one the step chose. The header
// names the model that answered, not the step's choice.
func TestRouterHeaderNamesTheModelABranchEndsAt(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: autoStep("route", map[string]*policy.Step{
		"keera-large": modelStep("instead", "keera-small"),
	})}
	h := treeHarness(t, picks("keera-large"), p, nil)

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sent := lastSent(h); !strings.Contains(sent, "small-served") {
		t.Errorf("the branch's model did not answer: %s", sent)
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "safe -> keera-small" {
		t.Errorf("X-Keera-Router = %q, want the model that answered", got)
	}
}

// answerOnly is what the answer alone cost on the last usage row.
func answerOnly(t *testing.T, h *harness) int64 {
	t.Helper()
	ev := h.sink.last(t)
	cost, _ := answerCost(policy.Model{InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 1_000_000},
		policy.Tokens{Input: ev.InputTokens, Output: ev.OutputTokens})
	return cost
}

// A router whose model was deleted under it refuses rather than guessing.
func TestRouterEndingAtAMissingModelIsRefused(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: modelStep("gone", "keera-gone")}
	h := treeHarness(t, picks("keera-small"), p, nil)

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "router_unavailable") {
		t.Errorf("body = %s", body)
	}
}

func TestRoutersAreListedWhereAClientLooksForModels(t *testing.T) {
	p := policy.Router{Alias: "safe", DisplayName: "Safe", Start: autoStep("route", map[string]*policy.Step{
		"keera-large": {
			ID: "redact", Kind: policy.StepFilter, Ref: "redact",
			Next: modelStep("large", "keera-large"),
		},
	})}
	h := treeHarness(t, picks("keera-small"), p, nil)

	resp := h.get(t, "/v1/models/safe")
	var out struct {
		ID           string   `json:"id"`
		DisplayName  string   `json:"display_name"`
		Destinations []string `json:"destinations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out.Destinations)
	if out.ID != "safe" || out.DisplayName != "Safe" ||
		!slices.Equal(out.Destinations, []string{"keera-large", "keera-small"}) {
		t.Errorf("listed as %+v", out)
	}
}

// On /v1/messages the route step translates the body before a filter on its
// branch rewrites the client's own. The model still gets the rewrite, not the
// translation made before it.
func TestRouterFilterAfterARouteStepRewritesWhatIsSent(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: autoStep("route", map[string]*policy.Step{
		"keera-large": {
			ID: "redact", Kind: policy.StepFilter, Ref: "redact",
			Next: modelStep("large", "keera-large"),
		},
	})}
	h := treeHarness(t, picks("keera-large"), p, nil)

	resp := h.post(t, "/v1/messages",
		`{"model":"safe","max_tokens":16,"messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	sent := lastSent(h)
	if !strings.Contains(sent, "large-served") || strings.Contains(sent, "hunter2") {
		t.Errorf("the model was sent %s", sent)
	}
}

// A trying step whose first choice is down would move on to the next
// destination. When that one has a branch, moving on would skip the branch,
// and with it the filter that redacts what goes there. So it is not tried.
func TestRouterNeverFailsOverPastABranch(t *testing.T) {
	h := chainHarness(t, chainRoute("keera-small", "keera-large"),
		map[string]int{"small-served": http.StatusServiceUnavailable})
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilter(policy.FilterRule{Pattern: `hunter2`, Replace: "[SECRET]"}),
	}
	safe := routerOf(chainRoute("keera-small", "keera-large"))
	safe.Alias = "safe"
	safe.Start.Branches = map[string]*policy.Step{
		"keera-large": {
			ID: "redact", Kind: policy.StepFilter, Ref: "redact",
			Next: modelStep("large", "keera-large"),
		},
	}
	h.src.routers["org_1/safe"] = safe

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the first destination's own 503", resp.StatusCode)
	}
	for _, b := range drain(h) {
		if strings.Contains(b, "large-served") {
			t.Fatalf("the step failed over past the branch: %s", b)
		}
	}
}

// A rate limit per day counts in the windows budgets use, so it survives a
// restart and starts again at midnight UTC. The tokens are counted once the
// answer says how many there were.
func TestRouterRateLimitPerDay(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "daily", Kind: policy.StepRateLimit, Requests: 2, Tokens: 1_000_000,
		Period:   policy.PeriodDay,
		Next:     modelStep("large", "keera-large"),
		Exceeded: modelStep("small", "keera-small"),
	}}
	h := treeHarness(t, picks("keera-small"), p, nil)

	for i, want := range []string{"large-served", "large-served", "small-served"} {
		if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
		if sent := lastSent(h); !strings.Contains(sent, want) {
			t.Errorf("request %d did not go to %s: %s", i, want, sent)
		}
	}
	h.budgets.mu.Lock()
	requests, tokens := h.budgets.tallied["org_1/safe/daily|requests"], h.budgets.tallied["org_1/safe/daily|tokens"]
	h.budgets.mu.Unlock()
	if requests != 2 {
		t.Errorf("requests counted = %d, want the 2 within the limit", requests)
	}
	if tokens == 0 {
		t.Error("the answers' tokens were not counted against the daily limit")
	}
}

// Without an exceeded branch, a spent daily limit refuses with 429, and says
// when it starts again, so a client does not retry every second all day.
func TestRouterRateLimitPerDayRefusesUntilItResets(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "daily", Kind: policy.StepRateLimit, Requests: 1, Period: policy.PeriodDay,
		Next: modelStep("large", "keera-large"),
	}}
	h := treeHarness(t, picks("keera-small"), p, nil)

	if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", resp.StatusCode)
	}
	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After, so the client cannot tell the limit lasts until tomorrow")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "this router is over its limit of 1 request per day") ||
		!strings.Contains(string(body), "requests_quota_exceeded") {
		t.Errorf("body = %s", body)
	}
}

// A system prompt step is added only on its own branch, after the key's own
// prompt. Like that one, it is added after every filter, so a filter placed
// after the step neither reads nor rewrites it.
func TestRouterSystemPromptGoesOnlyWhereItsBranchGoes(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{SystemPrompt: new("Be brief.")}, nil, nil)
	p := policy.Router{Alias: "safe", Start: autoStep("route", map[string]*policy.Step{
		"keera-large": {
			ID: "careful", Kind: policy.StepSystemPrompt,
			Prompt: "Never name a customer.",
			Next: &policy.Step{
				ID: "redact", Kind: policy.StepFilter, Ref: "redact",
				Next: modelStep("large", "keera-large"),
			},
		},
	})}
	for _, tt := range []struct {
		chosen, system string
	}{
		{"keera-large", "Be brief.\n\nNever name a customer."},
		{"keera-small", "Be brief."},
	} {
		t.Run(tt.chosen, func(t *testing.T) {
			h := treeHarness(t, picks(tt.chosen), p, resolved)
			// The filter would change this word if it could see the prompt.
			h.src.filters["org_1/redact"] = patternFilter(policy.FilterRule{Pattern: `customer`, Replace: "[X]"})

			if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var sent struct {
				Messages []struct{ Role, Content string } `json:"messages"`
			}
			if err := json.Unmarshal([]byte(lastSent(h)), &sent); err != nil {
				t.Fatal(err)
			}
			if len(sent.Messages) < 2 || sent.Messages[0].Role != "system" ||
				sent.Messages[0].Content != tt.system {
				t.Errorf("messages = %+v, want the system prompt %q first", sent.Messages, tt.system)
			}
		})
	}
}

// A step that cannot fail, such as a system prompt, may come first.
func TestRouterMayStartWithASystemPrompt(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "careful", Kind: policy.StepSystemPrompt, Prompt: "Be brief.",
		Next: modelStep("large", "keera-large"),
	}}
	h := treeHarness(t, picks("keera-small"), p, nil)

	resp := h.post(t, "/v1/chat/completions", secretPrompt)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("status = %d, body = %q; want an answer", resp.StatusCode, body)
	}
	if sent := lastSent(h); !strings.Contains(sent, "Be brief.") {
		t.Errorf("the model was sent %s", sent)
	}
}

// A route step that cannot decide adds its reason to the note an exceeded
// branch left, so the row still says which limit sent the request there.
func TestARouteStepKeepsTheNoteOfTheBranchBeforeIt(t *testing.T) {
	p := policy.Router{Alias: "safe", Start: &policy.Step{
		ID: "cap", Kind: policy.StepBudget, BudgetMicros: 1_000_000, Period: policy.PeriodMonth,
		Next:     modelStep("large", "keera-large"),
		Exceeded: autoStep("pick", nil),
	}}
	h := treeHarness(t, picks("keera-large"), p, nil)
	h.budgets.spentType = policy.ScopeRouter
	delete(h.src.models, "keera-picker")

	if resp := h.post(t, "/v1/chat/completions", secretPrompt); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 at the step's fallback", resp.StatusCode)
	}
	ev := h.sink.last(t)
	if !strings.Contains(ev.Note, "exceeded branch of 'cap'") ||
		!strings.Contains(ev.Note, "could not choose") {
		t.Errorf("note = %q, want both the branch and the step's reason", ev.Note)
	}
}
