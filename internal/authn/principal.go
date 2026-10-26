// Package authn is who the caller is, and what that lets them do.
//
// The control plane has three ways in: the operator key, for automation and
// new deployments; a browser session; and a command-line token.
package authn

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type Role string

const (
	// RoleOperator runs the deployment and works in any organisation.
	RoleOperator Role = "operator"
	// RoleAdmin runs one organisation.
	RoleAdmin Role = "admin"
	// RoleMember sees usage and their own requests, manages their own keys
	// and changes no policy.
	RoleMember Role = "member"
)

func (r Role) Valid() bool {
	return r == RoleOperator || r == RoleAdmin || r == RoleMember
}

// Assignable reports whether a request may grant r. The operator role comes
// only from KEERA_OPERATORS or an operator group, so an administrator cannot
// break out of their tenant.
func (r Role) Assignable() bool { return r == RoleAdmin || r == RoleMember }

type Method string

const (
	MethodOperatorKey Method = "operator_key"
	MethodSession     Method = "session"
	// MethodCLI is a session sent in a header rather than a cookie, so it
	// skips the cross-site checks a cookie needs.
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
	Name   string `json:"name,omitempty"`
	Role   Role   `json:"role"`
	// OrgID is empty only for the operator key. Unrestricted, not an empty
	// OrgID, lets a caller see every organisation.
	OrgID string `json:"org_id,omitempty"`
	// CSRF is the double-submit token for a session. It is empty for the
	// operator key and the command line, which no browser sends by itself.
	CSRF string `json:"-"`
	// CredentialHash names the session or token row, for sign-out.
	CredentialHash []byte `json:"-"`
	// SignedInAt matters because adding a passkey needs a recent sign-in.
	SignedInAt time.Time `json:"-"`
}

// Actor is what goes in the audit log. The operator key is recorded as
// "operator key".
func (p *Principal) Actor() string {
	if p.Via == MethodOperatorKey {
		return "operator key"
	}
	if p.Email != "" {
		return p.Email
	}
	return p.UserID
}

func (p *Principal) Unrestricted() bool {
	return p.Via == MethodOperatorKey || p.Role == RoleOperator
}

func (p *Principal) CanReadOrg(orgID string) bool {
	return p.Unrestricted() || p.inOrg(orgID)
}

func (p *Principal) CanAdminOrg(orgID string) bool {
	return p.Unrestricted() || (p.Role == RoleAdmin && p.inOrg(orgID))
}

// CanManageKeyFor reports whether the principal may manage a key in orgID
// attributed to userID ("" for nobody). A member may manage their own, so
// they can stop a leaked one. A key of nobody's stays an administrator's.
func (p *Principal) CanManageKeyFor(orgID, userID string) bool {
	return p.CanAdminOrg(orgID) ||
		(p.Role == RoleMember && p.inOrg(orgID) && p.UserID != "" && userID == p.UserID)
}

func (p *Principal) inOrg(orgID string) bool {
	return orgID != "" && orgID == p.OrgID
}

// ScopeOrg resolves which organisation a request applies to. Empty means the
// principal's own. Naming another is refused rather than quietly rewritten.
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

// RoleMapping maps an identity provider's claims to a role. The operator
// groups and KEERA_OPERATORS are checked first, then the admin groups, then
// the default.
type RoleMapping struct {
	OperatorGroups []string
	AdminGroups    []string
	// OperatorEmails lets the first operator sign in before the directory has
	// Keera groups, and is the only way on a provider with no groups.
	OperatorEmails []string
	// Default is the role for someone who matches nothing.
	Default Role
}

func (m RoleMapping) UsesGroups() bool {
	return anyNonBlank(m.OperatorGroups) || anyNonBlank(m.AdminGroups)
}

// DecidesAdmin reports whether a group decides the administrator role. Then
// the directory owns it and it is reapplied on every sign-in. Otherwise it is
// assigned in Keera.
func (m RoleMapping) DecidesAdmin() bool { return anyNonBlank(m.AdminGroups) }

func anyNonBlank(ss []string) bool {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

func (m RoleMapping) RoleFor(email string, groups []string) Role {
	if containsFold(groups, m.OperatorGroups) {
		return RoleOperator
	}
	// An address in KEERA_OPERATORS is an operator, whatever the directory
	// says.
	for _, e := range m.OperatorEmails {
		if strings.EqualFold(strings.TrimSpace(e), email) {
			return RoleOperator
		}
	}
	if containsFold(groups, m.AdminGroups) {
		return RoleAdmin
	}
	if m.Default.Assignable() {
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
