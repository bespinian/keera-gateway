package control

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

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
	name, ok := requiredName(w, in.Name)
	if !ok {
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
	s.changed(r, org.ID)
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
		orgs = slices.DeleteFunc(orgs, func(o store.Org) bool { return o.ID != p.OrgID })
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
		// Limited false lifts the limit of an organisation that signed up.
		Limited *bool `json:"limited"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == nil && in.EmailDomain == nil && in.Limited == nil {
		badRequest(w, "send 'name', 'email_domain' or 'limited'; an empty 'email_domain' clears it")
		return
	}
	if !renamed(w, in.Name) {
		return
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
		store.OrgChange{Name: in.Name, EmailDomain: in.EmailDomain, Limited: in.Limited})
	if err != nil {
		s.failOrg(w, err)
		return
	}
	s.auditf(r, p, orgID, "org.update", "org", orgID, org)
	if in.Limited != nil {
		// What the organisation's models may reach changed.
		s.changed(r, orgID)
	}
	httpx.WriteJSON(w, http.StatusOK, org)
}

// failOrg answers a name or domain another organisation already holds with a
// message that says so, instead of a generic internal error.
func (s *Server) failOrg(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrOrgNameTaken):
		conflict(w, "org_name_taken",
			"another organisation already has that name")
	case errors.Is(err, store.ErrDomainTaken):
		conflict(w, "domain_taken",
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
		conflict(w, "org_has_sandboxes",
			fmt.Sprintf("%s still has %d live sandbox(es); terminate them first "+
				"(KEERA_ORG=%s keera sandbox list), so none keeps running after its "+
				"organisation is gone", gone.Name, gone.LiveSandboxes, orgID))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if gone.CardToken != 0 && s.opts.Payments != nil {
		s.deleteToken(r.Context(), s.opts.Payments, gone.CardToken)
	}
	s.auditf(r, p, orgID, "org.delete", "org", orgID, gone)
	// Every key of the tenant is gone, so the gateways must drop them now.
	s.changed(r, orgID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": gone.ID, "name": gone.Name, "deleted": true,
		"projects": gone.Projects, "users": gone.Users, "keys": gone.Keys,
	})
}

// requiredName trims the name of something new, and refuses one left empty.
func requiredName(w http.ResponseWriter, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		badRequest(w, "a non-empty 'name' is required")
		return "", false
	}
	return name, true
}

// renamed trims the new name a change sends, and refuses one left empty. Nil
// keeps the name.
func renamed(w http.ResponseWriter, name *string) bool {
	if name == nil {
		return true
	}
	*name = strings.TrimSpace(*name)
	if *name == "" {
		badRequest(w, "'name' cannot be empty")
		return false
	}
	return true
}
