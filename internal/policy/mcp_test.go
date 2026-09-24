package policy

import (
	"slices"
	"testing"
)

func TestToolAllowListsNarrowToTheNarrowerEntry(t *testing.T) {
	org := &Limits{AllowedTools: []string{"github", "jira/search"}}
	team := &Limits{AllowedTools: []string{"github/search_code", "jira", "slack"}}
	r := Resolve(Key{ID: "k", OrgID: "o", TeamID: "t"}, org, team, nil)

	// The organisation allows all of github and one jira tool; the team allows
	// one github tool and all of jira. What both allow is one tool of each, and
	// slack, which the organisation never allowed, is not in it.
	want := []string{"github/search_code", "jira/search"}
	if !slices.Equal(r.AllowedTools, want) {
		t.Fatalf("allowed = %v, want %v", r.AllowedTools, want)
	}
	for _, tc := range []struct {
		server, tool string
		want         bool
	}{
		{"github", "search_code", true},
		{"github", "create_issue", false},
		{"jira", "search", true},
		{"slack", "post", false},
	} {
		if got := r.AllowsTool(tc.server, tc.tool); got != tc.want {
			t.Errorf("AllowsTool(%s, %s) = %v, want %v", tc.server, tc.tool, got, tc.want)
		}
	}
	if !r.AllowsServer("github") || r.AllowsServer("slack") {
		t.Error("AllowsServer should follow the entries")
	}
}

func TestNoToolAllowListAllowsEverything(t *testing.T) {
	r := Resolve(Key{ID: "k", OrgID: "o"}, nil, nil, nil)
	if !r.AllowsTool("any", "tool") || !r.AllowsServer("any") {
		t.Error("a scope that says nothing about tools should allow them all")
	}
}

func TestBlockingHostedToolsCannotBeUndoneBelow(t *testing.T) {
	yes, no := true, false
	r := Resolve(Key{ID: "k", OrgID: "o", TeamID: "t"},
		&Limits{BlockHostedTools: &yes}, &Limits{BlockHostedTools: &no}, nil)
	if !r.BlockHostedTools {
		t.Error("a team switched hosted tools back on under its organisation")
	}
}

func TestToolEntries(t *testing.T) {
	for entry, want := range map[string]bool{
		"github":              true,
		"github/search_code":  true,
		"github/repos.list-1": true,
		"GitHub":              false,
		"github/":             false,
		"/search":             false,
		"github/has space":    false,
	} {
		if got := ValidToolEntry(entry); got != want {
			t.Errorf("ValidToolEntry(%q) = %v, want %v", entry, got, want)
		}
	}
}
