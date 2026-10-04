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
		name           string
		p              *Principal
		org            string
		read, adminOrg bool
	}{
		{"operator sees any organisation", operator, "org_b", true, true},
		{"admin sees their own", admin, "org_a", true, true},
		{"admin sees no other", admin, "org_b", false, false},
		{"member reads their own", member, "org_a", true, false},
		{"member administers nothing", member, "org_a", true, false},
		{"member sees no other", member, "org_b", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.CanReadOrg(tc.org); got != tc.read {
				t.Errorf("CanReadOrg(%s) = %v, want %v", tc.org, got, tc.read)
			}
			if got := tc.p.CanAdminOrg(tc.org); got != tc.adminOrg {
				t.Errorf("CanAdminOrg(%s) = %v, want %v", tc.org, got, tc.adminOrg)
			}
		})
	}
}

func TestCanManageKeyForIsSelfOnlyForAMember(t *testing.T) {
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
		{"member rotates their own", member, "org_a", "user_1", true},
		{"member cannot rotate a colleague's", member, "org_a", "user_2", false},
		// Attributed to nobody is what an administrator issues for a shared
		// pipeline. It is not the member's.
		{"member cannot rotate an unattributed key", member, "org_a", "", false},
		{"member cannot rotate in another organisation", member, "org_b", "user_1", false},
		{"admin rotates anybody's in their own", admin, "org_a", "user_1", true},
		{"admin rotates nobody's in particular", admin, "org_a", "", true},
		{"admin cannot rotate in another organisation", admin, "org_b", "user_1", false},
		{"the operator key rotates anywhere", operatorKey, "org_a", "user_1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.CanManageKeyFor(tc.org, tc.user); got != tc.want {
				t.Errorf("CanManageKeyFor(%q, %q) = %v, want %v", tc.org, tc.user, got, tc.want)
			}
		})
	}
}

// A member whose session carries no user row has no keys of their own. That
// must not collapse into "the keys attributed to nobody", which are an
// administrator's.
func TestCanManageKeyForNeedsAUser(t *testing.T) {
	orphan := &Principal{Via: MethodSession, Role: RoleMember, OrgID: "org_a"}
	if orphan.CanManageKeyFor("org_a", "") {
		t.Error("a member with no user id was allowed to manage a key attributed to nobody")
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
		{"the bootstrap email beats an admin group", "operator@example.ch", []string{"keera-admins"}, RoleOperator},
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
