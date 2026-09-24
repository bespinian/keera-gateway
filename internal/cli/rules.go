package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The text form of a pattern filter's rules and of a size router's ceilings.
// The wire carries them as JSON; people edit and review them as text, so the
// command line reads and prints that text.

// ruleSeparator divides a rule's expression from what it becomes. The last
// one counts, because '=>' can appear in an expression but rarely in a
// replacement. Both sides are trimmed, so arrows can be lined up.
const ruleSeparator = "=>"

// ruleRefuseToken is the right-hand side that drops the request instead of
// replacing the match. It is the word a model filter answers with too.
const ruleRefuseToken = "REFUSE"

// parseRules reads a pattern filter's rules out of their text form. One rule
// per line, blank lines and '#' comments ignored:
//
//	# what must never leave, whatever it is asked for
//	(?i)\bsk-[a-z0-9]{20,}\b        => [CREDENTIAL]
//	\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b => [IBAN]
//	(?i)\bexport all customers\b    => REFUSE: that moves the customer list out
//
// A refusing rule may carry the sentence the sender is given, after a colon.
// Everything else is a replacement, inserted literally.
func parseRules(text string) ([]policy.FilterRule, error) {
	var out []policy.FilterRule
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		at := strings.LastIndex(trimmed, ruleSeparator)
		if at < 0 {
			return nil, fmt.Errorf("line %d has no %q: a rule is an expression, then %q, "+
				"then what each match becomes - or %q to drop the request instead:\n  %s",
				i+1, ruleSeparator, ruleSeparator, ruleRefuseToken, trimmed)
		}
		rule := policy.FilterRule{
			Pattern: strings.TrimSpace(trimmed[:at]),
		}
		answer := strings.TrimSpace(trimmed[at+len(ruleSeparator):])
		if reason, ok := cutRefusal(answer); ok {
			rule.Refuse, rule.Reason = true, reason
		} else {
			rule.Replace = answer
		}
		if err := rule.Valid(); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, rule)
	}
	if len(out) == 0 {
		return nil, errors.New("no rules were read; a pattern filter needs at least one, " +
			"and a rule is an expression, then " + ruleSeparator + ", then what each match " +
			"becomes")
	}
	return out, nil
}

// cutRefusal reads a right-hand side that refuses, and the sentence after it.
func cutRefusal(s string) (reason string, ok bool) {
	if len(s) < len(ruleRefuseToken) ||
		!strings.EqualFold(s[:len(ruleRefuseToken)], ruleRefuseToken) {
		return "", false
	}
	rest := s[len(ruleRefuseToken):]
	// The token alone, or followed by a separator and a sentence. A
	// replacement that only starts with the word is not a refusal.
	switch {
	case rest == "":
		return "", true
	case strings.HasPrefix(rest, "D"), strings.HasPrefix(rest, "d"):
		// REFUSED as well as REFUSE, as a gate accepts both.
		rest = rest[1:]
		if rest == "" {
			return "", true
		}
	}
	for _, sep := range []string{":", " ", "\t"} {
		if after, found := strings.CutPrefix(rest, sep); found {
			return strings.TrimSpace(after), true
		}
	}
	return "", false
}

// formatRules prints rules in the form parseRules reads, arrows lined up.
func formatRules(rules []policy.FilterRule) []string {
	width := 0
	for _, r := range rules {
		width = max(width, len(r.Pattern))
	}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		answer := r.Replace
		if r.Refuse {
			answer = ruleRefuseToken
			if r.Reason != "" {
				answer += ": " + r.Reason
			}
		}
		out = append(out, fmt.Sprintf("%-*s %s %s", width, r.Pattern, ruleSeparator, answer))
	}
	return out
}

// parseDestinations reads a router's destinations, and a size router's
// ceilings beside them:
//
//	--destinations keera-speed:4000,keera-frontier
//
// The one with no number takes everything larger.
func parseDestinations(spec string) ([]string, map[string]int, error) {
	var (
		aliases  []string
		ceilings map[string]int
	)
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		alias, size, bounded := strings.Cut(part, ":")
		alias = strings.TrimSpace(alias)
		aliases = append(aliases, alias)
		if !bounded {
			continue
		}
		n, err := parseTokenCount(strings.TrimSpace(size))
		if err != nil {
			return nil, nil, fmt.Errorf("the ceiling on %q: %w", alias, err)
		}
		if ceilings == nil {
			ceilings = map[string]int{}
		}
		ceilings[alias] = n
	}
	return aliases, ceilings, nil
}

// parseTokenCount reads a ceiling in tokens, where 'k' means a thousand.
func parseTokenCount(s string) (int, error) {
	mult := 1
	if rest, ok := strings.CutSuffix(strings.ToLower(s), "k"); ok {
		s, mult = rest, 1000
	}
	n, err := strconv.Atoi(s)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%q is not a number of tokens; write '4000' or '4k'", s)
	case n <= 0:
		return 0, errors.New("a ceiling is a positive number of tokens; leave it off " +
			"the destination that should take what the others will not")
	}
	return n * mult, nil
}

// formatDestinations prints destinations with their ceilings, in the form
// parseDestinations reads.
func formatDestinations(rt policy.Router) string {
	out := make([]string, 0, len(rt.Destinations))
	for _, alias := range rt.Destinations {
		if n, bounded := rt.Ceiling(alias); bounded {
			alias += ":" + formatTokenCount(n)
		}
		out = append(out, alias)
	}
	return strings.Join(out, ", ")
}

// formatTokenCount is parseTokenCount's inverse, so '32k' prints as '32k'.
func formatTokenCount(n int) string {
	if n >= 1000 && n%1000 == 0 {
		return strconv.Itoa(n/1000) + "k"
	}
	return strconv.Itoa(n)
}
