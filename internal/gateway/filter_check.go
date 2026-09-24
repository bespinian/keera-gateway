package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// FilterProbe is what one filter check found.
type FilterProbe struct {
	Alias string `json:"alias"`
	// Mode says which of the results below to read: a rewrite filter has
	// segments, a gate has verdicts.
	Mode  policy.FilterMode `json:"mode"`
	Model string            `json:"model"`
	// Shadow says the filter is not enforcing: a refusal here would not
	// happen, and a filter that cannot run stops nothing.
	Shadow bool `json:"shadow,omitempty"`
	// Segments is the sample before and after, so an administrator can read
	// what the filter did.
	Segments []FilterSegment `json:"segments,omitempty"`
	TotalMS  int64           `json:"total_ms,omitempty"`
	// CostMicros is what this run cost, which is also what the filter adds to
	// every request it guards.
	CostMicros int64  `json:"cost_micros,omitempty"`
	Error      string `json:"error,omitempty"`
	// Refused says the filter refused the sample instead of rewriting it, and
	// Refusal is its sentence. That is not a fault: the sample carries what a
	// filter may be written to refuse. It does mean there is no rewrite to
	// read.
	Refused bool   `json:"refused,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	// Verdicts is how a gate answered each half of the sample: one request
	// that should be refused, and one that should not.
	Verdicts []FilterVerdict `json:"verdicts,omitempty"`
	// Hits is which of a pattern filter's rules fired and how often, which
	// names the line to change when it does the wrong thing.
	Hits []patternHit `json:"hits,omitempty"`
	// Free says this run cost nothing and used no GPU, so a zero cost is not
	// mistaken for a missing measurement.
	Free bool `json:"free,omitempty"`
	// Warnings are problems that are not failures, such as a filter that left
	// the planted secret alone or rewrote the ordinary code.
	Warnings []string `json:"warnings,omitempty"`
	OK       bool     `json:"ok"`
}

// FilterVerdict is one gate run: what was sent, what it answered, and what the
// sample was meant to get. Both are shown, since a one-word answer means
// nothing without the question.
type FilterVerdict struct {
	Sent    []string `json:"sent"`
	Refused bool     `json:"refused"`
	Reason  string   `json:"reason,omitempty"`
	// ExpectRefusal is what this half of the sample was put in to provoke.
	ExpectRefusal bool `json:"expect_refusal"`
	// Confidence is the share of probability this verdict took against the
	// other, from 0 to 1. A gate near 0.5 cannot tell the samples apart and
	// may flip with a small change. Zero means the backend served no logprobs,
	// so it is unknown. See choice.go.
	Confidence float64 `json:"confidence,omitempty"`
}

// FilterSegment is one sample text as it went in and as it came back.
type FilterSegment struct {
	Before  string `json:"before"`
	After   string `json:"after"`
	Changed bool   `json:"changed"`
}

// filterCheckSample is what a check sends through a filter. The first two
// carry what a redaction filter should remove: a credential, and a named
// client with an account number. The third is ordinary code no filter should
// touch, which catches an instruction eager enough to rewrite developers' code.
var filterCheckSample = []string{
	"Deploy it with AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY " +
		"and tell me why the pod still restarts.",
	"The nightly reconciliation for Helvetia Versicherung failed on account " +
		"CH93 0076 2011 6238 5295 7 - what would you check first?",
	"func Add(a, b int) int {\n\treturn a + b\n}",
}

// The same sample, split for a gate. A gate answers once per request, so the
// halves go separately: together they would be refused for the credential,
// and say nothing about whether ordinary work gets through.
var (
	gateCheckSensitive = filterCheckSample[:2]
	gateCheckOrdinary  = filterCheckSample[2:]
)

// checkSecrets are the strings a passing filter must not have left behind.
var checkSecrets = []string{"wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", "CH93 0076 2011 6238 5295 7"}

// CheckFilter runs one filter over a fixed sample and reports what it did.
//
// Filters fail quietly: a model too small for the format refuses everything,
// and an eager instruction deletes part of a request. Neither shows until
// someone complains.
//
// Like CheckModel, this is not a tenant's request: no rate limit, budget,
// billing or usage event. Its cost is reported, since every guarded request
// will pay the same.
func (s *Server) CheckFilter(ctx context.Context, f policy.Filter, m policy.Model) FilterProbe {
	p := FilterProbe{Alias: f.Alias, Mode: f.Mode, Model: f.Model, Shadow: f.Shadow}
	if p.Mode == "" {
		p.Mode = policy.FilterModeRewrite
	}
	// A pattern filter needs no backend, and its result is exactly what
	// production will do.
	if !f.UsesModel() {
		return s.checkPattern(f, p)
	}
	switch {
	case !m.Enabled:
		p.Error = "the model '" + f.Model + "' is disabled, so this filter would refuse " +
			"every request it covers"
		return p
	case m.Kind != policy.KindChat:
		p.Error = "the model '" + f.Model + "' is a " + string(m.Kind) + " model; a filter " +
			"reads text and answers in words, which only a chat model does"
		return p
	case len(m.Backends) == 0:
		p.Error = "the model '" + f.Model + "' has no backend configured"
		return p
	}
	if !f.Mode.Rewrites() {
		return s.checkGate(ctx, f, m, p)
	}

	start := time.Now()
	out, cost, err := s.runFilter(ctx, f, m, filterCheckSample)
	p.TotalMS = time.Since(start).Milliseconds()
	p.CostMicros = cost.micros
	if err != nil {
		var refused *filterRefusedError
		if errors.As(err, &refused) {
			p.OK = true
			p.Refused, p.Refusal = true, refused.reason
			return p
		}
		p.Error = err.Error()
		return p
	}

	p.Segments = sampleSegments(out)
	p.OK = true
	p.Warnings = filterWarnings(out)
	return p
}

// sampleSegments pairs the check sample with what came back.
func sampleSegments(out []string) []FilterSegment {
	segments := make([]FilterSegment, 0, len(filterCheckSample))
	for i, before := range filterCheckSample {
		segments = append(segments, FilterSegment{
			Before: before, After: out[i], Changed: out[i] != before,
		})
	}
	return segments
}

// checkGate puts both halves of the sample through a gate, one request each.
func (s *Server) checkGate(ctx context.Context, f policy.Filter, m policy.Model,
	p FilterProbe,
) FilterProbe {
	halves := []struct {
		sent          []string
		expectRefusal bool
	}{
		{gateCheckSensitive, true},
		{gateCheckOrdinary, false},
	}

	start := time.Now()
	for _, half := range halves {
		_, cost, err := s.runFilter(ctx, f, m, half.sent)
		p.CostMicros += cost.micros
		v := FilterVerdict{
			Sent: half.sent, ExpectRefusal: half.expectRefusal, Confidence: cost.confidence,
		}
		var refused *filterRefusedError
		switch {
		case errors.As(err, &refused):
			v.Refused, v.Reason = true, refused.reason
		case err != nil:
			// One unreadable half is enough: such a gate refuses everything.
			p.Error = err.Error()
			p.TotalMS = time.Since(start).Milliseconds()
			return p
		}
		p.Verdicts = append(p.Verdicts, v)
	}
	p.TotalMS = time.Since(start).Milliseconds()
	p.OK = true
	p.Warnings = gateWarnings(p.Verdicts)
	return p
}

// checkPattern runs a pattern filter's rules over the sample and reports what
// each rule did, so the check can name the line to change. A rule that
// matches the ordinary code will match real code on every request.
func (s *Server) checkPattern(f policy.Filter, p FilterProbe) FilterProbe {
	p.Free = true
	c := s.patterns.rulesFor(f)

	start := time.Now()
	out, hits, err := runPattern(c, filterCheckSample)
	// The code sample on its own too, because a refusal on the first two
	// samples stops the sweep before it is reached.
	_, codeHits, _ := runPattern(c, gateCheckOrdinary)
	p.TotalMS = time.Since(start).Milliseconds()
	p.Hits = hits

	var refused *filterRefusedError
	switch {
	case errors.As(err, &refused):
		// An answer, not a fault, as with a model's refusal. There is no
		// rewrite to read, because the sweep stopped at the match.
		p.OK = true
		p.Refused, p.Refusal = true, refused.reason
		p.Warnings = refusingCodeWarnings(codeHits)
		return p
	case err != nil:
		// The rules do not compile, which refuses every real request.
		p.Error = err.Error()
		return p
	}

	p.Segments = sampleSegments(out)
	p.OK = true
	p.Warnings = patternCheckWarnings(out, hits, codeHits)
	return p
}

// refusingCodeWarnings names the rules that refuse ordinary source code.
func refusingCodeWarnings(codeHits []patternHit) []string {
	var warnings []string
	for _, hit := range codeHits {
		if hit.Refused {
			warnings = append(warnings, "the rule "+quoteRule(hit.Rule)+" refuses "+
				"ordinary source code. A rule this broad stops the work of everybody "+
				"the guardrail covers, and each of them is told only that a guardrail "+
				"refused them.")
		}
	}
	return warnings
}

// gateWarnings says what a gate that answered both halves still got wrong. A
// gate's mistakes are invisible both ways: it lets out what it should stop,
// or stops ordinary work and calls it policy.
func gateWarnings(verdicts []FilterVerdict) []string {
	var warnings []string
	for _, v := range verdicts {
		switch {
		case v.ExpectRefusal && !v.Refused:
			warnings = append(warnings, "the sample carrying a credential and a named client "+
				"with an account number was allowed through. The gate ran, but this "+
				"instruction does not stop what it was given to stop.")
		case !v.ExpectRefusal && v.Refused:
			warnings = append(warnings, "ordinary source code was refused. A gate this eager "+
				"stops the work of everybody the guardrail covers, and each of them is told "+
				"only that a guardrail refused them.")
		}
	}
	return warnings
}

// filterWarnings says what a run that worked mechanically still got wrong.
func filterWarnings(out []string) []string {
	var warnings []string
	joined := strings.Join(out[:2], "\n")
	for _, secret := range checkSecrets {
		if strings.Contains(joined, secret) {
			warnings = append(warnings, "the sample credential or account number came back "+
				"untouched. The filter ran, but this instruction did not remove what it was "+
				"given to remove.")
			break
		}
	}
	if len(out) > 2 && out[2] != filterCheckSample[2] {
		warnings = append(warnings, "the sample function was rewritten. A filter that edits "+
			"ordinary code changes what developers ask the model about, and nothing "+
			"downstream will report that it did.")
	}
	return warnings
}
