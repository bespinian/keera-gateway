package control

import (
	"cmp"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

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
	// Lowercased because identity providers' addresses are, and a sign-in
	// finds this row by exact match.
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		badRequest(w, "'email' is required")
		return
	}
	orgID, ok := s.orgToAdmin(w, p, in.OrgID)
	if !ok {
		return
	}
	role, err := newUserRole(in.ExternalID, in.Role, p.Unrestricted())
	if err != nil {
		refuse(w, err)
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
	signIn, err := newUserSignIn(in.SignIn, in.ExternalID, s.opts.Providers.Enabled(),
		s.opts.Passkeys != nil)
	if err != nil {
		refuse(w, err)
		return
	}
	passkey := signIn == authn.PasskeyProvider
	if passkey {
		if s.passkeysOff(w) || !s.mayVouchFor(w, r, p, orgID, email) {
			return
		}
		externalID = authn.PasskeyExternalID(userID)
	}
	user, err := s.st.AddUser(r.Context(), userID, orgID, email, externalID, string(role))
	switch {
	case errors.Is(err, store.ErrUserExists):
		// Adding never changes someone who is already here: a role change has
		// its own checks, and a subject is only ever set by a sign-in.
		conflict(w, "user_exists",
			email+" is already in this organisation; change their role with "+
				"PATCH /control/v1/users/{id} or keera user role")
		return
	case errors.Is(err, store.ErrExternalIDTaken):
		conflict(w, "external_id_taken",
			"someone in this organisation already has that 'external_id'")
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

// newUserRole checks who a person added by hand is, and returns the role they
// are added with. unrestricted says whether the caller is an operator.
func newUserRole(externalID, role string, unrestricted bool) (authn.Role, error) {
	// A subject decides whose sign-in becomes this person. An organisation's
	// administrator could name someone from another tenant's directory, so
	// only an operator may. Everyone else is linked on their first sign-in.
	if externalID != "" && !unrestricted {
		return "", forbidden("only an operator can set 'external_id'; leave it out, and the " +
			"person is linked to their identity on their first sign-in")
	}
	// Sign-in looks a person up by provider and subject together, so a bare
	// subject would never match, and the sign-in would be refused.
	if provider, subject, _ := strings.Cut(externalID, ":"); externalID != "" &&
		(provider == "" || subject == "") {
		return "", errors.New("'external_id' is the identity provider's name and the person's " +
			"subject there, as <provider>:<subject>, such as google:1234567890")
	}
	r := authn.Role(cmp.Or(role, string(authn.RoleMember)))
	if !r.Valid() {
		return "", errors.New("'role' must be admin or member")
	}
	return r, nil
}

// newUserSignIn decides how a person added by hand signs in: "sso" or
// authn.PasskeyProvider. requested is what the caller asked for; empty means
// the way this gateway signs people in, which is its identity provider, or
// passkeys when it has none.
func newUserSignIn(requested, externalID string, sso, passkeys bool) (string, error) {
	signIn := requested
	if signIn == "" {
		signIn = "sso"
		if !sso && passkeys {
			signIn = authn.PasskeyProvider
		}
	}
	switch signIn {
	case "sso":
		if sso {
			return signIn, nil
		}
		instead := "add a passkey account instead"
		if !passkeys {
			instead = "configure one, or turn on KEERA_PASSKEYS and add a passkey account"
		}
		return "", errors.New("this gateway has no identity provider, so nobody could sign in " +
			"to that account; " + instead)
	case authn.PasskeyProvider:
		if externalID != "" {
			return "", errors.New("a passkey account has no 'external_id'")
		}
		return signIn, nil
	}
	return "", errors.New("'sign_in' must be sso or passkey")
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
	target, ok := s.userToAdminister(w, r, p, userID)
	if !ok {
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
	if err := s.st.SetUserRole(r.Context(), writeOrg(p), userID, string(role)); err != nil {
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

// disableUser turns off a person who left: no sign-in, sessions or keys, and
// their sandboxes stop. Leaving the directory only stops new sign-ins, not the
// keys a person already has.
func (s *Server) disableUser(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	target, ok := s.userToSwitch(w, r, p, "disable")
	if !ok {
		return
	}
	revoked, err := s.st.DisableUser(r.Context(), writeOrg(p), target.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The keys must stop on every replica now, not a cache lifetime later.
	s.changed(r, target.OrgID)
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
	if err := s.st.EnableUser(r.Context(), writeOrg(p), target.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, target.OrgID, "user.enable", "user", target.ID,
		map[string]any{"email": target.Email})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": target.ID, "email": target.Email, "disabled": false,
	})
}

// userToAdminister reads a person, and checks the caller administers their
// organisation.
func (s *Server) userToAdminister(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	userID string,
) (store.User, bool) {
	target, err := s.st.UserByID(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return target, false
	}
	return target, s.requireOwnerAdmin(w, p, target.OrgID)
}

// userToSwitch reads the person a disable or an enable is for, and checks the
// caller may do it.
func (s *Server) userToSwitch(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	verb string,
) (store.User, bool) {
	target, ok := s.userToAdminister(w, r, p, r.PathValue("id"))
	if !ok {
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

// mayGrant reports whether the API may grant role. The operator role comes
// only from configuration (see authn.Role.Assignable), and where a directory
// group decides the admin role, the next sign-in would undo a grant made here.
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
