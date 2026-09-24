package catalog

import (
	"strings"
	"testing"
)

func TestACatalogueDeclaresMCPServers(t *testing.T) {
	c, err := ParseFile([]byte(`
mcp_servers:
  - alias: github
    url: https://api.githubcopilot.com/mcp/
    description: Issues and pull requests.
    api_key_env: GITHUB_TOKEN
  - alias: jira
    url: https://jira.example.ch/mcp
    auth_header: x-api-key
    disabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 0 || len(c.MCPServers) != 2 {
		t.Fatalf("parsed %d models and %d servers", len(c.Models), len(c.MCPServers))
	}
	gh, jira := c.MCPServers[0], c.MCPServers[1]
	if !gh.Managed || !gh.Enabled || gh.APIKeyEnv != "GITHUB_TOKEN" {
		t.Errorf("github = %+v", gh)
	}
	if jira.Enabled || jira.AuthHeader != "X-Api-Key" {
		t.Errorf("jira = %+v, want disabled with its header name made canonical", jira)
	}
}

func TestAnMCPServerNeedsAnAddress(t *testing.T) {
	for in, want := range map[string]string{
		"mcp_servers:\n  - alias: gh\n":                                               "url",
		"mcp_servers:\n  - alias: GH\n    url: https://x/mcp\n":                       "alias",
		"mcp_servers:\n  - alias: gh\n    url: ftp://x/mcp\n":                         "url",
		"mcp_servers:\n  - alias: gh\n    url: https://x\n    auth_header: \"a b\"\n": "auth_header",
	} {
		_, err := ParseFile([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error = %v, want it to mention %s", in, err, want)
		}
	}
}
