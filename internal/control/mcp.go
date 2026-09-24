package control

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The MCP servers the gateway stands in front of, and the log of their tool
// calls. The catalogue is shared by every tenant, like the models, so only an
// operator changes it. See docs/mcp.md.

// listMCPServers is readable by anyone signed in: a developer needs to know
// which servers exist to connect to them. Their addresses are the operator's,
// so only an operator sees them.
func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	servers, err := s.st.LoadMCPServers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for i := range servers {
		if !p.CanAdminCatalogue() {
			servers[i].URL = ""
			servers[i].AuthHeader = ""
			servers[i].APIKeyEnv = ""
			servers[i].HasAPIKey = false
		}
		servers[i].APIKeyCiphertext = nil
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": servers})
}

func (s *Server) putMCPServer(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminCatalogue() {
		s.forbid(w, "an MCP server is shared by every tenant; only an operator can change one")
		return
	}
	var body struct {
		URL         string `json:"url"`
		Description string `json:"description"`
		AuthHeader  string `json:"auth_header"`
		APIKeyEnv   string `json:"api_key_env"`
		// Enabled is a pointer, so leaving it out keeps a server on.
		Enabled *bool `json:"enabled"`
		// APIKey is write-only. Nil leaves the stored one alone, and "" clears it.
		APIKey        *string `json:"api_key"`
		FromCatalogue bool    `json:"from_catalogue"`
	}
	if err := httpx.ReadJSON(r, &body); err != nil {
		badRequest(w, err.Error())
		return
	}
	m, err := catalog.ParseMCPServer(catalog.MCPServer{
		Alias: r.PathValue("alias"), URL: body.URL, Description: body.Description,
		AuthHeader: body.AuthHeader, APIKeyEnv: body.APIKeyEnv,
		Disabled: body.Enabled != nil && !*body.Enabled,
	})
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	m.Managed = body.FromCatalogue
	credential := ""
	if body.APIKey != nil {
		credential = strings.TrimSpace(*body.APIKey)
	}
	if credential != "" && !s.opts.Secrets.Enabled() {
		badRequest(w, "this deployment cannot store a credential: set KEERA_SECRET_KEY (openssl rand -hex 32) "+
			"and restart, or name an environment variable in 'api_key_env' instead")
		return
	}
	switch existing, err := s.st.MCPServer(r.Context(), m.Alias); {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		s.fail(w, err)
		return
	case existing.Managed && !body.FromCatalogue:
		if !existing.SameDeclaration(m) {
			httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "mcp_server_is_managed",
				"the MCP server "+m.Alias+" is declared in this deployment's catalogue file, which is "+
					"applied on every start: change it there, or remove it from the file to take "+
					"it over here")
			return
		}
		// Only the credential changes, so the server stays managed.
		m.Managed = true
	}
	if err := s.st.UpsertMCPServer(r.Context(), m); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, "", "mcp_server.put", "mcp_server", m.Alias, m)
	if body.APIKey != nil {
		if err := s.setMCPCredential(r, p, m.Alias, credential); err != nil {
			s.fail(w, err)
			return
		}
		m.HasAPIKey = credential != ""
	}
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, m)
}

// setMCPCredential seals a pasted credential, or clears the stored one.
func (s *Server) setMCPCredential(r *http.Request, p *authn.Principal, alias, credential string) error {
	var sealed []byte
	if credential != "" {
		var err error
		if sealed, err = s.opts.Secrets.Seal(registry.MCPSecretName(alias), credential); err != nil {
			return err
		}
	}
	if err := s.st.SetMCPCredential(r.Context(), alias, sealed); err != nil {
		return err
	}
	action := "mcp_server.credential.set"
	if credential == "" {
		action = "mcp_server.credential.clear"
	}
	s.auditf(r, p, "", action, "mcp_server", alias, nil)
	return nil
}

func (s *Server) deleteMCPServer(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminCatalogue() {
		s.forbid(w, "only an operator can remove an MCP server")
		return
	}
	alias := r.PathValue("alias")
	switch existing, err := s.st.MCPServer(r.Context(), alias); {
	case err != nil:
		s.fail(w, err)
		return
	case existing.Managed:
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "mcp_server_is_managed",
			"the MCP server "+alias+" is declared in this deployment's catalogue file and would be "+
				"applied again on the next start: remove it from the file instead")
		return
	}
	if err := s.st.DeleteMCPServer(r.Context(), alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, "", "mcp_server", alias)
}

// toolCalls is the tool-call log of one organisation, newest first, or with
// summary=true the calls of each tool added up.
func (s *Server) toolCalls(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the tool-call log")
		return
	}
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return
	}
	q := r.URL.Query()
	tq := store.ToolCallQuery{
		OrgID: orgID, TeamID: sc.TeamID, KeyID: sc.KeyID, UserID: sc.UserID,
		Server: q.Get("server"), Tool: q.Get("tool"), From: from, To: to,
	}
	tq.Limit, _ = strconv.Atoi(q.Get("limit"))
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
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "data": data})
}

// checkAllowedTools refuses a tool allow-list with an entry that is not one,
// or that names a server the catalogue does not hold. Such an entry allows
// nothing, and a typo is better caught now than as a tool that never works.
func (s *Server) checkAllowedTools(w http.ResponseWriter, r *http.Request, lim *policy.Limits) bool {
	if lim.AllowedTools == nil {
		return true
	}
	servers, err := s.st.LoadMCPServers(r.Context())
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
		badRequest(w, "no MCP server is called "+strings.Join(unknown, ", ")+
			" (see: keera mcp list)")
		return false
	}
	return true
}
