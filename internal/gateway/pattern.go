package gateway

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A pattern filter runs regular expressions over the segments, in process,
// with no model and no cost. It is for secrets with a shape: credentials,
// connection strings, IBANs, card numbers. Being free, it can cover a whole
// organisation; a rule cannot "improve" someone's code; and it is
// deterministic, so its check shows what production does.
//
// It cannot read: a customer's name in a sentence has no shape. That needs a
// model mode.

// compiled is one pattern filter's rules. Filters change rarely and run on
// every request, so the rules are compiled once and kept.
type compiled struct {
	rules []compiledRule
	// err is a rule that would not compile. It is kept so a broken filter
	// fails the same way each time without being recompiled.
	err error
}

type compiledRule struct {
	re   *regexp.Regexp
	rule policy.FilterRule
}

// patternCache holds compiled rules, one entry per filter. An edit changes
// the filter's write time and the next request recompiles in place, so old
// rules do not pile up.
type patternCache struct{ m sync.Map }

type patternEntry struct {
	updatedAt time.Time
	compiled  compiled
}

func (c *patternCache) rulesFor(f policy.Filter) compiled {
	key := f.OrgID + "\x00" + f.Alias
	if got, ok := c.m.Load(key); ok && got.(patternEntry).updatedAt.Equal(f.UpdatedAt) {
		return got.(patternEntry).compiled
	}
	out := compileRules(f.Rules)
	c.m.Store(key, patternEntry{updatedAt: f.UpdatedAt, compiled: out})
	return out
}

// compileRules fails closed. The control plane validates rules on write, so a
// failure here means the row was changed behind its back.
func compileRules(rules []policy.FilterRule) compiled {
	if len(rules) == 0 {
		// The control plane refuses to store this. A guardrail that passes
		// everything is worse than one that is plainly broken.
		return compiled{err: errors.New("it has no rules, so it would read every request " +
			"the guardrail covers and change nothing")}
	}
	out := compiled{rules: make([]compiledRule, 0, len(rules))}
	for _, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return compiled{err: fmt.Errorf("its rule %q is not a valid regular expression: %w",
				r.Pattern, err)}
		}
		out.rules = append(out.rules, compiledRule{re: re, rule: r})
	}
	return out
}

// patternHit is one rule that fired, for a check to report. Real requests
// keep none.
type patternHit struct {
	// Rule is the expression that matched: the line to change.
	Rule string `json:"rule"`
	// Matches is how many times it matched across every segment.
	Matches int `json:"matches"`
	// Refused says this rule dropped the request rather than replaced text.
	Refused bool `json:"refused,omitempty"`
}

// runPattern applies one pattern filter's rules to the segments.
//
// The rules run in order, each on what the one before left, so a specific
// rule above a broad one can narrow it. A refusal is a filterRefusedError, as
// a model's is, so it takes the same path through outcomes, logs and shadow
// mode.
func runPattern(c compiled, texts []string) ([]string, []patternHit, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	out := make([]string, len(texts))
	copy(out, texts)

	var hits []patternHit
	for _, cr := range c.rules {
		matches := 0
		for i, s := range out {
			found := len(cr.re.FindAllStringIndex(s, -1))
			if found == 0 {
				continue
			}
			matches += found
			if cr.rule.Refuse {
				// The reason is the administrator's wording, but it is bounded
				// like a model's: it goes into an error body and a usage row.
				return nil, append(hits, patternHit{
					Rule: cr.rule.Pattern, Matches: matches, Refused: true,
				}), &filterRefusedError{reason: sanitizeReason(cr.rule.Reason)}
			}
			// Literal, not expanded: see policy.FilterRule.Replace.
			out[i] = cr.re.ReplaceAllLiteralString(s, cr.rule.Replace)
		}
		if matches > 0 {
			hits = append(hits, patternHit{Rule: cr.rule.Pattern, Matches: matches})
		}
	}
	return out, hits, nil
}

// patternCheckWarnings says what a pattern filter still got wrong: it left
// what it should remove, or touched what it should leave (naming the rule).
func patternCheckWarnings(out []string, hits []patternHit, codeHits []patternHit) []string {
	var warnings []string
	if leftSecret(out) {
		warnings = append(warnings, "the sample credential or account number came back "+
			"untouched. The filter ran, but no rule in it matches what it was given to "+
			"remove.")
	}
	for _, hit := range codeHits {
		warnings = append(warnings, "the rule "+quoteRule(hit.Rule)+" matched the sample "+
			"function, which is ordinary source code nothing should touch. A rule that edits "+
			"code changes what developers ask the model about, and nothing downstream will "+
			"report that it did.")
	}
	if len(hits) == 0 && len(codeHits) == 0 {
		warnings = append(warnings, "not one rule matched anything in the sample. That is "+
			"either a filter written for something the sample does not carry, or a list of "+
			"expressions that match nothing at all - and the two look identical from here.")
	}
	return warnings
}

// quoteRule renders an expression for a sentence somebody reads.
func quoteRule(s string) string {
	// With the ellipsis, a cut rule is 60 bytes too.
	if len(s) > 60 {
		s = shorten(s, 60-len(ellipsis))
	}
	return "'" + s + "'"
}
