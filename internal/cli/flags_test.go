package cli

import (
	"flag"
	"slices"
	"testing"

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
	if err := parse(fs, []string{"team", "team_1", "--rpm", "60", "--json"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *rpm != 60 {
		t.Errorf("rpm = %d, want 60", *rpm)
	}
	if !*asJSON {
		t.Error("a trailing boolean flag was not seen")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"team", "team_1"}) {
		t.Errorf("positional args = %v, want [team team_1]", got)
	}
}

func TestParseStillAcceptsTheConventionalOrder(t *testing.T) {
	fs, rpm, _, org := newFlagSet()
	if err := parse(fs, []string{"--rpm=60", "--org", "org_1", "team", "team_1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *rpm != 60 || *org != "org_1" {
		t.Errorf("rpm = %d, org = %q", *rpm, *org)
	}
	if got := fs.Args(); !slices.Equal(got, []string{"team", "team_1"}) {
		t.Errorf("positional args = %v, want [team team_1]", got)
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
	if err := parse(fs, []string{"--json", "team_1"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*asJSON {
		t.Error("--json was not set")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"team_1"}) {
		t.Errorf("positional args = %v, want [team_1]", got)
	}
}

func TestParseRejectsAnUnknownFlag(t *testing.T) {
	fs, _, _, _ := newFlagSet()
	fs.SetOutput(discard{})
	if err := parse(fs, []string{"team_1", "--nonsense", "1"}); err == nil {
		t.Error("an unknown flag was accepted; a typo would silently do nothing")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

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
