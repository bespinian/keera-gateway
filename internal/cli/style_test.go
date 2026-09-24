package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The promise this whole file checks: what a pipe, a file or a CI log gets is
// what it got before there was any colour at all.

func TestColourOnlyWhereItCanBeRead(t *testing.T) {
	// A pipe, which is what a redirect and a CI job both are.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("TERM", "xterm-256color")

	if wantsColor("auto", w) {
		t.Error("auto painted a pipe")
	}
	if !wantsColor("always", w) {
		t.Error("--color always did not paint a pipe")
	}
	if wantsColor("never", w) {
		t.Error("--color never painted something")
	}

	t.Setenv("CLICOLOR_FORCE", "1")
	if !wantsColor("auto", w) {
		t.Error("CLICOLOR_FORCE did not turn colour on")
	}
	t.Setenv("NO_COLOR", "1")
	if wantsColor("auto", w) {
		t.Error("NO_COLOR did not turn colour off")
	}
	// An explicit flag beats the environment, in both directions.
	if !wantsColor("always", w) {
		t.Error("NO_COLOR overrode --color always")
	}

	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("TERM", "dumb")
	if wantsColor("auto", w) {
		t.Error("TERM=dumb was painted")
	}
}

func TestTakeColorMode(t *testing.T) {
	t.Cleanup(func() { colorMode = "auto" })
	cases := []struct {
		args []string
		rest []string
		mode string
	}{
		{[]string{"org", "list"}, []string{"org", "list"}, "auto"},
		{[]string{"--color", "never", "org"}, []string{"org"}, "never"},
		{[]string{"org", "--color=always"}, []string{"org"}, "always"},
		{[]string{"--no-color", "org"}, []string{"org"}, "never"},
		// Everything after -- belongs to the command being run in a sandbox.
		{[]string{"sandbox", "ssh", "box", "--", "ls", "--color=always"},
			[]string{"sandbox", "ssh", "box", "--", "ls", "--color=always"}, "auto"},
	}
	for _, c := range cases {
		colorMode = "auto"
		rest, err := takeColorMode(c.args)
		if err != nil {
			t.Fatalf("takeColorMode(%v): %v", c.args, err)
		}
		if strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("takeColorMode(%v) left %v, want %v", c.args, rest, c.rest)
		}
		if colorMode != c.mode {
			t.Errorf("takeColorMode(%v) set %q, want %q", c.args, colorMode, c.mode)
		}
	}
	if _, err := takeColorMode([]string{"--color", "pink"}); err == nil {
		t.Error("--color pink should be an error")
	}
}

func TestStripANSIKeepsEverythingElse(t *testing.T) {
	cases := map[string]string{
		"":                          "",
		"plain":                     "plain",
		"\x1b[1mbold\x1b[0m":        "bold",
		"a\x1b[31mb\x1b[0mc":        "abc",
		"keys\tkept\tand\nnewlines": "keys\tkept\tand\nnewlines",
		// Not an escape sequence: left alone rather than eaten.
		"90% of \x1b nothing": "90% of \x1b nothing",
	}
	for in, want := range cases {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPaintedTableHasThePlainTableSColumns is the one that matters: colour is
// only allowed if it changes nothing but the colour.
func TestPaintedTableHasThePlainTableSColumns(t *testing.T) {
	t.Cleanup(func() { style = false })
	rows := func(w *table) {
		w.header("ID\tNAME\tSTATE\tLAST USED")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			"key_1", "laptop", statusWord("active"), "2026-09-01")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			"key_longer", "a much longer alias", statusWord("revoked"), statusWord("never"))
		_, _ = fmt.Fprintln(w, "\nA line of prose under the table, which is not a row.")
	}

	style = true
	var painted bytes.Buffer
	pw := newTable(&painted)
	rows(pw)
	if err := pw.Flush(); err != nil {
		t.Fatal(err)
	}

	style = false
	var plain bytes.Buffer
	lw := newTable(&plain)
	rows(lw)
	if err := lw.Flush(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(painted.String(), "\x1b[") {
		t.Fatal("the painted table has no colour in it")
	}
	if got := stripANSI(painted.String()); got != plain.String() {
		t.Errorf("colour moved the columns:\npainted, stripped:\n%s\nplain:\n%s", got, plain.String())
	}
}

// A cell whose own text starts or ends with spaces is where putting the
// escapes back is easiest to get wrong.
func TestPaintedCellsWithSpacesAroundThem(t *testing.T) {
	t.Cleanup(func() { style = false })
	rows := func(w *table) {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", style.head("  ok  "), " spaced ", "x")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", "wider cell", " also spaced ", "y")
	}
	style = true
	var painted bytes.Buffer
	pw := newTable(&painted)
	rows(pw)
	_ = pw.Flush()

	style = false
	var plain bytes.Buffer
	lw := newTable(&plain)
	rows(lw)
	_ = lw.Flush()

	if got := stripANSI(painted.String()); got != plain.String() {
		t.Errorf("painted %q, plain %q", got, plain.String())
	}
}
