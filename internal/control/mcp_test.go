package control

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestAnMCPServerIsNormalized(t *testing.T) {
	m := policy.MCPServer{Alias: "jira", URL: " https://jira.example.ch/mcp ", AuthHeader: "x-api-key"}
	if msg := normalizeMCPServer(&m); msg != "" {
		t.Fatal(msg)
	}
	if m.URL != "https://jira.example.ch/mcp" || m.AuthHeader != "X-Api-Key" {
		t.Errorf("jira = %+v, want its address trimmed and its header name made canonical", m)
	}
}

func TestAnMCPServerNeedsAnAddress(t *testing.T) {
	for _, tc := range []struct {
		in   policy.MCPServer
		want string
	}{
		{policy.MCPServer{Alias: "gh"}, "url"},
		{policy.MCPServer{Alias: "GH", URL: "https://x/mcp"}, "alias"},
		{policy.MCPServer{Alias: "gh", URL: "ftp://x/mcp"}, "url"},
		{policy.MCPServer{Alias: "gh", URL: "https://x", AuthHeader: "a b"}, "auth_header"},
	} {
		if msg := normalizeMCPServer(&tc.in); !strings.Contains(msg, tc.want) {
			t.Errorf("%+v: %q, want it to mention %s", tc.in, msg, tc.want)
		}
	}
}
