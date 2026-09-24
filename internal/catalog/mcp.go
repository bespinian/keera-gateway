package catalog

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// MCPServer is one declared MCP server.
type MCPServer struct {
	Alias string `yaml:"alias"`
	// URL is the server's Streamable HTTP endpoint.
	URL         string `yaml:"url"`
	Description string `yaml:"description"`
	// AuthHeader is the header the credential goes in. Empty is
	// Authorization, as a bearer token.
	AuthHeader string `yaml:"auth_header"`
	APIKeyEnv  string `yaml:"api_key_env"`
	Disabled   bool   `yaml:"disabled"`
}

// ParseMCPServer validates one declared MCP server. The control API uses it
// too, so a server added by hand means what the same entry in a file means.
func ParseMCPServer(m MCPServer) (policy.MCPServer, error) {
	switch {
	case m.Alias == "":
		return policy.MCPServer{}, fmt.Errorf("alias is required")
	case !policy.ValidMCPAlias(m.Alias):
		return policy.MCPServer{}, fmt.Errorf("alias %q must be lowercase letters, digits and "+
			"interior hyphens: it is part of the address clients are given", m.Alias)
	}
	u, err := url.Parse(strings.TrimSpace(m.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return policy.MCPServer{}, fmt.Errorf("url must be the server's http or https " +
			"endpoint, such as https://mcp.example.com/mcp")
	}
	header := strings.TrimSpace(m.AuthHeader)
	if header != "" {
		header = http.CanonicalHeaderKey(header)
		if !validHeaderName(header) {
			return policy.MCPServer{}, fmt.Errorf("auth_header %q is not a header name", m.AuthHeader)
		}
	}
	return policy.MCPServer{
		Alias: m.Alias, URL: u.String(), Description: strings.TrimSpace(m.Description),
		AuthHeader: header, APIKeyEnv: strings.TrimSpace(m.APIKeyEnv), Enabled: !m.Disabled,
	}, nil
}

// validHeaderName reports whether s is a header name: letters, digits and
// hyphens, which is every header anybody sends a credential in.
func validHeaderName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return s != ""
}
