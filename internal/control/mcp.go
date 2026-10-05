package control

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The MCP servers the gateway stands in front of, and the log of their tool
// calls. A server belongs to one organisation, and its administrators change
// it. See docs/mcp.md.

// listMCPServers is readable by anyone in the organisation: a developer needs
// to know which servers exist to connect to them. Their addresses are for the
// organisation's administrators.
func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	servers, err := s.st.ListMCPServers(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	admin := p.CanAdminOrg(orgID)
	for i := range servers {
		if !admin {
			servers[i].URL = ""
			servers[i].AuthHeader = ""
			servers[i].HasAPIKey = false
		}
		servers[i].APIKeyCiphertext = nil
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": servers})
}

func (s *Server) putMCPServer(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	var body struct {
		URL         string `json:"url"`
		Description string `json:"description"`
		AuthHeader  string `json:"auth_header"`
		// Enabled is a pointer, so leaving it out keeps a server on.
		Enabled *bool `json:"enabled"`
		// APIKey is write-only. Nil leaves the stored one alone, and "" clears it.
		APIKey *string `json:"api_key"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	m := policy.MCPServer{
		OrgID: orgID, Alias: r.PathValue("alias"), URL: body.URL, Description: body.Description,
		AuthHeader: body.AuthHeader, Enabled: body.Enabled == nil || *body.Enabled,
	}
	if msg := normalizeMCPServer(&m); msg != "" {
		badRequest(w, msg)
		return
	}
	credential := ""
	if body.APIKey != nil {
		credential = strings.TrimSpace(*body.APIKey)
	}
	if err := s.st.UpsertMCPServer(r.Context(), m); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "mcp_server.put", "mcp_server", m.Alias, m)
	if body.APIKey != nil {
		if err := s.setMCPCredential(r, p, orgID, m.Alias, credential); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.changed(r)
	// Read back, so has_api_key says what is stored and not what this body
	// happened to mention.
	stored, err := s.st.MCPServer(r.Context(), orgID, m.Alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, stored)
}

// normalizeMCPServer tidies a server's fields and says what is wrong with it,
// or "" when nothing is.
func normalizeMCPServer(m *policy.MCPServer) string {
	if !policy.ValidAlias(m.Alias) {
		return badAlias(m.Alias)
	}
	u, err := url.Parse(strings.TrimSpace(m.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "'url' must be the server's http or https endpoint, such as " +
			"https://mcp.example.com/mcp"
	}
	m.URL = u.String()
	m.Description = strings.TrimSpace(m.Description)
	if m.AuthHeader = strings.TrimSpace(m.AuthHeader); m.AuthHeader != "" {
		m.AuthHeader = http.CanonicalHeaderKey(m.AuthHeader)
		if !validHeaderName(m.AuthHeader) {
			return "'auth_header' " + strconv.Quote(m.AuthHeader) + " is not a header name"
		}
	}
	return ""
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

// setMCPCredential seals a pasted credential, or clears the stored one.
func (s *Server) setMCPCredential(r *http.Request, p *authn.Principal, orgID, alias, credential string) error {
	return s.setCredential(r, p, orgID, alias, credential, "mcp_server",
		registry.MCPSecretName, s.st.SetMCPCredential)
}

// deleteMCPServer keeps a server that is still in use: a guardrail naming a
// missing server cannot be saved again.
func (s *Server) deleteMCPServer(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	s.deleteUnused(w, r, p, "mcp_server", s.st.MCPServerUsers, s.st.DeleteMCPServer, func(alias, users string) string {
		return "the MCP server '" + alias + "' is in the tool allow-list of " + users +
			", so take it off them first"
	})
}

// toolCalls is the tool-call log of one organisation, newest first, or with
// summary=1 the calls of each tool added up. It needs the organisation: two
// organisations can each have a server of the same name, and their calls
// would add up as one.
func (s *Server) toolCalls(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !s.requireAdmin(w, p) {
		return
	}
	orgID, from, to, ok := s.reportOrg(w, r, p)
	if !ok {
		return
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return
	}
	q := r.URL.Query()
	tq := store.ToolCallQuery{
		OrgID: orgID, ProjectID: sc.ProjectID, KeyID: sc.KeyID, UserID: sc.UserID,
		Server: q.Get("server"), Tool: q.Get("tool"), From: from, To: to,
	}
	tq.Limit, tq.Before = page(q)
	if httpx.Flag(q, "summary") {
		data, err := s.st.SummarizeToolCalls(r.Context(), tq)
		if err != nil {
			s.fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "data": data})
		return
	}
	data, err := s.st.ListToolCalls(r.Context(), tq)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"from": from, "to": to, "data": data}
	if len(data) > 0 {
		out["next_before"] = data[len(data)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// checkAllowedTools refuses a tool allow-list with an entry that is not one,
// or that names a server the organisation does not have. Such an entry allows
// nothing, and a typo is better caught now than as a tool that never works.
func (s *Server) checkAllowedTools(w http.ResponseWriter, r *http.Request, orgID string, lim *policy.Limits) bool {
	if lim.AllowedTools == nil {
		return true
	}
	servers, err := s.st.ListMCPServers(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	var unknown []string
	for i, e := range lim.AllowedTools {
		e = strings.TrimSpace(e)
		lim.AllowedTools[i] = e
		if !policy.ValidToolEntry(e) {
			badRequest(w, "'"+e+"' is not a tool: write an MCP server's alias for all of its "+
				"tools, or alias/tool for one of them")
			return false
		}
		server, _, _ := strings.Cut(e, "/")
		if !slices.ContainsFunc(servers, func(m policy.MCPServer) bool { return m.Alias == server }) {
			unknown = append(unknown, server)
		}
	}
	if len(unknown) > 0 {
		badRequest(w, "this organisation has no MCP server called "+strings.Join(unknown, ", ")+
			" (see: keera mcp list)")
		return false
	}
	return true
}
