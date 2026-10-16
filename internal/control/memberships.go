package control

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A person can be in several organisations, with a role in each. Each is a
// row in users, and the rows share the identity the person first signed in
// with. A session or a command-line token is in one of them at a time, and
// switching moves it to another. See docs/sso.md.

// lastOrgCookie remembers the organisation this browser was last in, so the
// next sign-in lands there. It only chooses among the person's own.
const lastOrgCookie = "keera_org"

// lastOrgMaxAge is a year: it is a preference, not a credential.
const lastOrgMaxAge = 365 * 24 * 60 * 60

func (s *Server) rememberOrg(orgID string) *http.Cookie {
	return s.cookie(lastOrgCookie, orgID, "/", lastOrgMaxAge, http.SameSiteLaxMode)
}

// lastOrg picks where a sign-in lands among the person's organisations: the
// one this browser was last in, else the one they used last anywhere. ms is
// never empty, and is ordered by last use. When every row is disabled, the
// sign-in lands on one and is refused there.
func lastOrg(r *http.Request, ms []store.Membership) string {
	if c, err := r.Cookie(lastOrgCookie); err == nil {
		for _, m := range ms {
			if m.OrgID == c.Value && !m.Disabled {
				return m.OrgID
			}
		}
	}
	for _, m := range ms {
		if !m.Disabled {
			return m.OrgID
		}
	}
	return ms[0].OrgID
}

// vouchedByDirectory reports whether an external ID comes from an identity
// provider, which proved the address, rather than from the gateway itself: a
// passkey account or the operator key's stand-in.
func vouchedByDirectory(externalID string) bool {
	name, _, ok := strings.Cut(externalID, ":")
	return ok && name != authn.ReservedProvider
}

// sameIdentity refuses a new identity at an address that another identity
// provider already signs in. A person keeps the provider they first signed in
// with, in every organisation, so whoever controls the address at a second
// provider cannot become them.
//
// It is only asked of an identity without a row yet: two identities linked
// before this rule keep working.
func (s *Server) sameIdentity(ctx context.Context, identity authn.Identity) error {
	others, err := s.st.ExternalIDsFor(ctx, identity.Email)
	if err != nil {
		return err
	}
	for _, ext := range others {
		if ext == identity.ExternalID() || !vouchedByDirectory(ext) {
			continue
		}
		name, _, _ := strings.Cut(ext, ":")
		if p := s.opts.Providers.ByName(name); p != nil {
			name = p.Label()
		}
		return errors.New(identity.Email + " signs in through " + name +
			", the way it first signed in; sign in that way")
	}
	return nil
}

// directoryOrg is the organisation a directory's addresses at email's domain
// land in: the one with that domain, else the only one there is. It is empty
// when there is none.
func (s *Server) directoryOrg(ctx context.Context, email string) (string, error) {
	org, err := s.st.OrgByEmailDomain(ctx, (authn.Identity{Email: email}).Domain())
	if err == nil {
		return org.ID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if s.opts.Providers.SignUp() {
		return "", nil
	}
	org, err = s.st.OnlyOrg(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return org.ID, err
}

// orgsOf lists the organisations a person can switch to: the ones they are in
// and, for an address a directory proved, the ones that invited it.
func (s *Server) orgsOf(ctx context.Context, p *authn.Principal) ([]store.Membership, error) {
	if p.UserID == "" {
		return nil, nil
	}
	user, err := s.st.UserByID(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	ms, err := s.st.Memberships(ctx, user.ExternalID, user.Email,
		vouchedByDirectory(user.ExternalID))
	if err != nil {
		return nil, err
	}
	kept := ms[:0]
	for _, m := range ms {
		if !m.Disabled {
			kept = append(kept, m)
		}
	}
	// By name, as a menu reads.
	slices.SortFunc(kept, func(a, b store.Membership) int {
		return cmp.Compare(strings.ToLower(a.OrgName), strings.ToLower(b.OrgName))
	})
	return kept, nil
}

// switchOrg moves the caller's session or command-line token to another of
// their organisations. Choosing one that invited them accepts the
// invitation.
func (s *Server) switchOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		OrgID string `json:"org_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if p.UserID == "" || len(p.CredentialHash) == 0 {
		badRequest(w, "the operator key works in every organisation; name one with "+
			"org_id on each request instead")
		return
	}
	if in.OrgID == "" {
		badRequest(w, "'org_id' is required")
		return
	}
	ctx := r.Context()
	cur, err := s.st.UserByID(ctx, p.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	ms, err := s.st.Memberships(ctx, cur.ExternalID, cur.Email, vouchedByDirectory(cur.ExternalID))
	if err != nil {
		s.fail(w, err)
		return
	}
	var m *store.Membership
	for i := range ms {
		if ms[i].OrgID == in.OrgID {
			m = &ms[i]
		}
	}
	switch {
	case m == nil:
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_a_member",
			"you are not in that organisation; GET /control/v1/me lists the ones you are in")
		return
	case m.Disabled:
		forbid(w, "your access to "+m.OrgName+" has been turned off; ask an administrator there")
		return
	}

	if m.OrgID == cur.OrgID {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"org_id": cur.OrgID, "org_name": m.OrgName, "role": cur.Role,
		})
		return
	}

	var target store.User
	if m.Invited {
		target, err = s.st.AcceptInvitation(ctx, m.OrgID, cur.Email, cur.ExternalID)
	} else {
		target, err = s.st.UserByID(ctx, m.UserID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrEmailTaken):
		conflict(w, "invitation_gone",
			"that invitation is no longer there")
		return
	case err != nil:
		s.fail(w, err)
		return
	}

	// The operator role comes from the configuration and is the person's, so
	// it goes where they go. The current row has it right: its sign-in or
	// directory check set it last.
	was := target.Role
	if isOp := cur.Role == string(authn.RoleOperator); isOp != (target.Role == string(authn.RoleOperator)) {
		want := authn.RoleMember
		if isOp {
			want = authn.RoleOperator
		}
		if err := s.st.SetUserRole(ctx, target.ID, string(want)); err != nil {
			s.fail(w, err)
			return
		}
		target.Role = string(want)
	}

	move := s.st.MoveSession
	if p.Via == authn.MethodCLI {
		move = s.st.MoveCLIToken
	}
	if err := move(ctx, p.CredentialHash, target.ID); errors.Is(err, store.ErrNotFound) {
		unauthorized(w, "unauthenticated", "this sign-in has expired; sign in again")
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	if p.Via == authn.MethodSession {
		http.SetCookie(w, s.rememberOrg(target.OrgID))
	}

	entry := map[string]any{"role": target.Role}
	if m.Invited {
		entry["joined"] = true
	}
	if was != target.Role {
		entry["role_was"] = was
	}
	if p.Via == authn.MethodCLI {
		entry["client"] = "command line"
	}
	s.auditf(r, &authn.Principal{Via: p.Via, Email: target.Email, UserID: target.ID},
		target.OrgID, "auth.switch_org", "user", target.ID, entry)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"org_id": target.OrgID, "org_name": m.OrgName, "role": target.Role,
	})
}
