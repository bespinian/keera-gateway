package control

import (
	"errors"
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
		s.forbid(w, "only an operator can create an organisation")
		return
	}
	var in struct {
		Name        string `json:"name"`
		EmailDomain string `json:"email_domain"`
	}
	err := httpx.ReadJSON(r, &in)
	name := strings.TrimSpace(in.Name)
	if err != nil || name == "" {
		badRequest(w, "a non-empty 'name' is required")
		return
	}
	org, err := s.st.CreateOrg(r.Context(), id.New("org"), name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if domain := strings.TrimSpace(in.EmailDomain); domain != "" {
		// The organisation exists by now, so only the domain is reported as
		// failed. Failing the whole call would make the operator create the
		// organisation a second time.
		if err := s.st.SetOrgEmailDomain(r.Context(), org.ID, domain); err != nil {
			s.failDomain(w, err)
			return
		}
		org.EmailDomain = domain
	}
	s.auditf(r, p, org.ID, "org.create", "org", org.ID, org)
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
		s.forbid(w, "only an operator can change an organisation's identity mapping")
		return
	}
	var in struct {
		EmailDomain *string `json:"email_domain"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil || in.EmailDomain == nil {
		badRequest(w, "'email_domain' is required; send an empty string to clear it")
		return
	}
	orgID := r.PathValue("id")
	if err := s.st.SetOrgEmailDomain(r.Context(), orgID, *in.EmailDomain); err != nil {
		s.failDomain(w, err)
		return
	}
	s.auditf(r, p, orgID, "org.update", "org", orgID, in)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": orgID, "email_domain": *in.EmailDomain})
}

// failDomain answers a domain another organisation already holds with a
// message that says so, instead of a generic internal error.
func (s *Server) failDomain(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrDomainTaken) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "domain_taken",
			"that email domain belongs to another organisation; "+
				"one domain places sign-ins in one tenant, so it can only be set on one")
		return
	}
	s.fail(w, err)
}

func (s *Server) deleteOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.Unrestricted() {
		s.forbid(w, "only an operator can delete an organisation")
		return
	}
	orgID := r.PathValue("id")
	// Deleting your own organisation would delete your own account and
	// session, and maybe the last operator with them. This is a guard, not a
	// permission: another operator, or the operator key, can still do it.
	if p.OrgID != "" && p.OrgID == orgID {
		s.forbid(w, "you cannot delete the organisation you are signed in to; "+
			"use another operator's account or the operator key")
		return
	}
	gone, err := s.st.DeleteOrg(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "org.delete", "org", orgID, gone)
	// Every key of the tenant is gone, so the gateways must drop them now.
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": gone.ID, "name": gone.Name, "deleted": true,
		"teams": gone.Teams, "users": gone.Users, "keys": gone.Keys,
	})
}

// ------------------------------------------------------------------- teams

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID string `json:"org_id"`
		Name  string `json:"name"`
	}
	err := httpx.ReadJSON(r, &in)
	name := strings.TrimSpace(in.Name)
	if err != nil || name == "" {
		badRequest(w, "a non-empty 'name' is required")
		return
	}
	orgID, ok := s.scopeOrg(w, p, in.OrgID)
	if !ok {
		return
	}
	if orgID == "" {
		badRequest(w, "'org_id' is required")
		return
	}
	if !s.requireOrgAdmin(w, p, orgID) {
		return
	}
	team, err := s.st.CreateTeam(r.Context(), id.New("team"), orgID, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "team.create", "team", team.ID, team)
	httpx.WriteJSON(w, http.StatusCreated, team)
}

// listTeams returns teams with their guardrails and current spend, which is
// what the panel shows on one screen.
func (s *Server) listTeams(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.scopeOrg(w, p, r.URL.Query().Get("org_id"))
	if !ok {
		return
	}
	teams, err := s.st.TeamSummaries(r.Context(), orgID, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": teams})
}

// teamOrg resolves the organisation of a team named in the path and checks
// that the caller administers it, before anything is written.
func (s *Server) teamOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	teamID string,
) (string, bool) {
	// A caller who administers nothing is refused before the read, so a
	// member cannot probe which team ids exist.
	if !p.Unrestricted() && p.Role != authn.RoleAdmin {
		s.forbid(w, "only an administrator of this organisation can do that")
		return "", false
	}
	owner, err := s.st.TeamOrg(r.Context(), teamID)
	if err != nil {
		s.fail(w, err)
		return "", false
	}
	orgID, ok := s.scopeOrg(w, p, owner)
	if !ok || !s.requireOrgAdmin(w, p, orgID) {
		return "", false
	}
	return orgID, true
}

// requireTeamInOrg refuses a team from another organisation. The foreign key
// only says that the team exists somewhere.
func (s *Server) requireTeamInOrg(w http.ResponseWriter, r *http.Request, teamID, orgID string) bool {
	owner, err := s.st.TeamOrg(r.Context(), teamID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if owner != orgID {
		s.forbid(w, "that team is not in this organisation")
		return false
	}
	return true
}

// updateTeam renames a team, the only field of a team that is not an id or a
// guardrail.
func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name string `json:"name"`
	}
	err := httpx.ReadJSON(r, &in)
	name := strings.TrimSpace(in.Name)
	if err != nil || name == "" {
		badRequest(w, "a non-empty 'name' is required")
		return
	}
	teamID := r.PathValue("id")
	orgID, ok := s.teamOrg(w, r, p, teamID)
	if !ok {
		return
	}
	team, err := s.st.RenameTeam(r.Context(), teamID, name)
	if errors.Is(err, store.ErrTeamNameTaken) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "team_name_taken",
			"another team in this organisation is already called '"+name+"'; "+
				"two teams of one name make every report ambiguous, so the name is held to one")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "team.update", "team", teamID, team)
	httpx.WriteJSON(w, http.StatusOK, team)
}

// deleteTeam removes a team once no live key is bound to it.
//
// The store refuses while a key still works. This turns that into a message
// naming the keys, because "in use" alone leaves the administrator guessing.
func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	teamID := r.PathValue("id")
	orgID, ok := s.teamOrg(w, r, p, teamID)
	if !ok {
		return
	}
	gone, err := s.st.DeleteTeam(r.Context(), teamID)
	var inUse *store.TeamInUseError
	if errors.As(err, &inUse) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "team_has_keys",
			"'"+inUse.Team+"' still holds "+keyCount(len(inUse.Aliases))+" that have not been "+
				"revoked ("+strings.Join(inUse.Aliases, ", ")+"). Deleting the team would take "+
				"them with it and whatever uses them would start answering 401, so revoke them "+
				"or move them to another team first")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "team.delete", "team", teamID, gone)
	// The team's guardrails are gone, so the gateways must drop them now.
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": gone.ID, "name": gone.Name, "deleted": true,
		"detached_keys": gone.DetachedKeys,
	})
}

// keyCount writes "1 key" or "n keys".
func keyCount(n int) string {
	if n == 1 {
		return "1 key"
	}
	return strconv.Itoa(n) + " keys"
}

// ------------------------------------------------------------------- users

func (s *Server) upsertUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID      string `json:"org_id"`
		Email      string `json:"email"`
		ExternalID string `json:"external_id"`
		Role       string `json:"role"`
	}
	err := httpx.ReadJSON(r, &in)
	email := strings.TrimSpace(in.Email)
	if err != nil || email == "" {
		badRequest(w, "'email' is required")
		return
	}
	orgID, ok := s.scopeOrg(w, p, in.OrgID)
	if !ok || !s.requireOrgAdmin(w, p, orgID) {
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
	user, err := s.st.UpsertUser(r.Context(), id.New("user"), orgID,
		email, in.ExternalID, string(role))
	if err != nil {
		s.fail(w, err)
		return
	}
	// This may change the role of someone who already exists. A role only
	// applies from the next request, so their sessions are ended.
	if err := s.st.DeleteUserSessions(r.Context(), user.ID); err != nil {
		s.log.Warn("signing out a user after an upsert failed", "error", err, "user", user.ID)
	}
	s.auditf(r, p, orgID, "user.put", "user", user.ID, user)
	httpx.WriteJSON(w, http.StatusOK, user)
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
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, "'role' is required")
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
	if !s.requireOrgAdmin(w, p, target.OrgID) {
		return
	}
	if target.Role == string(authn.RoleOperator) {
		// The operator role comes from configuration, and the next sign-in
		// would restore it.
		s.forbid(w, target.Email+" is an operator through the gateway's configuration; "+
			"remove the address from KEERA_OPERATORS, or the person from a group in "+
			"KEERA_OIDC_OPERATOR_GROUPS")
		return
	}
	if target.ID == p.UserID && role != p.Role {
		// Dropping your own last privilege could lock everyone out.
		s.forbid(w, "you cannot change your own role")
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
	if !s.requireOrgAdmin(w, p, target.OrgID) {
		return target, false
	}
	if target.ID == p.UserID {
		s.forbid(w, "you cannot "+verb+" yourself")
		return target, false
	}
	if target.Role == string(authn.RoleOperator) {
		// Operators come from configuration, and an administrator must not be
		// able to lock them out.
		s.forbid(w, target.Email+" is an operator through the gateway's configuration; "+
			"remove the address from KEERA_OPERATORS, or the person from a group in "+
			"KEERA_OIDC_OPERATOR_GROUPS")
		return target, false
	}
	return target, true
}

// mayGrant reports whether a role may be granted through the API at all.
//
// The operator role never is (see authn.Role.Assignable), so configuration is
// the one place that says who the operators are. And where a directory group
// decides the admin role, no role is granted here: the next sign-in would undo
// it.
func (s *Server) mayGrant(w http.ResponseWriter, role authn.Role) bool {
	if !role.Assignable() {
		s.forbid(w, "the operator role is granted by KEERA_OPERATORS or a group in "+
			"KEERA_OIDC_OPERATOR_GROUPS, and cannot be assigned here")
		return false
	}
	if s.opts.Providers.AdminFromDirectory() {
		s.forbid(w, "roles come from the identity provider on this deployment: "+
			"change this person's group membership in the directory, or unset "+
			"KEERA_OIDC_ADMIN_GROUPS to assign roles here")
		return false
	}
	return true
}

// -------------------------------------------------------------------- keys

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID     string `json:"org_id"`
		TeamID    string `json:"team_id"`
		UserID    string `json:"user_id"`
		Alias     string `json:"alias"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	orgID, ok := s.scopeOrg(w, p, in.OrgID)
	if !ok {
		return
	}
	if orgID == "" {
		badRequest(w, "'org_id' is required")
		return
	}
	// A member may only issue keys for themselves, so leaving out the person
	// means "me".
	if in.UserID == "" && !p.CanAdminOrg(orgID) {
		in.UserID = p.UserID
	}
	if !p.CanIssueKeyFor(orgID, in.UserID) {
		s.forbid(w, "a member can only issue a key attributed to themselves; "+
			"issuing one for somebody else is for an administrator of this organisation")
		return
	}
	// Nor may a member pick the team. This is refused rather than ignored, so
	// nobody gets a key that belongs somewhere else than they asked. A
	// member's key uses the organisation's own guardrails.
	if in.TeamID != "" && !p.CanAdminOrg(orgID) {
		s.forbid(w, "a member cannot choose the team a key belongs to; "+
			"the key is issued against this organisation's own guardrails")
		return
	}
	if in.Alias == "" {
		in.Alias = "unnamed"
	}
	// The person a key is attributed to is who its spend is reported under,
	// so it must be someone in this organisation.
	if in.UserID != "" {
		user, err := s.st.UserByID(r.Context(), in.UserID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if user.OrgID != orgID {
			s.forbid(w, "that person is not in this organisation")
			return
		}
		if user.Disabled() {
			s.forbid(w, user.Email+" is disabled, so a key for them would not work; "+
				"enable them first")
			return
		}
	}
	// A key on another tenant's team would carry their system prompt, spend
	// their budget and use their rate limit.
	if in.TeamID != "" && !s.requireTeamInOrg(w, r, in.TeamID, orgID) {
		return
	}
	info := store.KeyInfo{
		ID: id.New("key"), OrgID: orgID, TeamID: in.TeamID, UserID: in.UserID, Alias: in.Alias,
	}
	if in.ExpiresIn != "" {
		d, err := time.ParseDuration(in.ExpiresIn)
		if err != nil || d <= 0 {
			badRequest(w, "'expires_in' must be a positive duration such as 720h")
			return
		}
		t := time.Now().Add(d)
		info.ExpiresAt = &t
	}

	secret, hash, prefix, err := auth.Generate()
	if err != nil {
		s.fail(w, err)
		return
	}
	info.Prefix = prefix
	info, err = s.st.CreateKey(r.Context(), info, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The attribution decides whose budget and report the spend lands in, so
	// it is part of the record.
	s.auditf(r, p, orgID, "key.create", "key", info.ID, map[string]any{
		"org_id": info.OrgID, "team_id": info.TeamID, "user_id": info.UserID,
		"alias": info.Alias, "prefix": info.Prefix,
	})
	s.changed(r)

	// The secret is returned exactly once. Nothing stores it, so nobody can
	// read a developer's key later.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.KeyInfo
		Key string `json:"key"`
	}{KeyInfo: info, Key: secret})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	q := r.URL.Query()
	orgID, ok := s.scopeOrg(w, p, q.Get("org_id"))
	if !ok {
		return
	}
	// Spend uses the same window as the teams screen, so the two agree.
	keys, err := s.st.KeySummaries(r.Context(), store.KeyQuery{
		OrgID: orgID, TeamID: q.Get("team_id"),
		Since: policy.PeriodMonth.Start(time.Now()),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data": keys, "currency": s.opts.Currency, "since": policy.PeriodMonth.Start(time.Now()),
	})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	keyID := r.PathValue("id")
	// Another tenant's key answers 404, not 403, so ids cannot be probed.
	owner, holder, err := s.st.KeyOwner(r.Context(), keyID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !p.CanReadOrg(owner) {
		s.fail(w, store.ErrNotFound)
		return
	}
	// Inside their organisation a member can already see every key, so they
	// are told whose key it is rather than "not found".
	if !p.CanRevokeKeyFor(owner, holder) {
		s.forbid(w, "a member can only revoke a key attributed to themselves; "+
			"somebody else's, or one attributed to nobody, is for an "+
			"administrator of this organisation")
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
