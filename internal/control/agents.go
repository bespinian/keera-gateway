package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Keera Agents runs agents for people, started from a chat such as Slack. It
// signs people in through the gateway as Keera Chat does (see cliauth.go),
// and its server calls the routes here with KEERA_AGENTS_SERVICE_KEY.
//
// The service key belongs to a program, not to a person, so it gets only what
// Keera Agents needs: issuing and revoking keys for its agents, finding the
// person behind a chat message, and listing what such a key may use. It
// cannot read usage, change a guardrail or touch a key a person issued.
//
// The routes take nothing but the service key, and the service key opens
// nothing but these routes. So a person's sign-in cannot issue keys in
// somebody else's name through them, and a leaked service key cannot read or
// change anything else.

// serviceAuthenticated is authenticated for the service routes.
func (s *Server) serviceAuthenticated(next handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hasServiceKey {
			switchedOff(w, "agents_disabled", "this deployment does not work with Keera "+
				"Agents; its operator turns it on with KEERA_AGENTS_SERVICE_KEY")
			return
		}
		presented, err := auth.FromHeader(r.Header.Get("Authorization"))
		if err != nil || !constantTimeEqual(presented, s.serviceHash) {
			unauthorized(w, "unauthenticated", "this route is for Keera Agents; send its "+
				"service key as 'Authorization: Bearer <key>'")
			return
		}
		next(w, r, &authn.Principal{Via: authn.MethodService})
	})
}

// requireService is each service route's own check. serviceAuthenticated
// lets nothing else through; this keeps a handler closed should it ever be
// registered as an ordinary route, where the operator key would reach it.
func requireService(w http.ResponseWriter, p *authn.Principal) bool {
	if p.Via != authn.MethodService {
		forbid(w, "this route is for Keera Agents' server only")
		return false
	}
	return true
}

// missing answers 404 with a reason. Keera Agents is told what is missing,
// since it already knows every id it sends.
func missing(w http.ResponseWriter, msg string) {
	httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_found", msg)
}

// serviceOrg checks a service request named an organisation that exists.
func (s *Server) serviceOrg(w http.ResponseWriter, r *http.Request, orgID string) bool {
	if orgID == "" {
		needOrg(w, "name the organisation in 'org_id'")
		return false
	}
	_, err := s.st.OrgByID(r.Context(), orgID)
	if errors.Is(err, store.ErrNotFound) {
		missing(w, "there is no organisation "+orgID)
		return false
	}
	if err != nil {
		s.fail(w, err)
		return false
	}
	return true
}

// serviceKeyRequest is the body of POST /v1/service/keys.
type serviceKeyRequest struct {
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	// BudgetMicros caps what the key may spend in a calendar month. Absent or
	// zero sets no cap of its own; the project's and organisation's budgets
	// apply either way.
	BudgetMicros *int64 `json:"budget_micros"`
}

// serviceCreateKey issues a key for an agent, in the name of the person it
// works for. Their spend, guardrails and audit trail are theirs, as with a
// key an administrator issues them.
func (s *Server) serviceCreateKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !requireService(w, p) {
		return
	}
	var in serviceKeyRequest
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.UserID == "":
		badRequest(w, "name the person the key acts for in 'user_id'; a key for an agent "+
			"is always somebody's")
		return
	case in.Name == "":
		badRequest(w, "send a 'name' for the key; it is how people recognise it in the panel")
		return
	case in.BudgetMicros != nil && *in.BudgetMicros < 0:
		badRequest(w, "'budget_micros' cannot be negative; leave it out for no budget of the key's own")
		return
	}
	if !s.serviceOrg(w, r, in.OrgID) {
		return
	}
	// The person is who the spend is reported under, so they must be in this
	// organisation, by their row here.
	user, err := s.st.UserByID(r.Context(), in.UserID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user.OrgID != in.OrgID) {
		missing(w, "there is nobody "+in.UserID+" in this organisation")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if user.Disabled() {
		forbid(w, user.Email+" is disabled in this organisation, so a key for them would not work")
		return
	}
	// A key on another tenant's project would carry their guardrails and
	// spend their budget.
	if in.ProjectID != "" {
		owner, err := s.st.ProjectOrg(r.Context(), in.ProjectID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && owner != in.OrgID) {
			missing(w, "there is no project "+in.ProjectID+" in this organisation")
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
	}

	secret, hash, prefix := auth.Generate()
	info := store.KeyInfo{
		ID: id.New("key"), OrgID: in.OrgID, ProjectID: in.ProjectID, UserID: in.UserID,
		Name: in.Name, Prefix: prefix, Kind: policy.KeyStandard, IssuedBy: store.IssuedByAgents,
	}
	var budget int64
	if in.BudgetMicros != nil {
		budget = *in.BudgetMicros
	}
	if budget > 0 {
		// The key's own guardrail. It is written with the key, so the key
		// never works without it.
		info, err = s.st.CreateKeyWithLimits(r.Context(), info, hash, policy.Limits{
			Budgets: []policy.Budget{{BudgetMicros: budget, Period: policy.PeriodMonth}},
		})
	} else {
		info, err = s.st.CreateKey(r.Context(), info, hash)
	}
	if errors.Is(err, store.ErrNoProject) {
		noProject(w)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, info.OrgID, "key.create", "key", info.ID, map[string]any{
		"org_id": info.OrgID, "project_id": info.ProjectID, "user_id": info.UserID,
		"name": info.Name, "prefix": info.Prefix, "kind": info.Kind,
		"issued_by": info.IssuedBy, "budget_micros": budget,
	})
	s.keyChanged(r, info.ID)

	// The secret is returned exactly once, as on POST /v1/keys.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.KeyInfo
		Key string `json:"key"`
	}{KeyInfo: info, Key: secret})
}

// serviceRevokeKey revokes a key Keera Agents issued. Any other key answers
// as missing, so the service key cannot take a person's key away.
func (s *Server) serviceRevokeKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !requireService(w, p) {
		return
	}
	// The organisation is named although the id is unique, so a mix-up in
	// Keera Agents between two tenants fails instead of revoking.
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		needOrg(w, "name the key's organisation in ?org_id")
		return
	}
	keyID := r.PathValue("id")
	o, err := s.st.KeyOwnerOf(r.Context(), keyID)
	if errors.Is(err, store.ErrNotFound) ||
		(err == nil && (o.OrgID != orgID || o.IssuedBy != store.IssuedByAgents)) {
		missing(w, "Keera Agents issued no key "+keyID+" in this organisation")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	err = s.st.RevokeKey(r.Context(), orgID, keyID)
	if errors.Is(err, policy.ErrKeyRevoked) {
		// Somebody revoked it first, maybe in the panel. Either way it is
		// gone, which is all Keera Agents asked for.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "key.revoke", "key", keyID, map[string]any{
		"user_id": o.UserID, "issued_by": o.IssuedBy,
	})
	s.keyChanged(r, keyID)
	w.WriteHeader(http.StatusNoContent)
}

// serviceUser finds a person's membership of one organisation, by address or
// by id. Keera Agents maps the sender of a chat message to a person with it,
// and notices an owner who was disabled.
func (s *Server) serviceUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !requireService(w, p) {
		return
	}
	q := r.URL.Query()
	orgID, email, userID := q.Get("org_id"), strings.TrimSpace(q.Get("email")), q.Get("id")
	if orgID == "" {
		needOrg(w, "name the organisation in ?org_id")
		return
	}
	if (email == "") == (userID == "") {
		badRequest(w, "send ?email or ?id, one of the two")
		return
	}
	var u store.User
	var err error
	if email != "" {
		u, err = s.st.UserInOrgByEmail(r.Context(), orgID, email)
	} else if u, err = s.st.UserByID(r.Context(), userID); err == nil && u.OrgID != orgID {
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		missing(w, "that person is not in this organisation")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "email": u.Email, "name": u.Name, "role": u.Role, "disabled": u.Disabled(),
	})
}

// serviceCatalog lists what a key Keera Agents issues in a project may use:
// the chat models and routers, and the MCP servers.
func (s *Server) serviceCatalog(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !requireService(w, p) {
		return
	}
	q := r.URL.Query()
	orgID, projectID := q.Get("org_id"), q.Get("project_id")
	if !s.serviceOrg(w, r, orgID) {
		return
	}
	projects, err := s.st.Projects(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(projects) == 0 {
		noProject(w)
		return
	}
	// As for a key: no project means the oldest, which Projects lists first.
	if projectID == "" {
		projectID = projects[0].ID
	} else if !slices.ContainsFunc(projects, func(p store.Project) bool { return p.ID == projectID }) {
		missing(w, "there is no project "+projectID+" in this organisation")
		return
	}

	org, project, err := s.chainLimits(r.Context(), orgID, projectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The keys it issues are standard, with no allow-list of their own.
	res := policy.Resolve(policy.Key{OrgID: orgID, ProjectID: projectID, Kind: policy.KeyStandard},
		&org, project, nil)
	catalogue, err := s.st.ListModels(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	routers, err := s.st.ListRouters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	servers, err := s.st.ListMCPServers(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, catalogueOf(projectID, projects, res, catalogue, routers, servers))
}

// serviceCatalogue is the answer of GET /v1/service/catalog.
type serviceCatalogue struct {
	ProjectID  string           `json:"project_id"`
	Projects   []serviceProject `json:"projects"`
	Models     []serviceModel   `json:"models"`
	MCPServers []serviceServer  `json:"mcp_servers"`
}

type serviceProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// serviceModel is a chat model or a router.
type serviceModel struct {
	Alias string `json:"alias"`
	// Kind is "model" or "router": a router is named like a model, but
	// decides by itself where a request goes.
	Kind string `json:"kind"`
}

type serviceServer struct {
	Alias string `json:"alias"`
}

// catalogueOf picks what res allows from the organisation's catalogue.
func catalogueOf(projectID string, projects []store.Project, res *policy.Resolved,
	catalogue []policy.Model, routers []policy.Router, servers []policy.MCPServer,
) serviceCatalogue {
	out := serviceCatalogue{
		ProjectID: projectID, Projects: []serviceProject{},
		Models: []serviceModel{}, MCPServers: []serviceServer{},
	}
	for _, p := range projects {
		out.Projects = append(out.Projects, serviceProject{ID: p.ID, Name: p.Name})
	}
	// A standard key cannot reach subscription models, which MayUse checks.
	for _, m := range catalogue {
		if m.Enabled && m.Kind == policy.KindChat && res.MayUse(m) {
			out.Models = append(out.Models, serviceModel{Alias: m.Alias, Kind: "model"})
		}
	}
	// A router is named and allowed like a model.
	for _, r := range routers {
		if res.MayCallRouter(r, modelFinder(catalogue)) {
			out.Models = append(out.Models, serviceModel{Alias: r.Alias, Kind: "router"})
		}
	}
	for _, sv := range servers {
		if sv.Enabled && res.AllowsServer(sv.Alias) {
			out.MCPServers = append(out.MCPServers, serviceServer{Alias: sv.Alias})
		}
	}
	return out
}
