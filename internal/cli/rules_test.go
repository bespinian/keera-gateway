package cli

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestParseRules(t *testing.T) {
	rules, err := parseRules(`
# what must never leave, whatever it is asked for
(?i)\bsk-[a-z0-9]{16,}\b   => [CREDENTIAL]
\b[A-Z]{2}\d{2}\w{10,}\b   => [IBAN]

(?i)\bexport all customers\b => REFUSE: that moves the customer list out
`)
	if err != nil {
		t.Fatalf("parseRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3 - blank lines and comments are not rules", len(rules))
	}
	if rules[0].Pattern != `(?i)\bsk-[a-z0-9]{16,}\b` || rules[0].Replace != "[CREDENTIAL]" {
		t.Errorf("rule 0 = %+v", rules[0])
	}
	if !rules[2].Refuse || rules[2].Reason != "that moves the customer list out" {
		t.Errorf("rule 2 = %+v, want a refusal carrying its sentence", rules[2])
	}
}

func TestParseRulesSplitsAtTheLastArrow(t *testing.T) {
	// The left side is an expression and the right side is a literal, so '=>'
	// is plausible in the first and not in the second.
	rules, err := parseRules(`a=>b\w+ => [X]`)
	if err != nil {
		t.Fatalf("parseRules: %v", err)
	}
	if rules[0].Pattern != `a=>b\w+` {
		t.Errorf("pattern = %q, want the whole expression", rules[0].Pattern)
	}
	if rules[0].Replace != "[X]" {
		t.Errorf("replace = %q, want [X]", rules[0].Replace)
	}
}

func TestParseRulesDoesNotReadAReplacementAsARefusal(t *testing.T) {
	// The word has to end where the word ends, or a redaction token that
	// begins with it would drop somebody's request.
	rules, err := parseRules(`\bx\b => [REFUSED-CREDENTIAL]`)
	if err != nil {
		t.Fatalf("parseRules: %v", err)
	}
	if rules[0].Refuse {
		t.Errorf("rule = %+v, want a replacement rather than a refusal", rules[0])
	}
}

func TestParseRulesRejectsWhatItCannotRead(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"a line with no arrow", `\bsk-\w+\b [CREDENTIAL]`, "has no"},
		{"an expression that will not compile", `([ => [X]`, "line 1"},
		{"an expression matching everything", `x* => [X]`, "matches the empty string"},
		{"nothing at all", "\n# only a comment\n", "no rules were read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRules(tc.text)
			if err == nil {
				t.Fatalf("want an error about %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRulesRoundTripThroughTheirTextForm(t *testing.T) {
	// What somebody reads back has to be what they would write. A file kept in
	// a repository and pasted into the panel is the same text either way.
	want := []policy.FilterRule{
		{Pattern: `\bsk-\w+\b`, Replace: "[CREDENTIAL]"},
		{Pattern: `\bexport\b`, Refuse: true, Reason: "no exports"},
		{Pattern: `\bdrop\b`, Refuse: true},
	}
	got, err := parseRules(strings.Join(formatRules(want), "\n"))
	if err != nil {
		t.Fatalf("parseRules of formatRules: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rules, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDestinations(t *testing.T) {
	aliases, ceilings, err := parseDestinations("keera-speed:4k, keera-mid:32000,keera-frontier")
	if err != nil {
		t.Fatalf("parseDestinations: %v", err)
	}
	want := []string{"keera-speed", "keera-mid", "keera-frontier"}
	if len(aliases) != 3 {
		t.Fatalf("aliases = %v, want %v", aliases, want)
	}
	for i := range want {
		if aliases[i] != want[i] {
			t.Errorf("alias %d = %q, want %q", i, aliases[i], want[i])
		}
	}
	if ceilings["keera-speed"] != 4000 {
		t.Errorf("'4k' read as %d, want 4000", ceilings["keera-speed"])
	}
	if ceilings["keera-mid"] != 32000 {
		t.Errorf("ceiling = %d, want 32000", ceilings["keera-mid"])
	}
	// The one with no number takes what is above every ceiling, so it must not
	// appear here at all.
	if _, bounded := ceilings["keera-frontier"]; bounded {
		t.Error("a destination written without a ceiling must not get one")
	}
}

func TestParseDestinationsRejectsACeilingThatIsNotOne(t *testing.T) {
	for _, spec := range []string{"a:lots,b", "a:0,b", "a:-5,b"} {
		if _, _, err := parseDestinations(spec); err == nil {
			t.Errorf("parseDestinations(%q) = nil, want an error", spec)
		}
	}
}

func TestFormatDestinationsPrintsWhatParseDestinationsReads(t *testing.T) {
	rt := policy.Router{
		Destinations: []string{"keera-speed", "keera-frontier"},
		Ceilings:     map[string]int{"keera-speed": 4000},
	}
	got := formatDestinations(rt)
	if got != "keera-speed:4k, keera-frontier" {
		t.Errorf("formatDestinations = %q", got)
	}
	aliases, ceilings, err := parseDestinations(got)
	if err != nil {
		t.Fatalf("the printed form did not parse: %v", err)
	}
	if len(aliases) != 2 || ceilings["keera-speed"] != 4000 {
		t.Errorf("round trip gave %v / %v", aliases, ceilings)
	}
}
