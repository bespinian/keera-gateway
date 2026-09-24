package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// The registry in help.go says which flags each subcommand reads, and each
// command's FlagSet says which flags exist. These tests keep the two in step.

// capture runs one invocation and returns what it printed. Help goes to
// stdout, which is what a person redirects when they want to read it.
func capture(t *testing.T, args ...string) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	// Every command below is a help request, which returns before it reaches
	// the control API - so the error is only interesting when it is not nil.
	runErr := Run(context.Background(), args)
	_ = w.Close()
	os.Stdout = old
	out := <-done
	if runErr != nil {
		t.Fatalf("keera %s: %v", strings.Join(args, " "), runErr)
	}
	return out
}

// TestEveryFlagIsDocumented is the drift guard. helpText prints flags a
// command declares and the registry does not claim under "Other flags", so
// the check is that no command has any.
func TestEveryFlagIsDocumented(t *testing.T) {
	for _, c := range commands {
		if len(c.subs) == 0 {
			continue // its flags are on the command itself, checked below
		}
		t.Run(c.name, func(t *testing.T) {
			out := capture(t, c.name, "--help")
			if strings.Contains(out, "Other flags:") {
				t.Errorf("keera %s declares flags no subcommand in help.go claims:\n%s",
					c.name, out[strings.Index(out, "Other flags:"):])
			}
		})
	}
}

// TestSubcommandHelpNamesItsFlags is the other direction: a flag the registry
// claims and the command does not declare is silently dropped by writeFlags,
// so this asserts each one actually appears.
func TestSubcommandHelpNamesItsFlags(t *testing.T) {
	for _, c := range commands {
		for _, sub := range c.subs {
			t.Run(c.name+"/"+sub.name, func(t *testing.T) {
				out := capture(t, c.name, sub.name, "--help")
				for _, name := range sub.flags {
					if !strings.Contains(out, "--"+name) {
						t.Errorf("keera help %s %s does not describe --%s, which help.go "+
							"says it takes; the command does not declare it",
							c.name, sub.name, name)
					}
				}
			})
		}
	}
}

// TestCommandHelpNamesItsFlags is the same for the commands that have no
// subcommands and take their flags directly.
func TestCommandHelpNamesItsFlags(t *testing.T) {
	for _, c := range commands {
		if len(c.subs) > 0 || len(c.flags) == 0 {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			out := capture(t, c.name, "--help")
			for _, name := range c.flags {
				if !strings.Contains(out, "--"+name) {
					t.Errorf("keera help %s does not describe --%s", c.name, name)
				}
			}
		})
	}
}

// TestOverviewNamesEveryCommand guards the thing somebody sees first: a
// command that dispatches and is not in the registry is a command nobody can
// find.
func TestOverviewNamesEveryCommand(t *testing.T) {
	out := overview()
	for _, c := range commands {
		if c.hidden {
			continue
		}
		if !strings.Contains(out, c.name) {
			t.Errorf("the overview does not name %q", c.name)
		}
	}
	// It is the screen for somebody who has just installed this, so its
	// length is part of its job.
	if lines := strings.Count(out, "\n"); lines > 40 {
		t.Errorf("the overview is %d lines; it is meant to be read without scrolling", lines)
	}
}

// TestEveryDispatchedCommandIsRegistered walks the names Run accepts and
// checks each resolves. It is spelled as a list rather than read out of the
// switch because there is no way to read a switch.
func TestEveryDispatchedCommandIsRegistered(t *testing.T) {
	dispatched := []string{
		"org", "orgs", "team", "teams", "user", "users", "key", "keys",
		"model", "models", "filter", "filters", "router", "routers",
		"sandbox", "sandboxes", "sbx", "guardrail", "guardrails",
		"limit", "budget", "usage", "failure", "failures", "sessions",
		"session", "connect", "doctor", "login", "logout", "whoami",
		"version", "help",
	}
	for _, name := range dispatched {
		if _, ok := find(name); !ok {
			t.Errorf("Run dispatches %q and help.go does not know it", name)
		}
	}
}

func TestWantsHelp(t *testing.T) {
	cases := []struct {
		args []string
		sub  string
		want bool
	}{
		{[]string{"--help"}, "", true},
		{[]string{"-h"}, "", true},
		{[]string{"help"}, "", true},
		{[]string{"add", "--help"}, "add", true},
		{[]string{"help", "add"}, "add", true},
		{[]string{"--help", "add"}, "add", true},
		{[]string{"list"}, "", false},
		{[]string{"add", "my-filter", "--mode", "gate"}, "", false},
	}
	for _, c := range cases {
		sub, ok := wantsHelp(c.args)
		if ok != c.want || sub != c.sub {
			t.Errorf("wantsHelp(%v) = %q, %v; want %q, %v", c.args, sub, ok, c.sub, c.want)
		}
	}
}

func TestSuggest(t *testing.T) {
	cases := map[string]string{
		"fliter":   "filter",
		"guardrai": "guardrail",
		"keys":     "key",
		"sesions":  "sessions",
		// Nothing near enough: a guess here would read as the tool having
		// misunderstood rather than as help.
		"deploy": "",
	}
	for typed, want := range cases {
		if got := suggest(typed); got != want {
			t.Errorf("suggest(%q) = %q, want %q", typed, got, want)
		}
	}
}

func TestSuggestSub(t *testing.T) {
	if got := suggestSub("org", "lst"); got != "list" {
		t.Errorf(`suggestSub("org", "lst") = %q, want "list"`, got)
	}
	if got := suggestSub("filter", "delet"); got != "delete" {
		t.Errorf(`suggestSub("filter", "delet") = %q, want "delete"`, got)
	}
}

// TestTakeURL covers the global flag, including the spelling somebody
// actually types: a bare host with no scheme.
func TestTakeURL(t *testing.T) {
	t.Cleanup(func() { urlFlag = "" })
	cases := []struct {
		args []string
		rest []string
		url  string
	}{
		{[]string{"whoami"}, []string{"whoami"}, ""},
		{[]string{"--url", "https://a.ch", "whoami"}, []string{"whoami"}, "https://a.ch"},
		{[]string{"whoami", "--url=https://b.ch"}, []string{"whoami"}, "https://b.ch"},
		{[]string{"--url", "keera.example.ch", "whoami"}, []string{"whoami"},
			"https://keera.example.ch"},
		// Everything after -- belongs to the command being run in a sandbox.
		{[]string{"sandbox", "ssh", "box", "--", "curl", "--url", "x"},
			[]string{"sandbox", "ssh", "box", "--", "curl", "--url", "x"}, ""},
	}
	for _, c := range cases {
		urlFlag = ""
		rest, err := takeURL(c.args)
		if err != nil {
			t.Fatalf("takeURL(%v): %v", c.args, err)
		}
		if strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("takeURL(%v) left %v, want %v", c.args, rest, c.rest)
		}
		if urlFlag != c.url {
			t.Errorf("takeURL(%v) set url %q, want %q", c.args, urlFlag, c.url)
		}
	}
	if _, err := takeURL([]string{"--url"}); err == nil {
		t.Error("--url with no address should be an error")
	}
}
