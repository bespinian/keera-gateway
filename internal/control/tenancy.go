package control

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// ------------------------------------------------------------------- orgs

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.Unrestricted() {
		forbid(w, "only an operator can create an organisation")
		return
	}
	var in struct {
		Name        string `json:"name"`
		EmailDomain string `json:"email_domain"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		badRequest(w, "a non-empty 'name' is required")
		return
	}
	domain, err := policy.CleanEmailDomain(in.EmailDomain)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	org, err := s.st.CreateOrg(r.Context(),
		store.Org{ID: id.New("org"), Name: name, EmailDomain: domain}, s.opts.Template)
	if err != nil {
		s.failOrg(w, err)
		return
	}
	s.auditf(r, p, org.ID, "org.create", "org", org.ID, org)
	// Its models, copied from the template, are for the gateway to serve now.
	s.changed(r)
	httpx.WriteJSON(w, http.StatusCreated, org)
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgs, err := s.st.ListOrgs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	// A caller bound to one organisation sees only that one. Filtering rather
	// than refusing saves the panel a second code path.
	if !p.Unrestricted() {
		kept := orgs[:0]
		for _, o := range orgs {
			if o.ID == p.OrgID {
				kept = append(kept, o)
			}
		}
		orgs = kept
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": orgs})
}

func (s *Server) updateOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.Unrestricted() {
		forbid(w, "only an operator can change an organisation")
		return
	}
	var in struct {
		Name        *string `json:"name"`
		EmailDomain *string `json:"email_domain"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == nil && in.EmailDomain == nil {
		badRequest(w, "send 'name', 'email_domain' or both; an empty 'email_domain' clears it")
		return
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			badRequest(w, "'name' cannot be empty")
			return
		}
		in.Name = &name
	}
	if in.EmailDomain != nil {
		domain, err := policy.CleanEmailDomain(*in.EmailDomain)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		in.EmailDomain = &domain
	}
	orgID := r.PathValue("id")
	org, err := s.st.UpdateOrg(r.Context(), orgID,
		store.OrgChange{Name: in.Name, EmailDomain: in.EmailDomain})
	if err != nil {
		s.failOrg(w, err)
		return
	}
	s.auditf(r, p, orgID, "org.update", "org", orgID, org)
	httpx.WriteJSON(w, http.StatusOK, org)
}

// failOrg answers a name or domain another organisation already holds with a
// message that says so, instead of a generic internal error.
func (s *Server) failOrg(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrOrgNameTaken):
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "org_name_taken",
			"another organisation already has that name")
	case errors.Is(err, store.ErrDomainTaken):
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "domain_taken",
			"that email domain belongs to another organisation; "+
				"one domain places sign-ins in one organisation, so it can only be set on one")
	default:
		s.fail(w, err)
	}
}

func (s *Server) deleteOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.Unrestricted() {
		forbid(w, "only an operator can delete an organisation")
		return
	}
	orgID := r.PathValue("id")
	// Deleting your own organisation would delete your own account and
	// session, and maybe the last operator with them. This is a guard, not a
	// permission: another operator, or the operator key, can still do it.
	if p.OrgID != "" && p.OrgID == orgID {
		forbid(w, "you cannot delete the organisation you are signed in to; "+
			"use another operator's account or the operator key")
		return
	}
	gone, err := s.st.DeleteOrg(r.Context(), orgID)
	if errors.Is(err, store.ErrOrgHasSandboxes) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "org_has_sandboxes",
			fmt.Sprintf("%s still has %d live sandbox(es); terminate them first "+
				"(keera sandbox list --org %s), so none keeps running after its "+
				"organisation is gone", gone.Name, gone.LiveSandboxes, orgID))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "org.delete", "org", orgID, gone)
	// Every key of the tenant is gone, so the gateways must drop them now.
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": gone.ID, "name": gone.Name, "deleted": true,
		"projects": gone.Projects, "users": gone.Users, "keys": gone.Keys,
	})
}

// ------------------------------------------------------------------- projects

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID       string `json:"org_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		badRequest(w, "a non-empty 'name' is required")
		return
	}
	orgID, ok := s.requireOrg(w, p, in.OrgID, orgRequired)
	if !ok || !s.requireOrgAdmin(w, p, orgID) {
		return
	}
	project, err := s.st.CreateProject(r.Context(), store.Project{
		ID: id.New("project"), OrgID: orgID, Name: name,
		Description: strings.TrimSpace(in.Description),
	})
	if errors.Is(err, store.ErrProjectNameTaken) {
		projectNameTaken(w, name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "project.create", "project", project.ID, project)
	httpx.WriteJSON(w, http.StatusCreated, project)
}

// listProjects returns projects with their guardrails and current spend, which is
// what the panel shows on one screen.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	projects, err := s.st.ProjectSummaries(r.Context(), orgID, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": projects})
}

// projectOrg resolves the organisation of a project named in the path and checks
// that the caller administers it, before anything is written.
func (s *Server) projectOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	projectID string,
) (string, bool) {
	// A caller who administers nothing is refused before the read, so a
	// member cannot probe which project ids exist.
	if !s.requireAdmin(w, p) {
		return "", false
	}
	owner, err := s.st.ProjectOrg(r.Context(), projectID)
	if err != nil {
		s.fail(w, err)
		return "", false
	}
	if !s.requireOwnerAdmin(w, p, owner) {
		return "", false
	}
	return owner, true
}

// requireProjectInOrg refuses a project from another organisation. The foreign key
// only says that the project exists somewhere.
func (s *Server) requireProjectInOrg(w http.ResponseWriter, r *http.Request, projectID, orgID string) bool {
	owner, err := s.st.ProjectOrg(r.Context(), projectID)
	return s.inOrg(w, orgID, owner, err)
}

// updateProject renames a project, changes its description, or both: the
// only fields of a project that are not an id or a guardrail.
func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == nil && in.Description == nil {
		badRequest(w, "send 'name', 'description' or both; an empty 'description' clears it")
		return
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			badRequest(w, "'name' cannot be empty")
			return
		}
		in.Name = &name
	}
	if in.Description != nil {
		description := strings.TrimSpace(*in.Description)
		in.Description = &description
	}
	projectID := r.PathValue("id")
	orgID, ok := s.projectOrg(w, r, p, projectID)
	if !ok {
		return
	}
	project, err := s.st.UpdateProject(r.Context(), projectID,
		store.ProjectChange{Name: in.Name, Description: in.Description})
	if errors.Is(err, store.ErrProjectNameTaken) {
		projectNameTaken(w, *in.Name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "project.update", "project", projectID, project)
	httpx.WriteJSON(w, http.StatusOK, project)
}

// firstProject returns the project a sandbox goes in when none was named: the
// organisation's oldest, as for keys. It answers the refusal itself when the
// organisation has no project.
func (s *Server) firstProject(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	projectID, err := s.st.FirstProject(r.Context(), orgID)
	if errors.Is(err, store.ErrNotFound) {
		noProject(w)
		return "", false
	}
	if err != nil {
		s.fail(w, err)
		return "", false
	}
	return projectID, true
}

// noProject refuses a key in an organisation with no projects. Every key is
// in one, so there can be no keys until a project is created.
func noProject(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "project_required",
		"this organisation has no projects, and every key is in one; create a project first")
}

// projectNameTaken refuses a second project of one name. Two would make
// every report that names a project ambiguous.
func projectNameTaken(w http.ResponseWriter, name string) {
	httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "project_name_taken",
		"another project in this organisation is already called '"+name+"'")
}

// deleteProject removes a project once no live key is bound to it.
//
// The store refuses while a key still works. This turns that into a message
// naming the keys, because "in use" alone leaves the administrator guessing.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	projectID := r.PathValue("id")
	orgID, ok := s.projectOrg(w, r, p, projectID)
	if !ok {
		return
	}
	gone, err := s.st.DeleteProject(r.Context(), projectID)
	if inUse, ok := errors.AsType[*store.ProjectInUseError](err); ok {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "project_has_keys",
			"revoke the keys of '"+inUse.Project+"' first: "+strings.Join(inUse.Keys, ", "))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "project.delete", "project", projectID, gone)
	// The project's guardrails are gone, so the gateways must drop them now.
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": gone.ID, "name": gone.Name, "deleted": true,
		"detached_keys": gone.DetachedKeys,
	})
}

// ------------------------------------------------------------------- users

func (s *Server) addUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID      string `json:"org_id"`
		Email      string `json:"email"`
		ExternalID string `json:"external_id"`
		Role       string `json:"role"`
		// SignIn is "passkey" for an account that signs in with passkeys.
		// Empty is the identity provider.
		SignIn string `json:"sign_in"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	email := strings.TrimSpace(in.Email)
	if email == "" {
		badRequest(w, "'email' is required")
		return
	}
	orgID, ok := s.requireOrg(w, p, in.OrgID, orgRequired)
	if !ok || !s.requireOrgAdmin(w, p, orgID) {
		return
	}
	// A subject decides whose sign-in becomes this person. An organisation's
	// administrator could name someone from another tenant's directory, so
	// only an operator may. Everyone else is linked on their first sign-in.
	if in.ExternalID != "" && !p.Unrestricted() {
		forbid(w, "only an operator can set 'external_id'; leave it out, and the "+
			"person is linked to their identity on their first sign-in")
		return
	}
	role := authn.Role(in.Role)
	if in.Role == "" {
		role = authn.RoleMember
	}
	if !role.Valid() {
		badRequest(w, "'role' must be admin or member")
		return
	}
	// Adding someone as a member grants nothing, and places them in the right
	// organisation before their first sign-in. That works even where roles
	// come from the directory; a higher role does not.
	if role != authn.RoleMember && !s.mayGrant(w, role) {
		return
	}
	userID := id.New("user")
	externalID := in.ExternalID
	passkey := false
	switch in.SignIn {
	case "", "sso":
	case authn.PasskeyProvider:
		if s.passkeysOff(w) {
			return
		}
		if in.ExternalID != "" {
			badRequest(w, "a passkey account has no 'external_id'")
			return
		}
		if !s.mayVouchFor(w, r, p, orgID, email) {
			return
		}
		externalID, passkey = authn.PasskeyExternalID(userID), true
	default:
		badRequest(w, "'sign_in' must be sso or passkey")
		return
	}
	user, err := s.st.AddUser(r.Context(), userID, orgID, email, externalID, string(role))
	switch {
	case errors.Is(err, store.ErrUserExists):
		// Adding never changes someone who is already here: a role change has
		// its own checks, and a subject is only ever set by a sign-in.
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "user_exists",
			email+" is already in this organisation; change their role with "+
				"PATCH /control/v1/users/{id} or keera user role")
		return
	case errors.Is(err, store.ErrExternalIDTaken):
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "external_id_taken",
			"another person already has that 'external_id'")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "user.create", "user", user.ID, user)
	if !passkey {
		httpx.WriteJSON(w, http.StatusCreated, user)
		return
	}
	// A passkey account is no use without its first passkey, so the link
	// comes with it.
	link, err := s.issuePasskeyLink(r, p, user)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.User
		PasskeyLink passkeyLinkOut `json:"passkey_link"`
	}{user, link})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	users, err := s.st.ListUsers(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": users})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Role string `json:"role"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	role := authn.Role(in.Role)
	if !role.Valid() {
		badRequest(w, "'role' must be admin or member")
		return
	}
	// Whether a role can be granted at all does not depend on the user, so it
	// is checked before reading the row.
	if !s.mayGrant(w, role) {
		return
	}
	userID := r.PathValue("id")
	target, err := s.st.UserByID(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.requireOwnerAdmin(w, p, target.OrgID) {
		return
	}
	if target.Role == string(authn.RoleOperator) {
		// The operator role comes from configuration, and the next sign-in
		// would restore it.
		s.forbidOperator(w, target.Email)
		return
	}
	if target.ID == p.UserID && role != p.Role {
		// Dropping your own last privilege could lock everyone out.
		forbid(w, "you cannot change your own role")
		return
	}
	if err := s.st.SetUserRole(r.Context(), userID, string(role)); err != nil {
		s.fail(w, err)
		return
	}
	// A role only applies from the next request, so sessions holding the old
	// one are ended.
	if err := s.st.DeleteUserSessions(r.Context(), userID); err != nil {
		s.log.Warn("signing out a user after a role change failed", "error", err, "user", userID)
	}
	s.auditf(r, p, target.OrgID, "user.set_role", "user", userID, map[string]any{"role": role})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": userID, "role": role})
}

// disableUser turns a person off, for when they leave. They cannot sign in,
// every session and key they had stops working, and their sandboxes stop.
//
// It works the same with single sign-on: leaving the directory stops new
// sign-ins, but not the keys a person already has. This does.
func (s *Server) disableUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	target, ok := s.userToSwitch(w, r, p, "disable")
	if !ok {
		return
	}
	revoked, err := s.st.DisableUser(r.Context(), target.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The keys must stop on every replica now, not a cache lifetime later.
	s.changed(r)
	out := map[string]any{
		"id": target.ID, "email": target.Email, "disabled": true, "revoked_keys": revoked,
	}
	if s.opts.Sandboxes != nil {
		stopped, err := s.opts.Sandboxes.Offboard(r.Context(), target.ID)
		out["sandboxes"] = stopped
		switch {
		case err != nil:
			s.log.Warn("reading a disabled person's sandboxes failed", "error", err,
				"user", target.ID)
			out["warning"] = "their sandboxes could not be read, so none were stopped; " +
				"their keys are revoked, so the sandboxes cannot reach the gateway"
		case stopped.Failed > 0:
			out["warning"] = strconv.Itoa(stopped.Failed) + " of their sandboxes could not " +
				"be stopped; their keys are revoked, and the log says why"
		}
	}
	s.auditf(r, p, target.OrgID, "user.disable", "user", target.ID, map[string]any{
		"email": target.Email, "revoked_keys": revoked, "sandboxes": out["sandboxes"],
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}

// enableUser lets a disabled person sign in again. Their old keys stay
// revoked, so they start with none.
func (s *Server) enableUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	target, ok := s.userToSwitch(w, r, p, "enable")
	if !ok {
		return
	}
	if err := s.st.EnableUser(r.Context(), target.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, target.OrgID, "user.enable", "user", target.ID,
		map[string]any{"email": target.Email})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": target.ID, "email": target.Email, "disabled": false,
	})
}

// userToSwitch reads the person a disable or an enable is for, and checks the
// caller may do it.
func (s *Server) userToSwitch(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	verb string,
) (store.User, bool) {
	target, err := s.st.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return target, false
	}
	if !s.requireOwnerAdmin(w, p, target.OrgID) {
		return target, false
	}
	if target.ID == p.UserID {
		forbid(w, "you cannot "+verb+" yourself")
		return target, false
	}
	if target.Role == string(authn.RoleOperator) {
		// Operators come from configuration, and an administrator must not be
		// able to lock them out.
		s.forbidOperator(w, target.Email)
		return target, false
	}
	return target, true
}

// forbidOperator refuses a change to an operator, whose role comes from the
// gateway's configuration and not from here.
func (s *Server) forbidOperator(w http.ResponseWriter, email string) {
	forbid(w, email+" is an operator through the gateway's configuration; "+
		"remove the address from KEERA_OPERATORS, or the person from a group in "+
		"KEERA_OIDC_<NAME>_OPERATOR_GROUPS")
}

// mayGrant reports whether a role may be granted through the API at all.
//
// The operator role never is (see authn.Role.Assignable), so configuration is
// the one place that says who the operators are. And where a directory group
// decides the admin role, no role is granted here: the next sign-in would undo
// it.
func (s *Server) mayGrant(w http.ResponseWriter, role authn.Role) bool {
	if !role.Assignable() {
		forbid(w, "the operator role is granted by KEERA_OPERATORS or a group in "+
			"KEERA_OIDC_<NAME>_OPERATOR_GROUPS, and cannot be assigned here")
		return false
	}
	if s.opts.Providers.AdminFromDirectory() {
		forbid(w, "roles come from the identity provider on this deployment: "+
			"change this person's group membership in the directory, or unset "+
			"KEERA_OIDC_<NAME>_ADMIN_GROUPS to assign roles here")
		return false
	}
	return true
}

// -------------------------------------------------------------------- keys

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID     string         `json:"org_id"`
		ProjectID string         `json:"project_id"`
		UserID    string         `json:"user_id"`
		Name      string         `json:"name"`
		Kind      policy.KeyKind `json:"kind"`
		ExpiresIn string         `json:"expires_in"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Kind == "" {
		in.Kind = policy.KeyStandard
	}
	if !in.Kind.Valid() {
		badRequest(w, "'kind' must be standard or subscription")
		return
	}
	orgID, ok := s.requireOrg(w, p, in.OrgID, orgRequired)
	if !ok {
		return
	}
	// Only an administrator issues keys, so each one has the project and
	// guardrails they chose for it. A key a member issued themselves would have
	// only the organisation's, and step around their project's.
	if !p.CanAdminOrg(orgID) {
		forbid(w, "only an administrator of this organisation can issue a key; "+
			"ask one for a key in your name, and rotate it yourself from then on")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "unnamed"
	}
	// A subscription key goes next to one person's own Claude sign-in, so it is
	// always somebody's.
	if in.Kind == policy.KeySubscription && in.UserID == "" {
		badRequest(w, "a subscription key is for one person's Claude Code, so it must be "+
			"attributed to them; name them in 'user_id'")
		return
	}
	// The person a key is attributed to is who its spend is reported under,
	// so it must be someone in this organisation.
	if in.UserID != "" {
		user, err := s.st.UserByID(r.Context(), in.UserID)
		if !s.inOrg(w, orgID, user.OrgID, err) {
			return
		}
		if user.Disabled() {
			forbid(w, user.Email+" is disabled, so a key for them would not work; "+
				"enable them first")
			return
		}
	}
	// A key on another tenant's project would carry their system prompt, spend
	// their budget and use their rate limit.
	if in.ProjectID != "" && !s.requireProjectInOrg(w, r, in.ProjectID, orgID) {
		return
	}
	info := store.KeyInfo{
		ID: id.New("key"), OrgID: orgID, ProjectID: in.ProjectID, UserID: in.UserID, Name: in.Name,
		Kind: in.Kind,
	}
	if info.ExpiresAt, ok = expiresIn(w, in.ExpiresIn); !ok {
		return
	}

	secret, hash, prefix := auth.Generate()
	info.Prefix = prefix
	info, err := s.st.CreateKey(r.Context(), info, hash)
	if errors.Is(err, store.ErrNoProject) {
		noProject(w)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// The attribution decides whose budget and report the spend lands in, so
	// it is part of the record.
	s.auditf(r, p, orgID, "key.create", "key", info.ID, map[string]any{
		"org_id": info.OrgID, "project_id": info.ProjectID, "user_id": info.UserID,
		"name": info.Name, "prefix": info.Prefix, "kind": info.Kind,
	})
	s.changed(r)

	// The secret is returned exactly once. Nothing stores it, so nobody can
	// read a developer's key later.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.KeyInfo
		Key string `json:"key"`
	}{KeyInfo: info, Key: secret})
}

// rotateKey replaces a key with a new one that keeps its project, person, kind
// and own guardrails, and revokes the old one. Whoever may revoke a key may rotate
// it, so a member can replace their own leaked key.
//
// A member cannot choose the new key's lifetime. It keeps the old one's, so a
// key an administrator issued for 30 days is not rotated into one for years.
func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name      string `json:"name"`
		ExpiresIn string `json:"expires_in"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	oldID := r.PathValue("id")
	owner, _, ok := s.keyToManage(w, r, p, oldID, "rotate")
	if !ok {
		return
	}
	if in.ExpiresIn != "" && !p.CanAdminOrg(owner) {
		forbid(w, "a member cannot choose how long a key lasts; "+
			"the new key keeps the old one's lifetime")
		return
	}
	next := store.KeyInfo{ID: id.New("key"), Name: strings.TrimSpace(in.Name)}
	if next.ExpiresAt, ok = expiresIn(w, in.ExpiresIn); !ok {
		return
	}
	secret, hash, prefix := auth.Generate()
	next.Prefix = prefix
	info, err := s.st.RotateKey(r.Context(), oldID, next, hash)
	if errors.Is(err, store.ErrKeyRevoked) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "key_revoked",
			"that key was already revoked, so there is nothing to rotate; issue a new one")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, owner, "key.rotate", "key", info.ID, map[string]any{
		"replaced": oldID, "project_id": info.ProjectID, "user_id": info.UserID,
		"name": info.Name, "prefix": info.Prefix, "kind": info.Kind,
	})
	s.changed(r)

	// The secret is returned exactly once, as on create.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.KeyInfo
		Key      string `json:"key"`
		Replaced string `json:"replaced"`
	}{KeyInfo: info, Key: secret, Replaced: oldID})
}

// renameKey changes what a key is called. Whoever may revoke a key may rename
// it. The name is only a label, so nothing else about the key changes.
func (s *Server) renameKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		badRequest(w, "send the new 'name'; it cannot be empty")
		return
	}
	keyID := r.PathValue("id")
	owner, holder, ok := s.keyToManage(w, r, p, keyID, "rename")
	if !ok {
		return
	}
	old, err := s.st.RenameKey(r.Context(), keyID, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, owner, "key.rename", "key", keyID, map[string]any{
		"user_id": holder, "from": old, "to": name,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": keyID, "name": name})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	q := r.URL.Query()
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	// Spend uses the same window as the projects screen, so the two agree.
	since := policy.PeriodMonth.Start(time.Now())
	keys, err := s.st.KeySummaries(r.Context(), store.KeyQuery{
		OrgID: orgID, ProjectID: q.Get("project_id"), Since: since,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data": keys, "currency": s.opts.Currency, "since": since,
	})
}

// keyToManage reads whose key keyID is, and checks the caller may verb it.
// Another tenant's key answers 404, not 403, so ids cannot be probed. Inside
// their organisation a member can already see every key, so they are told
// whose key it is rather than "not found".
func (s *Server) keyToManage(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	keyID, verb string,
) (owner, holder string, ok bool) {
	o, err := s.st.KeyOwnerOf(r.Context(), keyID)
	if err != nil {
		s.fail(w, err)
		return "", "", false
	}
	owner, holder = o.OrgID, o.UserID
	if !p.CanReadOrg(owner) {
		s.fail(w, store.ErrNotFound)
		return "", "", false
	}
	if !p.CanManageKeyFor(owner, holder) {
		forbid(w, "a member can only "+verb+" a key attributed to themselves; "+
			"somebody else's, or one attributed to nobody, is for an "+
			"administrator of this organisation")
		return "", "", false
	}
	return owner, holder, true
}

// expiresIn reads a key's 'expires_in'. Empty means the key does not expire.
func expiresIn(w http.ResponseWriter, in string) (*time.Time, bool) {
	if in == "" {
		return nil, true
	}
	d, err := time.ParseDuration(in)
	if err != nil || d <= 0 {
		badRequest(w, "'expires_in' must be a positive duration such as 720h")
		return nil, false
	}
	t := time.Now().Add(d)
	return &t, true
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	keyID := r.PathValue("id")
	owner, holder, ok := s.keyToManage(w, r, p, keyID, "revoke")
	if !ok {
		return
	}
	if err := s.st.RevokeKey(r.Context(), keyID); err != nil {
		s.fail(w, err)
		return
	}
	// The holder tells a member revoking their own leaked key apart from an
	// administrator taking someone's access away.
	s.auditf(r, p, owner, "key.revoke", "key", keyID, map[string]any{"user_id": holder})
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": keyID, "revoked": true})
}
