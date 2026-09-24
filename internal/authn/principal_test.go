package authn

import (
	"errors"
	"testing"
)

func TestScopeOrgRefusesRatherThanRewrites(t *testing.T) {
	admin := &Principal{Via: MethodSession, Role: RoleAdmin, OrgID: "org_a"}

	// Leaving the parameter off means "mine".
	if got, err := admin.ScopeOrg(""); err != nil || got != "org_a" {
		t.Errorf("ScopeOrg(\"\") = %q, %v; want org_a", got, err)
	}
	// Naming your own is fine.
	if got, err := admin.ScopeOrg("org_a"); err != nil || got != "org_a" {
		t.Errorf("ScopeOrg(own) = %q, %v", got, err)
	}
	// Naming someone else's is refused. Quietly rewriting it to your own would
	// answer a question nobody asked.
	if _, err := admin.ScopeOrg("org_b"); !errors.Is(err, ErrForbidden) {
		t.Errorf("ScopeOrg(other) err = %v, want ErrForbidden", err)
	}
}

func TestScopeOrgIsUnrestrictedForOperatorsAndTheOperatorKey(t *testing.T) {
	for _, p := range []*Principal{
		{Via: MethodSession, Role: RoleOperator},
		{Via: MethodOperatorKey, Role: RoleOperator},
	} {
		if got, err := p.ScopeOrg("org_b"); err != nil || got != "org_b" {
			t.Errorf("%s: ScopeOrg = %q, %v", p.Via, got, err)
		}
		// Empty means every organisation, not "none".
		if got, err := p.ScopeOrg(""); err != nil || got != "" {
			t.Errorf("%s: ScopeOrg(\"\") = %q, %v", p.Via, got, err)
		}
	}
}

func TestScopeOrgRefusesAnAccountWithNoOrganisation(t *testing.T) {
	orphan := &Principal{Via: MethodSession, Role: RoleMember}
	if _, err := orphan.ScopeOrg(""); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden - an unattached account must not see everything", err)
	}
}

func TestPermissions(t *testing.T) {
	operator := &Principal{Via: MethodSession, Role: RoleOperator}
	admin := &Principal{Via: MethodSession, Role: RoleAdmin, OrgID: "org_a"}
	member := &Principal{Via: MethodSession, Role: RoleMember, OrgID: "org_a"}

	tests := []struct {
		name                      string
		p                         *Principal
		org                       string
		read, adminOrg, catalogue bool
	}{
		{"operator sees any organisation", operator, "org_b", true, true, true},
		{"admin sees their own", admin, "org_a", true, true, false},
		{"admin sees no other", admin, "org_b", false, false, false},
		{"member reads their own", member, "org_a", true, false, false},
		{"member administers nothing", member, "org_a", true, false, false},
		{"member sees no other", member, "org_b", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.CanReadOrg(tc.org); got != tc.read {
				t.Errorf("CanReadOrg(%s) = %v, want %v", tc.org, got, tc.read)
			}
			if got := tc.p.CanAdminOrg(tc.org); got != tc.adminOrg {
				t.Errorf("CanAdminOrg(%s) = %v, want %v", tc.org, got, tc.adminOrg)
			}
			if got := tc.p.CanAdminCatalogue(); got != tc.catalogue {
				t.Errorf("CanAdminCatalogue() = %v, want %v", got, tc.catalogue)
			}
		})
	}
}

func TestCanIssueKeyForIsSelfOnlyForAMember(t *testing.T) {
	operatorKey := &Principal{Via: MethodOperatorKey, Role: RoleOperator}
	admin := &Principal{Via: MethodSession, Role: RoleAdmin, OrgID: "org_a", UserID: "user_admin"}
	member := &Principal{Via: MethodSession, Role: RoleMember, OrgID: "org_a", UserID: "user_1"}

	tests := []struct {
		name string
		p    *Principal
		org  string
		user string
		want bool
	}{
		{"member issues their own", member, "org_a", "user_1", true},
		{"member cannot issue for a colleague", member, "org_a", "user_2", false},
		// Attributed to nobody is what an administrator issues for a shared
		// pipeline. A member's key is the one thing a member may create, and a
		// key attributed to nobody is not theirs.
		{"member cannot issue an unattributed key", member, "org_a", "", false},
		{"member cannot issue into another organisation", member, "org_b", "user_1", false},
		{"admin issues for anybody in their own", admin, "org_a", "user_1", true},
		{"admin issues for nobody in particular", admin, "org_a", "", true},
		{"admin cannot issue into another organisation", admin, "org_b", "user_1", false},
		{"the operator key issues anywhere", operatorKey, "org_a", "user_1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.CanIssueKeyFor(tc.org, tc.user); got != tc.want {
				t.Errorf("CanIssueKeyFor(%q, %q) = %v, want %v", tc.org, tc.user, got, tc.want)
			}
		})
	}
}

// A member whose session carries no user row has nobody to attribute a key to,
// so "issue your own" has no meaning for them. It must not collapse into
// "issue one attributed to nobody", which is an administrator's key.
func TestCanIssueKeyForNeedsAUser(t *testing.T) {
	orphan := &Principal{Via: MethodSession, Role: RoleMember, OrgID: "org_a"}
	if orphan.CanIssueKeyFor("org_a", "") {
		t.Error("a member with no user id was allowed to issue a key")
	}
}

// Revoking is scoped the same way as issuing, with one deliberate difference:
// there is no "attributed to nobody means me" anywhere near it. Such a key is
// the shared one an administrator issued for a pipeline, and a member who could
// revoke it could stop work that was never theirs.
func TestCanRevokeKeyForIsSelfOnlyForAMember(t *testing.T) {
	operatorKey := &Principal{Via: MethodOperatorKey, Role: RoleOperator}
	admin := &Principal{Via: MethodSession, Role: RoleAdmin, OrgID: "org_a", UserID: "user_admin"}
	member := &Principal{Via: MethodSession, Role: RoleMember, OrgID: "org_a", UserID: "user_1"}

	tests := []struct {
		name string
		p    *Principal
		org  string
		user string
		want bool
	}{
		{"member revokes their own", member, "org_a", "user_1", true},
		{"member cannot revoke a colleague's", member, "org_a", "user_2", false},
		{"member cannot revoke an unattributed key", member, "org_a", "", false},
		{"member cannot reach into another organisation", member, "org_b", "user_1", false},
		{"admin revokes anybody's in their own", admin, "org_a", "user_1", true},
		{"admin revokes an unattributed key", admin, "org_a", "", true},
		{"admin cannot reach into another organisation", admin, "org_b", "user_1", false},
		{"the operator key revokes anywhere", operatorKey, "org_a", "user_1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.CanRevokeKeyFor(tc.org, tc.user); got != tc.want {
				t.Errorf("CanRevokeKeyFor(%q, %q) = %v, want %v", tc.org, tc.user, got, tc.want)
			}
		})
	}
}

// A member with no user row of their own has no key attributed to them, so
// "revoke your own" must not collapse into revoking an unattributed one.
func TestCanRevokeKeyForNeedsAUser(t *testing.T) {
	orphan := &Principal{Via: MethodSession, Role: RoleMember, OrgID: "org_a"}
	if orphan.CanRevokeKeyFor("org_a", "") {
		t.Error("a member with no user id was allowed to revoke an unattributed key")
	}
}

func TestEmptyOrgIDNeverMatches(t *testing.T) {
	// A row with no organisation must not be readable by everyone whose own
	// organisation is also unset.
	orphan := &Principal{Via: MethodSession, Role: RoleAdmin}
	if orphan.CanReadOrg("") || orphan.CanAdminOrg("") {
		t.Error("an empty organisation id matched an empty principal organisation")
	}
}

func TestActorNamesAPersonWhenThereIsOne(t *testing.T) {
	if got := (&Principal{Via: MethodOperatorKey}).Actor(); got != "operator key" {
		t.Errorf("Actor() = %q; the shared credential cannot claim to be a person", got)
	}
	session := &Principal{Via: MethodSession, Email: "ada@example.ch", UserID: "user_1"}
	if got := session.Actor(); got != "ada@example.ch" {
		t.Errorf("Actor() = %q, want the email", got)
	}
	noEmail := &Principal{Via: MethodSession, UserID: "user_1"}
	if got := noEmail.Actor(); got != "user_1" {
		t.Errorf("Actor() = %q, want the user id as a fallback", got)
	}
}

func TestRoleForPrefersTheDirectory(t *testing.T) {
	m := RoleMapping{
		OperatorGroups: []string{"keera-operators"},
		AdminGroups:    []string{"keera-admins", " AI-Platform-Admins "},
		OperatorEmails: []string{"operator@example.ch"},
		Default:        RoleMember,
	}
	tests := []struct {
		name   string
		email  string
		groups []string
		want   Role
	}{
		{"an operator group wins", "x@example.ch", []string{"keera-admins", "keera-operators"}, RoleOperator},
		{"an admin group", "x@example.ch", []string{"keera-admins"}, RoleAdmin},
		{"group names are case-insensitive", "x@example.ch", []string{"KEERA-Admins"}, RoleAdmin},
		{"configured names are trimmed", "x@example.ch", []string{"ai-platform-admins"}, RoleAdmin},
		{"the bootstrap email", "operator@example.ch", nil, RoleOperator},
		{"the email check is case-insensitive", "Operator@Example.ch", nil, RoleOperator},
		{"anybody else", "x@example.ch", []string{"engineering"}, RoleMember},
		{"no groups at all", "x@example.ch", nil, RoleMember},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.RoleFor(tc.email, tc.groups); got != tc.want {
				t.Errorf("RoleFor(%q, %v) = %q, want %q", tc.email, tc.groups, got, tc.want)
			}
		})
	}
}

func TestRoleForIgnoresEmptyConfiguredGroups(t *testing.T) {
	// A blank entry from splitting an empty environment variable must not match
	// every group, which would make everybody an operator.
	m := RoleMapping{OperatorGroups: []string{"", "   "}, Default: RoleMember}
	if got := m.RoleFor("x@example.ch", []string{"", "engineering"}); got != RoleMember {
		t.Errorf("RoleFor = %q, want member", got)
	}
}

func TestRoleForFallsBackToMemberWhenTheDefaultIsNonsense(t *testing.T) {
	m := RoleMapping{Default: Role("wizard")}
	if got := m.RoleFor("x@example.ch", nil); got != RoleMember {
		t.Errorf("RoleFor = %q, want member", got)
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleOperator, RoleAdmin, RoleMember} {
		if !r.Valid() {
			t.Errorf("%q should be valid", r)
		}
	}
	for _, r := range []Role{"", "root", "Admin"} {
		if r.Valid() {
			t.Errorf("%q should not be valid", r)
		}
	}
}
