package cli

import (
	"flag"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func newFlagSet() (*flag.FlagSet, *int, *bool, *string) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	rpm := fs.Int("rpm", -1, "")
	asJSON := fs.Bool("json", false, "")
	org := fs.String("org", "", "")
	return fs, rpm, asJSON, org
}

func TestParseAcceptsFlagsAfterPositionalArguments(t *testing.T) {
	// This is how the command reads naturally, and how it will be typed.
	fs, rpm, asJSON, _ := newFlagSet()
	if err := parse(fs, []string{"project", "project_1", "--rpm", "60", "--json"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *rpm != 60 {
		t.Errorf("rpm = %d, want 60", *rpm)
	}
	if !*asJSON {
		t.Error("a trailing boolean flag was not seen")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"project", "project_1"}) {
		t.Errorf("positional args = %v, want [project project_1]", got)
	}
}

func TestParseStillAcceptsTheConventionalOrder(t *testing.T) {
	fs, rpm, _, org := newFlagSet()
	if err := parse(fs, []string{"--rpm=60", "--org", "org_1", "project", "project_1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *rpm != 60 || *org != "org_1" {
		t.Errorf("rpm = %d, org = %q", *rpm, *org)
	}
	if got := fs.Args(); !slices.Equal(got, []string{"project", "project_1"}) {
		t.Errorf("positional args = %v, want [project project_1]", got)
	}
}

func TestParseTreatsEverythingAfterDoubleDashAsPositional(t *testing.T) {
	fs, _, _, _ := newFlagSet()
	if err := parse(fs, []string{"name", "--", "--rpm"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := fs.Args(); !slices.Equal(got, []string{"name", "--rpm"}) {
		t.Errorf("positional args = %v, want [name --rpm]", got)
	}
}

func TestParseDoesNotSwallowAPositionalAfterABooleanFlag(t *testing.T) {
	fs, _, asJSON, _ := newFlagSet()
	if err := parse(fs, []string{"--json", "project_1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*asJSON {
		t.Error("--json was not set")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"project_1"}) {
		t.Errorf("positional args = %v, want [project_1]", got)
	}
}

func TestParseRejectsAnUnknownFlag(t *testing.T) {
	fs, _, _, _ := newFlagSet()
	err := parse(fs, []string{"project_1", "--nonsense", "1"})
	if err == nil || err.Error() != "unknown flag --nonsense" {
		t.Errorf("err = %v, want it to name the flag the way the help does", err)
	}
	err = parse(fs, []string{"--rpm"})
	if err == nil || err.Error() != "a value is missing after --rpm" {
		t.Errorf("err = %v, want it to say --rpm needs a value", err)
	}
	err = parse(fs, []string{"--rpm", "abc"})
	if err == nil || !strings.Contains(err.Error(), "for --rpm") {
		t.Errorf("err = %v, want it to name --rpm", err)
	}
}

// The panel's amount() rounds the same way, so one cost reads the same in
// both.
func TestFormatAmountRoundsAsThePanelDoes(t *testing.T) {
	tests := []struct {
		micros int64
		want   string
	}{
		{0, "0.00"},
		{12_345_000, "12.35"},
		{-2_500_000, "-2.50"},
		{500_000, "0.50"},
		{1_234, "0.0012"}, // a per-request cost must not print as 0.00
		{12, "0.000012"},  // nor as an exponent
	}
	for _, tc := range tests {
		if got := policy.FormatAmount(tc.micros); got != tc.want {
			t.Errorf("FormatAmount(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
}

func TestFormatMicrosKeepsSmallAmountsVisible(t *testing.T) {
	tests := []struct {
		micros int64
		want   string
	}{
		{0, "0.00"},
		{1_000_000, "1.00"},
		{2_000, "0.002"},  // a small budget must not print as 0.00
		{1_500, "0.0015"}, // nor a per-request cost
		{123_456_789, "123.456789"},
	}
	for _, tc := range tests {
		if got := policy.FormatMicros(tc.micros); got != tc.want {
			t.Errorf("FormatMicros(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
}

// Every sandbox verb shares one FlagSet, so without this 'resume --ttl 8h'
// would parse and the lifetime would be dropped without a word. The same
// check holds the arguments to what help.go says the verb takes.
func TestParseVerbHoldsAVerbToItsFlagsAndArguments(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		args []string
		want string // the verb's own name, or "" for a refusal
	}{
		{"extend", []string{"box", "--ttl", "8h"}, "extend"},
		{"resume", []string{"box", "--ttl", "8h"}, ""},
		{"resume", []string{"box", "--json"}, "resume"},
		{"down", []string{"box"}, "terminate"},
		{"", nil, "list"},
		{"show", nil, ""},
		{"show", []string{"box", "extra"}, ""},
		{"ssh", []string{"box", "--", "git", "status"}, "ssh"},
		{"lst", nil, ""},
	} {
		fs := flag.NewFlagSet("sandbox "+tc.sub, flag.ContinueOnError)
		fs.Duration("ttl", 0, "")
		fs.Bool("json", false, "")
		got, err := parseVerb(fs, "sandbox", tc.sub, tc.args)
		if tc.want == "" && err == nil {
			t.Errorf("sandbox %s %v was accepted, want a refusal", tc.sub, tc.args)
		}
		if tc.want != "" && (err != nil || got != tc.want) {
			t.Errorf("sandbox %s %v = %q, %v; want %q", tc.sub, tc.args, got, err, tc.want)
		}
	}
}

func TestArgCount(t *testing.T) {
	for spec, want := range map[string][2]int{
		"":                             {0, 0},
		"[flags]":                      {0, 0},
		"<alias>":                      {1, 1},
		"[<alias>]":                    {0, 1},
		"<scope> [<id>] [flags]":       {1, 2},
		"<name> [-- <command>]":        {1, -1},
		"<project> <new-name>":         {2, 2},
		"[<client>] [flags]":           {0, 1},
		"<email-or-id> <member|admin>": {2, 2},
	} {
		if lo, hi := argCount(spec); lo != want[0] || hi != want[1] {
			t.Errorf("argCount(%q) = %d, %d; want %d, %d", spec, lo, hi, want[0], want[1])
		}
	}
}

// Report windows and lifetimes are thought of in days, so --since 7d works,
// and a default prints as 7d rather than 168h0m0s.
func TestDurationFlagTakesDays(t *testing.T) {
	const day = 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"7d": 7 * day, "1d12h": day + 12*time.Hour, "90m": 90 * time.Minute, "0d": 0,
	} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "7", "7x", "-1d", "1d-2h", "7days"} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) was accepted", in)
		}
	}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var since, ttl time.Duration
	durationVar(fs, &since, "since", 7*day, "")
	durationVar(fs, &ttl, "ttl", -1, "")
	if def := fs.Lookup("since").DefValue; def != "7d" {
		t.Errorf("--since default prints as %q, want 7d", def)
	}
	if def := fs.Lookup("ttl").DefValue; shownDefault(def) {
		t.Errorf("--ttl default prints as %q, which help would show", def)
	}
	// 'sandbox agent' passes its flags on as text, so a value must read back.
	if err := fs.Parse([]string{"--ttl", "1d12h"}); err != nil || ttl != day+12*time.Hour {
		t.Fatalf("--ttl 1d12h = %v, %v", ttl, err)
	}
	if again, err := parseDuration(fs.Lookup("ttl").Value.String()); err != nil || again != ttl {
		t.Errorf("--ttl prints as %q, which reads back as %v, %v",
			fs.Lookup("ttl").Value.String(), again, err)
	}
}
