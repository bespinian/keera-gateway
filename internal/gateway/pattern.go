package gateway

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A pattern filter is a filter without a model: it runs a list of regular
// expressions over the same segments, in this process, with no GPU and no
// cost.
//
// It is for secrets that have a shape: credentials, connection strings,
// IBANs, card numbers. A model is a slower, dearer and less reliable way to
// match those. Because it is free it can cover a whole organisation; a rule
// cannot decide to "improve" someone's code; and it answers the same way every
// time, so its check shows what production does.
//
// What it cannot do is read: a customer's name in a sentence has no shape.
// That needs one of the model modes.

// compiled is one pattern filter's rules, ready to run. Filters change rarely
// and run on every request, so the rules are compiled once and kept.
type compiled struct {
	rules []compiledRule
	// err is a rule that would not compile, the one way this mode fails. It is
	// kept so a broken filter fails the same way each time without being
	// recompiled.
	err error
}

type compiledRule struct {
	re   *regexp.Regexp
	rule policy.FilterRule
}

// patternCache holds the compiled rules of every pattern filter this process
// has run, keyed by the filter and when it was last written. An edit changes
// the key, so nothing has to invalidate the cache.
type patternCache struct{ m sync.Map }

// rulesFor returns the compiled rules of one pattern filter.
func (c *patternCache) rulesFor(f policy.Filter) compiled {
	key := f.OrgID + "\x00" + f.Alias + "\x00" + f.UpdatedAt.UTC().String()
	if got, ok := c.m.Load(key); ok {
		return got.(compiled)
	}
	out := compileRules(f.Rules)
	c.m.Store(key, out)
	return out
}

// compileRules turns an administrator's rules into something that can be run.
// The control plane validates rules when they are written, so a failure here
// means the row was changed behind its back. It fails closed.
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

// patternHit is one rule that fired, for a check to report. Nothing like it is
// kept for real requests.
type patternHit struct {
	// Rule is the expression that matched: the line to change.
	Rule string `json:"rule"`
	// Matches is how many times it matched across every segment.
	Matches int `json:"matches"`
	// Refused says this is the rule that dropped the request rather than one
	// that replaced anything.
	Refused bool `json:"refused,omitempty"`
}

// runPattern applies one pattern filter's rules to the segments.
//
// The rules run in order and each sees what the one before left, so a
// specific rule above a broad one can narrow it.
//
// A refusal is a filterRefusedError, as a model's is, so it takes the same
// path through outcomes, statuses, logs and shadow mode.
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
				// Stop here: the request is going nowhere.
				return nil, append(hits, patternHit{
					Rule: cr.rule.Pattern, Matches: matches, Refused: true,
				}), &filterRefusedError{reason: ruleReason(cr.rule)}
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

// ruleReason is the sentence a refusing rule gives the sender. It is the
// administrator's wording, but it is bounded like a model's because it goes
// the same way, into an error body and a usage row.
func ruleReason(r policy.FilterRule) string {
	return sanitizeReason(r.Reason)
}

/* ------------------------------------------------------------ checking one */

// patternCheckWarnings says what a pattern filter that ran still got wrong:
// it left what it should remove, or touched what it should leave. For the
// second, it names the rule.
func patternCheckWarnings(out []string, hits []patternHit, codeHits []patternHit) []string {
	var warnings []string
	joined := strings.Join(out[:2], "\n")
	for _, secret := range checkSecrets {
		if strings.Contains(joined, secret) {
			warnings = append(warnings, "the sample credential or account number came back "+
				"untouched. The filter ran, but no rule in it matches what it was given to "+
				"remove.")
			break
		}
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
	if len(s) > 60 {
		return "'" + s[:57] + "…'"
	}
	return "'" + s + "'"
}
