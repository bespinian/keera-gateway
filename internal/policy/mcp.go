package policy

import (
	"slices"
	"strings"
)

// MCPServer is an MCP server the gateway stands in front of. Clients name its
// alias, and the gateway holds its address and credential.
type MCPServer struct {
	OrgID string `json:"org_id"`
	Alias string `json:"alias"`
	// URL is the server's Streamable HTTP endpoint.
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	// AuthHeader carries the credential. Empty is Authorization, as a bearer
	// token; any other header gets the credential as it is.
	AuthHeader string `json:"auth_header,omitempty"`
	// The credential fields work as a Model's do.
	APIKeyCiphertext []byte `json:"-"`
	APIKey           string `json:"-"`
	HasAPIKey        bool   `json:"has_api_key,omitempty"`
	// Limited, Locked and OrgName work as a Model's do.
	Limited bool   `json:"-"`
	Locked  bool   `json:"-"`
	OrgName string `json:"-"`
	Enabled bool   `json:"enabled"`
}

// maxToolNameLen is the longest tool name MCP allows.
const maxToolNameLen = 128

// ValidToolEntry reports whether s is a server alias or 'alias/tool'.
func ValidToolEntry(s string) bool {
	server, tool, one := strings.Cut(s, "/")
	if !ValidAlias(server) {
		return false
	}
	return !one || (tool != "" && len(tool) <= maxToolNameLen && !strings.ContainsAny(tool, " \t\n"))
}

// AllowsTool reports whether a key may call a tool. A nil list allows all.
func (r *Resolved) AllowsTool(server, tool string) bool {
	return r.AllowedTools == nil ||
		slices.Contains(r.AllowedTools, server) ||
		slices.Contains(r.AllowedTools, server+"/"+tool)
}

// AllowsWholeServer reports whether a key may call every tool of a server.
func (r *Resolved) AllowsWholeServer(server string) bool {
	return r.AllowedTools == nil || slices.Contains(r.AllowedTools, server)
}

// AllowsServer reports whether a key may call any tool of a server.
func (r *Resolved) AllowsServer(server string) bool {
	if r.AllowedTools == nil {
		return true
	}
	for _, e := range r.AllowedTools {
		if s, _, _ := strings.Cut(e, "/"); s == server {
			return true
		}
	}
	return false
}

// intersectTools narrows a tool allow-list. A server and one of its tools meet
// in the tool.
func intersectTools(cur, next []string) []string {
	return narrowAllowList(cur, next, func(a, b string) bool {
		return a == b || (!strings.Contains(a, "/") && strings.HasPrefix(b, a+"/"))
	})
}
