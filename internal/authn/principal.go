// Package authn is who the caller is, and what that lets them do.
//
// The control plane has three ways in: the operator key, for automation and
// for a new deployment with no identity provider yet; a browser session, for a
// person signed in through the identity provider; and a command-line token,
// for that same person at a terminal.
package authn

import (
	"errors"
	"fmt"
	"strings"
)

// Role is what a person may do.
type Role string

const (
	// RoleOperator crosses organisations and owns the model catalogue.
	RoleOperator Role = "operator"
	// RoleAdmin runs one organisation: its teams, guardrails, keys and people.
	RoleAdmin Role = "admin"
	// RoleMember sees their organisation's usage, manages their own keys and
	// changes no policy.
	RoleMember Role = "member"
)

// Valid reports whether r is a role Keera Gateway knows.
func (r Role) Valid() bool {
	return r == RoleOperator || r == RoleAdmin || r == RoleMember
}

// Assignable reports whether r is a role a request may grant.
//
// The operator role is not: it comes only from KEERA_OPERATORS or an operator
// group, so a tenant's administrator cannot use the panel to break out of
// their tenant.
func (r Role) Assignable() bool { return r == RoleAdmin || r == RoleMember }

// Method is how a caller proved who they are.
type Method string

// The ways a caller can prove who they are.
const (
	MethodOperatorKey Method = "operator_key"
	MethodSession     Method = "session"
	// MethodCLI is the same identity as a session, but sent in a header
	// rather than a cookie. It is its own method so that only it may skip the
	// cross-site checks a cookie needs.
	MethodCLI Method = "cli"
)

// Errors a caller can act on.
var (
	ErrUnauthenticated = errors.New("authn: not signed in")
	ErrForbidden       = errors.New("authn: not permitted")
)

// Principal is the authenticated caller.
type Principal struct {
	Via    Method `json:"via"`
	UserID string `json:"user_id,omitempty"`
	Email  string `json:"email,omitempty"`
	Role   Role   `json:"role"`
	// OrgID empty means every organisation: true of operators and the
	// operator key.
	OrgID string `json:"org_id,omitempty"`
	// CSRF is the double-submit token for a session. It is empty for the
	// operator key and the command line, which no browser sends by itself.
	CSRF string `json:"-"`
	// CredentialHash names the session or command-line token row, for
	// sign-out.
	CredentialHash []byte `json:"-"`
}

// Actor is what goes in the audit log. The operator key cannot say who used
// it, so it is recorded as "operator key".
func (p *Principal) Actor() string {
	if p.Via == MethodOperatorKey {
		return "operator key"
	}
	if p.Email != "" {
		return p.Email
	}
	return p.UserID
}

// Unrestricted reports whether this principal sees every organisation.
func (p *Principal) Unrestricted() bool {
	return p.Via == MethodOperatorKey || p.Role == RoleOperator
}

// CanReadOrg reports whether the principal may see an organisation's data.
func (p *Principal) CanReadOrg(orgID string) bool {
	return p.Unrestricted() || p.inOrg(orgID)
}

// CanAdminOrg reports whether the principal may change an organisation's teams,
// guardrails, keys and people.
func (p *Principal) CanAdminOrg(orgID string) bool {
	return p.Unrestricted() || (p.Role == RoleAdmin && p.inOrg(orgID))
}

// CanIssueKeyFor reports whether the principal may issue a key in an
// organisation, attributed to userID (empty means nobody in particular).
//
// A member may issue keys only for themselves. Otherwise they could put their
// spend under a colleague's name, or under nobody's.
func (p *Principal) CanIssueKeyFor(orgID, userID string) bool {
	return p.CanAdminOrg(orgID) || p.isMemberFor(orgID, userID)
}

// CanRevokeKeyFor reports whether the principal may revoke a key in an
// organisation, given who the key is attributed to (empty means nobody in
// particular).
//
// The rule is the same as for issuing. It adds no privilege, and it lets a
// member revoke a leaked key without waiting for an administrator. A key
// attributed to nobody, such as a pipeline's, stays an administrator's.
func (p *Principal) CanRevokeKeyFor(orgID, keyUserID string) bool {
	return p.CanAdminOrg(orgID) || p.isMemberFor(orgID, keyUserID)
}

// CanAdminCatalogue reports whether the principal may change the model
// catalogue. Every tenant shares it, so only an operator may.
func (p *Principal) CanAdminCatalogue() bool { return p.Unrestricted() }

func (p *Principal) inOrg(orgID string) bool {
	return orgID != "" && orgID == p.OrgID
}

// isMemberFor reports whether p is a member of orgID acting for themselves.
func (p *Principal) isMemberFor(orgID, userID string) bool {
	return p.Role == RoleMember && p.inOrg(orgID) &&
		p.UserID != "" && userID == p.UserID
}

// ScopeOrg resolves which organisation a request applies to.
//
// A principal bound to one organisation gets their own when the parameter is
// empty. Naming another one is refused rather than quietly rewritten, so a
// request never does something other than what it says.
func (p *Principal) ScopeOrg(requested string) (string, error) {
	if p.Unrestricted() {
		return requested, nil
	}
	if p.OrgID == "" {
		return "", fmt.Errorf("%w: this account is not attached to an organisation", ErrForbidden)
	}
	if requested != "" && requested != p.OrgID {
		return "", fmt.Errorf("%w: this account can only see its own organisation", ErrForbidden)
	}
	return p.OrgID, nil
}

// RoleMapping maps an identity provider's claims onto a Keera Gateway role.
//
// Groups are checked before the operator list, and the operator list before
// the default, so a directory that manages access through groups gets exactly
// what it says.
type RoleMapping struct {
	OperatorGroups []string
	AdminGroups    []string
	// OperatorEmails names operators by address. It is how the first operator
	// signs in before the directory has Keera groups, and the only way to be
	// an operator on a provider that issues no groups.
	OperatorEmails []string
	// Default is the role for someone who matches nothing.
	Default Role
}

// UsesGroups reports whether any role here is decided by group membership.
func (m RoleMapping) UsesGroups() bool {
	return anyNonBlank(m.OperatorGroups) || anyNonBlank(m.AdminGroups)
}

// DecidesAdmin reports whether a group here decides the administrator role.
//
// If one does, the directory owns the role and it is reapplied on every
// sign-in. If none does (for example Google Workspace, which issues no groups
// claim), the role is assigned in Keera and kept until someone changes it.
func (m RoleMapping) DecidesAdmin() bool { return anyNonBlank(m.AdminGroups) }

func anyNonBlank(ss []string) bool {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// RoleFor returns the role for an identity.
func (m RoleMapping) RoleFor(email string, groups []string) Role {
	if containsFold(groups, m.OperatorGroups) {
		return RoleOperator
	}
	if containsFold(groups, m.AdminGroups) {
		return RoleAdmin
	}
	for _, e := range m.OperatorEmails {
		if strings.EqualFold(strings.TrimSpace(e), email) {
			return RoleOperator
		}
	}
	if m.Default.Valid() {
		return m.Default
	}
	return RoleMember
}

func containsFold(have, want []string) bool {
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		for _, h := range have {
			if strings.EqualFold(strings.TrimSpace(h), w) {
				return true
			}
		}
	}
	return false
}
