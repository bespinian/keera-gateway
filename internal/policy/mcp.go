package policy

import (
	"slices"
	"strings"
)

// MCPServer is one MCP server the gateway stands in front of. It belongs to one
// organisation: clients name its alias, and the gateway holds its address and
// its credential.
type MCPServer struct {
	// OrgID is the organisation the server belongs to. Only its keys reach it.
	OrgID string `json:"org_id"`
	Alias string `json:"alias"`
	// URL is the server's Streamable HTTP endpoint.
	URL string `json:"url"`
	// Description says what the server is for.
	Description string `json:"description,omitempty"`
	// AuthHeader is the header the credential is sent in. Empty is
	// Authorization, as a bearer token; any other header gets the credential
	// as it is.
	AuthHeader string `json:"auth_header,omitempty"`
	// APIKeyCiphertext is a credential set through the control plane, sealed
	// with a key only the gateway's environment holds.
	APIKeyCiphertext []byte `json:"-"`
	// APIKey is that credential decrypted when the registry loads it. It lives
	// only in memory.
	APIKey string `json:"-"`
	// HasAPIKey tells the control panel a credential is stored, and nothing
	// more about it.
	HasAPIKey bool `json:"has_api_key,omitempty"`
	// Limited and Locked are a model's, for an MCP server: the organisation
	// has not paid yet, and the server is inside the deployment's network.
	Limited bool `json:"-"`
	Locked  bool `json:"-"`
	Enabled bool `json:"enabled"`
}

// maxToolNameLen is the longest tool name MCP allows.
const maxToolNameLen = 128

// ValidToolEntry reports whether s can stand in a tool allow-list: a server's
// alias, which allows all of its tools, or 'alias/tool' for one of them.
func ValidToolEntry(s string) bool {
	server, tool, one := strings.Cut(s, "/")
	if !ValidAlias(server) {
		return false
	}
	return !one || (tool != "" && len(tool) <= maxToolNameLen && !strings.ContainsAny(tool, " \t\n"))
}

// AllowsTool reports whether a key may call one tool of one server. A nil list
// allows every tool of every server of the organisation.
func (r *Resolved) AllowsTool(server, tool string) bool {
	return r.AllowedTools == nil ||
		slices.Contains(r.AllowedTools, server) ||
		slices.Contains(r.AllowedTools, server+"/"+tool)
}

// AllowsWholeServer reports whether a key may call every tool of a server,
// rather than only some of them.
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

// intersectTools narrows a tool allow-list. An entry is a whole server or one
// of its tools, so the intersection keeps the narrower of two entries that
// overlap: a server and one of its tools meet in the tool.
func intersectTools(cur, next []string) []string {
	switch {
	case next == nil:
		return cur
	case cur == nil:
		return slices.Clone(next)
	}
	out := make([]string, 0, min(len(cur), len(next)))
	add := func(e string) {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	for _, a := range cur {
		for _, b := range next {
			switch {
			case a == b:
				add(a)
			case !strings.Contains(a, "/") && strings.HasPrefix(b, a+"/"):
				add(b)
			case !strings.Contains(b, "/") && strings.HasPrefix(a, b+"/"):
				add(a)
			}
		}
	}
	return out
}
