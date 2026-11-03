package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A pattern filter runs regular expressions over the segments, in process,
// with no model and no cost. It is for secrets with a shape: credentials,
// connection strings, IBANs, card numbers. It is free and deterministic, but
// cannot catch what has no shape, such as a name; that needs a model mode.

// compiled is one pattern filter's rules, compiled once and kept.
type compiled struct {
	rules []compiledRule
	// err is a rule that would not compile, kept so a broken filter fails
	// the same way without recompiling.
	err error
	// clean remembers the segments these rules matched nothing in, under
	// gen, which is new for every compile. Nil remembers nothing.
	clean *cleanTexts
	gen   uint64
}

type compiledRule struct {
	re   *regexp.Regexp
	rule policy.FilterRule
}

// patternCache holds compiled rules, one entry per filter. An edit changes
// the write time, and the next request recompiles in place.
type patternCache struct {
	m sync.Map
	// clean is shared by every filter, so its bound holds however many there
	// are.
	clean cleanTexts
	gens  atomic.Uint64
}

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
	if out.err == nil {
		// A new generation, so nothing remembered under the old rules
		// applies to the new ones.
		out.clean, out.gen = &c.clean, c.gens.Add(1)
	}
	c.m.Store(key, patternEntry{updatedAt: f.UpdatedAt, compiled: out})
	return out
}

// minRemembered is the shortest segment worth remembering. Shorter ones are
// cheap to scan, and would crowd the long ones out.
const minRemembered = 512

// maxCleanTexts is how many segments one generation of cleanTexts holds:
// about 2 MB each, and at least 16 MB of text.
const maxCleanTexts = 1 << 15

// cleanTexts remembers segments a filter's rules matched nothing in. Coding
// agents resend the whole conversation every turn, so without it each turn
// scans again what the turns before already did, and that grows with the
// conversation.
//
// It keeps two generations: when the current one fills, the older is
// dropped. A segment found in the older moves to the current, so what is
// still being resent stays.
type cleanTexts struct {
	mu       sync.Mutex
	cur, old map[[sha256.Size]byte]struct{}
}

// cleanKey names a segment under one generation of rules. SHA-256, so a text
// cannot be made to pass as one that was clean.
func cleanKey(gen uint64, s string) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write(binary.BigEndian.AppendUint64(nil, gen))
	_, _ = io.WriteString(h, s)
	return [sha256.Size]byte(h.Sum(nil))
}

func (t *cleanTexts) has(k [sha256.Size]byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.cur[k]; ok {
		return true
	}
	if _, ok := t.old[k]; ok {
		t.addLocked(k)
		return true
	}
	return false
}

func (t *cleanTexts) add(k [sha256.Size]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addLocked(k)
}

func (t *cleanTexts) addLocked(k [sha256.Size]byte) {
	if t.cur == nil || len(t.cur) >= maxCleanTexts {
		t.old, t.cur = t.cur, make(map[[sha256.Size]byte]struct{})
	}
	t.cur[k] = struct{}{}
}

// compileRules fails closed. The control plane validates rules on write, so a
// failure means the row was changed directly.
func compileRules(rules []policy.FilterRule) compiled {
	if len(rules) == 0 {
		// A guardrail that passes everything is worse than a broken one.
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

// patternHit is one rule that fired, for a check to report.
type patternHit struct {
	// Rule is the expression that matched.
	Rule string `json:"rule"`
	// Matches is how many times it matched across every segment.
	Matches int `json:"matches"`
	// Refused says this rule dropped the request rather than replaced text.
	Refused bool `json:"refused,omitempty"`
}

// runPattern applies one pattern filter's rules to the segments, in order,
// each on what the one before left. A refusal is a filterRefusedError, as a
// model's is, so it takes the same path through outcomes, logs and shadow
// mode.
func runPattern(c compiled, texts []string) ([]string, []patternHit, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	out := slices.Clone(texts)

	// A segment no rule matched is left as it was, so a remembered one needs
	// no scan and adds no hits.
	keys := make(map[int][sha256.Size]byte)
	scan := make([]int, 0, len(out))
	for i, s := range out {
		if c.clean != nil && len(s) >= minRemembered {
			k := cleanKey(c.gen, s)
			if c.clean.has(k) {
				continue
			}
			keys[i] = k
		}
		scan = append(scan, i)
	}
	matched := make([]bool, len(out))

	var hits []patternHit
	for _, cr := range c.rules {
		matches := 0
		for _, i := range scan {
			s := out[i]
			found := len(cr.re.FindAllStringIndex(s, -1))
			if found == 0 {
				continue
			}
			matched[i] = true
			matches += found
			if cr.rule.Refuse {
				// Capped like a model's reason: it goes into an error body and
				// a usage row.
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
	for i, k := range keys {
		if !matched[i] {
			c.clean.add(k)
		}
	}
	return out, hits, nil
}

// patternCheckWarnings says what a pattern filter got wrong: it left what it
// should remove, or touched what it should leave.
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

// quoteRule renders an expression for use in a message.
func quoteRule(s string) string {
	// With the ellipsis, a cut rule is 60 bytes too.
	if len(s) > 60 {
		s = shorten(s, 60-len(ellipsis))
	}
	return "'" + s + "'"
}
