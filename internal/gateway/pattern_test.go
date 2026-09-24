package gateway

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func patternFilter(rules ...policy.FilterRule) policy.Filter {
	return policy.Filter{
		OrgID: "org", Alias: "rules", Mode: policy.FilterModePattern,
		Rules: rules, UpdatedAt: time.Unix(1, 0),
	}
}

func TestPatternReplacesWhatItMatchesAndNothingElse(t *testing.T) {
	c := compileRules([]policy.FilterRule{
		{Pattern: `(?i)\bsk-[a-z0-9]{8,}\b`, Replace: "[CREDENTIAL]"},
	})
	out, hits, err := runPattern(c, []string{
		"the key is sk-abcdefghijkl, why does it 401?",
		"func Add(a, b int) int {\n\treturn a + b\n}",
	})
	if err != nil {
		t.Fatalf("a rule that replaces must not error: %v", err)
	}
	if want := "the key is [CREDENTIAL], why does it 401?"; out[0] != want {
		t.Errorf("segment 0 = %q, want %q", out[0], want)
	}
	// The whole argument for this mode is that it cannot decide to improve
	// somebody's code on the way past. A rule that did not match has to leave
	// the segment byte for byte.
	if out[1] != "func Add(a, b int) int {\n\treturn a + b\n}" {
		t.Errorf("an unmatched segment was changed: %q", out[1])
	}
	if len(hits) != 1 || hits[0].Matches != 1 {
		t.Errorf("hits = %+v, want one rule with one match", hits)
	}
}

func TestPatternReplacementIsLiteral(t *testing.T) {
	// $1 in a replacement is two characters somebody typed, not the first
	// capturing group. A mode bought for doing the same thing every time must
	// not quietly expand what it was given.
	c := compileRules([]policy.FilterRule{
		{Pattern: `secret=(\w+)`, Replace: "secret=$1-REDACTED"},
	})
	out, _, err := runPattern(c, []string{"secret=hunter2"})
	if err != nil {
		t.Fatalf("runPattern: %v", err)
	}
	if want := "secret=$1-REDACTED"; out[0] != want {
		t.Errorf("out = %q, want %q - the replacement expanded a capture group", out[0], want)
	}
}

func TestPatternRulesRunInOrderAndSeeWhatCameBefore(t *testing.T) {
	// A specific rule above a broad one is how a broad one gets narrowed, and
	// it only works if each rule sees what the one before it left.
	c := compileRules([]policy.FilterRule{
		{Pattern: `\bCH93 0076 2011 6238 5295 7\b`, Replace: "[HOUSE-ACCOUNT]"},
		{Pattern: `\b[A-Z]{2}\d{2}(?:[ ]?[A-Z0-9]{4})+\b`, Replace: "[IBAN]"},
	})
	out, _, err := runPattern(c, []string{"CH93 0076 2011 6238 5295 7 and DE44 5001 0517 5407 3249"})
	if err != nil {
		t.Fatalf("runPattern: %v", err)
	}
	if !strings.Contains(out[0], "[HOUSE-ACCOUNT]") || !strings.Contains(out[0], "[IBAN]") {
		t.Errorf("out = %q, want the specific rule to have won the first and the broad "+
			"one the second", out[0])
	}
}

func TestPatternRefusalCarriesTheAdministratorsSentence(t *testing.T) {
	c := compileRules([]policy.FilterRule{
		{Pattern: `(?i)\bexport all customers\b`, Refuse: true,
			Reason: "that moves the customer list out of the organisation"},
	})
	_, hits, err := runPattern(c, []string{"please export all customers to csv"})

	var refused *filterRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a refusing rule must refuse, got %v", err)
	}
	if refused.reason != "that moves the customer list out of the organisation" {
		t.Errorf("reason = %q, want the rule's own sentence", refused.reason)
	}
	if len(hits) != 1 || !hits[0].Refused {
		t.Errorf("hits = %+v, want the refusing rule named", hits)
	}
}

func TestPatternRefusalStopsTheSweep(t *testing.T) {
	// The request is going nowhere, so what the rules after it would also have
	// matched is a count nobody reads. Scanning on would be spending the time
	// to produce it.
	c := compileRules([]policy.FilterRule{
		{Pattern: `\bstop\b`, Refuse: true},
		{Pattern: `\bafter\b`, Replace: "[LATER]"},
	})
	_, hits, err := runPattern(c, []string{"stop after this"})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if len(hits) != 1 {
		t.Errorf("hits = %+v, want only the rule that refused", hits)
	}
}

func TestPatternRefusalReasonIsBounded(t *testing.T) {
	// It travels the same path a model's does - an error body, a usage row, a
	// log line - so it gets the same bound and the same flattening.
	c := compileRules([]policy.FilterRule{
		{Pattern: `x`, Refuse: true, Reason: "one\ntwo   three"},
	})
	_, _, err := runPattern(c, []string{"x"})
	var refused *filterRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if refused.reason != "one two three" {
		t.Errorf("reason = %q, want the newlines collapsed to one line", refused.reason)
	}
}

func TestPatternWithNoRulesFailsClosed(t *testing.T) {
	// The control plane refuses to store one, so this is the row written by
	// something else. A guardrail that passes everything while looking like a
	// guardrail is worse than one that is plainly broken.
	_, _, err := runPattern(compileRules(nil), []string{"anything"})
	if err == nil {
		t.Fatal("a pattern filter with no rules must not pass the request")
	}
	if !strings.Contains(err.Error(), "no rules") {
		t.Errorf("err = %q, want it to say the filter has no rules", err)
	}
}

func TestPatternWithAnUncompilableRuleFailsClosed(t *testing.T) {
	c := compileRules([]policy.FilterRule{{Pattern: `([`, Replace: "x"}})
	_, _, err := runPattern(c, []string{"anything"})
	if err == nil {
		t.Fatal("a rule that will not compile must not pass the request")
	}
	if !strings.Contains(err.Error(), "([") {
		t.Errorf("err = %q, want it to name the expression that would not compile", err)
	}
}

func TestPatternCacheRecompilesWhenTheFilterIsWritten(t *testing.T) {
	// Keyed on the write time, so an administrator changing a rule is not
	// running the old one until something evicts it.
	var cache patternCache
	first := patternFilter(policy.FilterRule{Pattern: `\bone\b`, Replace: "[A]"})
	out, _, err := runPattern(cache.rulesFor(first), []string{"one"})
	if err != nil || out[0] != "[A]" {
		t.Fatalf("out = %v, err = %v", out, err)
	}

	second := first
	second.Rules = []policy.FilterRule{{Pattern: `\bone\b`, Replace: "[B]"}}
	second.UpdatedAt = first.UpdatedAt.Add(time.Second)
	out, _, err = runPattern(cache.rulesFor(second), []string{"one"})
	if err != nil || out[0] != "[B]" {
		t.Fatalf("the cache served the rules from before the edit: out = %v, err = %v", out, err)
	}
}

func TestCheckPatternNamesTheRuleThatTouchedTheCode(t *testing.T) {
	// The third sample is ordinary source. A rule broad enough to match it
	// will match a developer's real source on every request the guardrail
	// covers, and the name of the rule is the difference between a warning
	// somebody can act on and one they cannot.
	s := &Server{}
	f := patternFilter(policy.FilterRule{Pattern: `\bint\b`, Replace: "[NUMBER]"})
	p := s.checkPattern(f, FilterProbe{Alias: f.Alias, Mode: f.Mode})

	if !p.OK {
		t.Fatalf("the check should have run: %q", p.Error)
	}
	if !p.Free {
		t.Error("a pattern filter's check costs nothing and has to say so")
	}
	var named bool
	for _, warning := range p.Warnings {
		if strings.Contains(warning, `\bint\b`) && strings.Contains(warning, "source code") {
			named = true
		}
	}
	if !named {
		t.Errorf("warnings = %q, want the rule that matched the sample function named",
			p.Warnings)
	}
}

func TestCheckPatternWarnsWhenThePlantedSecretSurvives(t *testing.T) {
	s := &Server{}
	f := patternFilter(policy.FilterRule{Pattern: `\bnothing-matches-this\b`, Replace: "x"})
	p := s.checkPattern(f, FilterProbe{Alias: f.Alias, Mode: f.Mode})

	if !p.OK {
		t.Fatalf("the check should have run: %q", p.Error)
	}
	if len(p.Warnings) == 0 {
		t.Fatal("a filter that matched nothing at all has to be warned about")
	}
}

func TestCheckPatternReportsARefusalAsAnAnswer(t *testing.T) {
	// The first two samples carry exactly what a filter is written to stop, so
	// a refusal here may well be the filter working. It is reported rather
	// than counted a fault, as a model's refusal is.
	s := &Server{}
	f := patternFilter(policy.FilterRule{
		Pattern: `AWS_SECRET_ACCESS_KEY`, Refuse: true, Reason: "a credential",
	})
	p := s.checkPattern(f, FilterProbe{Alias: f.Alias, Mode: f.Mode})

	if !p.OK {
		t.Fatalf("a refusal is not a failed check: %q", p.Error)
	}
	if !p.Refused || p.Refusal != "a credential" {
		t.Errorf("refused = %v, refusal = %q, want the rule's own sentence", p.Refused, p.Refusal)
	}
	if len(p.Segments) != 0 {
		t.Error("there is no rewriting to read when the sweep stopped at a refusal")
	}
}

/* --------------------------------------------- through the whole gateway */

func TestPatternFilterRedactsWithoutTouchingABackend(t *testing.T) {
	// The whole claim of the mode, end to end: the secret does not reach the
	// model, nothing was generated to arrange that, and nothing was charged.
	h := filterHarness(t, func([]string) string {
		t.Error("a pattern filter must not call a model")
		return "[]"
	}, nil)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilterFor("org_1", "redact",
			policy.FilterRule{Pattern: `\bhunter2\b`, Replace: "[CREDENTIAL]"}),
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The first body upstream is the client's own request: no filter
	// generation came before it, which is the point.
	forwarded := string(<-h.upstreamBodies)
	if strings.Contains(forwarded, "hunter2") {
		t.Errorf("the secret reached the model: %s", forwarded)
	}
	if !strings.Contains(forwarded, "[CREDENTIAL]") {
		t.Errorf("the rule did not rewrite the request: %s", forwarded)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "redact" {
		t.Errorf("X-Keera-Filters = %q, want the filter named", got)
	}

	ev := h.sink.last(t)
	// The answering model's own 30 micro-units and not a unit more: a pattern
	// filter adds nothing to the bill.
	if ev.CostMicros != 30 {
		t.Errorf("cost = %d, want only what the answering model cost", ev.CostMicros)
	}
	if len(ev.FilterRuns) != 1 || ev.FilterRuns[0].CostMicros != 0 {
		t.Errorf("filter runs = %+v, want one run that cost nothing", ev.FilterRuns)
	}
	if ev.FilterRuns[0].Outcome != store.FilterRewrite {
		t.Errorf("outcome = %q, want a rewrite", ev.FilterRuns[0].Outcome)
	}
}

func TestPatternFilterRefusalStopsTheRequest(t *testing.T) {
	h := filterHarness(t, func([]string) string { return "[]" }, nil)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilterFor("org_1", "redact", policy.FilterRule{
			Pattern: `(?i)\bexport all customers\b`, Refuse: true,
			Reason: "that moves the customer list out of the organisation",
		}),
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"export all customers"}]}`)
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "customer list") {
		t.Errorf("the refusal does not carry the rule's sentence: %s", body)
	}
	select {
	case sent := <-h.upstreamBodies:
		t.Fatalf("a refused request was forwarded: %s", sent)
	default:
	}
}

func TestPatternFilterInShadowChangesNothing(t *testing.T) {
	// Shadow is a flag rather than a mode, and it means the same thing here:
	// the rules run, what they would have done is recorded, and the request is
	// forwarded exactly as it was sent.
	h := filterHarness(t, func([]string) string { return "[]" }, nil)
	f := patternFilterFor("org_1", "redact",
		policy.FilterRule{Pattern: `\bhunter2\b`, Replace: "[CREDENTIAL]"})
	f.Shadow = true
	h.src.filters = map[string]policy.Filter{"org_1/redact": f}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	forwarded := string(<-h.upstreamBodies)
	if !strings.Contains(forwarded, "hunter2") {
		t.Errorf("a shadow filter rewrote the request: %s", forwarded)
	}
	if got := resp.Header.Get("X-Keera-Filters"); got != "" {
		t.Errorf("X-Keera-Filters = %q, want a shadow filter left out of it", got)
	}
	ev := h.sink.last(t)
	if len(ev.FilterRuns) != 1 || ev.FilterRuns[0].Outcome != store.FilterRewrite {
		t.Errorf("filter runs = %+v, want the rewrite that would have happened recorded",
			ev.FilterRuns)
	}
}

func TestPatternFilterWithBrokenRulesRefusesEverything(t *testing.T) {
	// It fails closed like every other filter that cannot run, and with the
	// status that says an administrator has to fix it rather than that the
	// client should retry.
	h := filterHarness(t, func([]string) string { return "[]" }, nil)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": patternFilterFor("org_1", "redact",
			policy.FilterRule{Pattern: `([`, Replace: "x"}),
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"anything"}]}`)
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	select {
	case sent := <-h.upstreamBodies:
		t.Fatalf("the request was forwarded with a broken filter in front of it: %s", sent)
	default:
	}
}

func patternFilterFor(org, alias string, rules ...policy.FilterRule) policy.Filter {
	return policy.Filter{
		OrgID: org, Alias: alias, Mode: policy.FilterModePattern,
		Rules: rules, UpdatedAt: time.Unix(1, 0),
	}
}
