package gateway

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// FilterProbe is what one filter check found.
type FilterProbe struct {
	Alias string `json:"alias"`
	// Mode says which results to read: segments for a rewrite, verdicts for a
	// gate.
	Mode  policy.FilterMode `json:"mode"`
	Model string            `json:"model"`
	// Shadow says the filter is not enforcing, so nothing it does here would
	// stop a request.
	Shadow bool `json:"shadow,omitempty"`
	// Segments is the sample before and after.
	Segments []FilterSegment `json:"segments,omitempty"`
	TotalMS  int64           `json:"total_ms,omitempty"`
	// CostMicros is what this run cost, which is also what the filter adds to
	// every request.
	CostMicros int64  `json:"cost_micros,omitempty"`
	Error      string `json:"error,omitempty"`
	// Refused says the filter refused the sample instead of rewriting it, and
	// Refusal is its sentence. That is not a fault, but there is no rewrite.
	Refused bool   `json:"refused,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	// Verdicts is a gate's answer to each half of the sample: one that should
	// be refused, and one that should not.
	Verdicts []FilterVerdict `json:"verdicts,omitempty"`
	// Hits is which pattern rules fired and how often.
	Hits []patternHit `json:"hits,omitempty"`
	// Free says the run cost nothing, so a zero cost is not read as missing.
	Free bool `json:"free,omitempty"`
	// Warnings are problems that are not failures, such as a secret left in or
	// ordinary code rewritten.
	Warnings []string `json:"warnings,omitempty"`
	OK       bool     `json:"ok"`
}

// FilterVerdict is one gate run: what was sent, what it answered, and what it
// should have answered.
type FilterVerdict struct {
	Sent          []string `json:"sent"`
	Refused       bool     `json:"refused"`
	Reason        string   `json:"reason,omitempty"`
	ExpectRefusal bool     `json:"expect_refusal"`
	// Confidence is this verdict's share of probability, from 0 to 1. Near 0.5
	// the gate cannot tell the samples apart. 0 means unknown: the backend sent
	// no logprobs.
	Confidence float64 `json:"confidence,omitempty"`
}

// FilterSegment is one sample text before and after the filter.
type FilterSegment struct {
	Before  string `json:"before"`
	After   string `json:"after"`
	Changed bool   `json:"changed"`
}

// filterCheckSample is what a check sends through a filter. The first two hold
// what a redaction filter should remove: a credential, and a named client with
// an account number. The third is ordinary code no filter should touch.
var filterCheckSample = []string{
	"Deploy it with AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY " +
		"and tell me why the pod still restarts.",
	"The nightly reconciliation for Helvetia Versicherung failed on account " +
		"CH93 0076 2011 6238 5295 7 - what would you check first?",
	"func Add(a, b int) int {\n\treturn a + b\n}",
}

// The sample split for a gate, which answers once per request. Sent together,
// the credential would hide whether ordinary work gets through.
var (
	gateCheckSensitive = filterCheckSample[:2]
	gateCheckOrdinary  = filterCheckSample[2:]
)

// checkSecrets must not survive a passing filter.
var checkSecrets = []string{"wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", "CH93 0076 2011 6238 5295 7"}

func leftSecret(out []string) bool {
	joined := strings.Join(out[:2], "\n")
	return slices.ContainsFunc(checkSecrets, func(s string) bool { return strings.Contains(joined, s) })
}

// CheckFilter runs one filter over a fixed sample and reports what it did,
// since filters fail quietly. Like CheckModel, it skips rate limits, budgets
// and usage events, but a model on the deployment's key needs credit and is
// billed.
func (s *Server) CheckFilter(ctx context.Context, f policy.Filter) FilterProbe {
	p := FilterProbe{Alias: f.Alias, Mode: f.Mode, Model: f.Model, Shadow: f.Shadow}
	if !f.UsesModel() {
		return s.checkPattern(f, p)
	}
	m, why := s.filterModel(f)
	if why != "" {
		// A broken filter in shadow refuses nothing, so say so.
		if f.Shadow {
			p.Error = why + ", so this filter cannot run; it is in shadow, so requests go on as sent"
		} else {
			p.Error = why + ", so this filter would refuse every request it covers"
		}
		return p
	}
	if why := s.checkCredit(f.OrgID, m); why != "" {
		p.Error = why
		return p
	}
	if f.Mode.Gates() {
		return s.checkGate(ctx, f, m, p)
	}

	start := time.Now()
	out, cost, err := s.runFilter(ctx, f, m, filterCheckSample)
	p.TotalMS = time.Since(start).Milliseconds()
	p.CostMicros = cost.micros
	s.billCheck(f.OrgID, cost.bills)
	if err != nil {
		if refused, ok := errors.AsType[*filterRefusedError](err); ok {
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

func sampleSegments(out []string) []FilterSegment {
	segments := make([]FilterSegment, 0, len(filterCheckSample))
	for i, before := range filterCheckSample {
		segments = append(segments, FilterSegment{
			Before: before, After: out[i], Changed: out[i] != before,
		})
	}
	return segments
}

// checkGate sends each half of the sample through a gate as its own request.
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
		s.billCheck(f.OrgID, cost.bills)
		v := FilterVerdict{
			Sent: half.sent, ExpectRefusal: half.expectRefusal, Confidence: cost.confidence,
		}
		refused, isRefusal := errors.AsType[*filterRefusedError](err)
		switch {
		case isRefusal:
			v.Refused, v.Reason = true, refused.reason
		case err != nil:
			// Such a gate would refuse everything.
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
// each rule did, so the check can name the line to change.
func (s *Server) checkPattern(f policy.Filter, p FilterProbe) FilterProbe {
	p.Free = true
	c := s.patterns.rulesFor(f)

	start := time.Now()
	out, hits, err := runPattern(c, filterCheckSample)
	// Run the code sample alone too, since a refusal earlier stops the sweep.
	_, codeHits, _ := runPattern(c, gateCheckOrdinary)
	p.TotalMS = time.Since(start).Milliseconds()
	p.Hits = hits

	refused, isRefusal := errors.AsType[*filterRefusedError](err)
	switch {
	case isRefusal:
		// An answer, not a fault, as with a model's refusal.
		p.OK = true
		p.Refused, p.Refusal = true, refused.reason
		p.Warnings = refusingCodeWarnings(codeHits)
		return p
	case err != nil:
		// The rules do not compile, so every real request is refused.
		p.Error = err.Error()
		return p
	}

	p.Segments = sampleSegments(out)
	p.OK = true
	p.Warnings = patternCheckWarnings(out, hits, codeHits)
	return p
}

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

// gateWarnings says what a gate that answered both halves still got wrong,
// since its mistakes are otherwise invisible.
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

// filterWarnings says what a run that worked still got wrong.
func filterWarnings(out []string) []string {
	var warnings []string
	if leftSecret(out) {
		warnings = append(warnings, "the sample credential or account number came back "+
			"untouched. The filter ran, but this instruction did not remove what it was "+
			"given to remove.")
	}
	if len(out) > 2 && out[2] != filterCheckSample[2] {
		warnings = append(warnings, "the sample function was rewritten. A filter that edits "+
			"ordinary code changes what developers ask the model about, and nothing "+
			"downstream will report that it did.")
	}
	return warnings
}
