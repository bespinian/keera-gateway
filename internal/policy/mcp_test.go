package policy

import (
	"slices"
	"testing"
)

func TestToolAllowListsNarrowToTheNarrowerEntry(t *testing.T) {
	org := &Limits{AllowedTools: []string{"github", "jira/search"}}
	project := &Limits{AllowedTools: []string{"github/search_code", "jira", "slack"}}
	r := Resolve(Key{ID: "k", OrgID: "o", ProjectID: "t"}, org, project, nil)

	// What both allow is one tool of each; slack was never the org's to grant.
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
