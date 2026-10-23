package control

import (
	"errors"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
)

func TestOnlyAnOperatorNamesWhoseSignInBecomesSomeone(t *testing.T) {
	for _, tc := range []struct {
		externalID, role string
		unrestricted     bool
		want             authn.Role
		refused          string
	}{
		{"", "", false, authn.RoleMember, ""},
		{"", "admin", false, authn.RoleAdmin, ""},
		{"", "operator", false, authn.RoleOperator, ""},
		{"", "owner", false, "", "'role'"},
		// An administrator could name someone from another tenant's directory.
		{"google:123", "", false, "", "only an operator"},
		{"google:123", "", true, authn.RoleMember, ""},
		// A bare subject would never match a sign-in.
		{"123", "", true, "", "<provider>:<subject>"},
		{"google:", "", true, "", "<provider>:<subject>"},
	} {
		got, err := newUserRole(tc.externalID, tc.role, tc.unrestricted)
		switch {
		case tc.refused == "" && (err != nil || got != tc.want):
			t.Errorf("%q as %q: %q, %v; want %q", tc.externalID, tc.role, got, err, tc.want)
		case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
			t.Errorf("%q as %q: %v; want it refused for %s", tc.externalID, tc.role, err, tc.refused)
		}
	}
	// Naming a subject is a permission, not a mistake in the request.
	_, err := newUserRole("google:123", "", false)
	if _, ok := errors.AsType[forbidden](err); !ok {
		t.Errorf("an administrator naming a subject = %T, want forbidden", err)
	}
}

func TestAPersonAddedByHandSignsInTheWayTheGatewayCan(t *testing.T) {
	for _, tc := range []struct {
		requested, externalID string
		sso, passkeys         bool
		want, refused         string
	}{
		{"", "", true, false, "sso", ""},
		{"", "", true, true, "sso", ""},
		// With passkeys and no identity provider, that is the way in.
		{"", "", false, true, authn.PasskeyProvider, ""},
		{"", "", false, false, "", "configure one"},
		{"sso", "", false, true, "", "add a passkey account instead"},
		{"passkey", "", true, true, authn.PasskeyProvider, ""},
		{"passkey", "google:123", true, true, "", "no 'external_id'"},
		{"password", "", true, true, "", "'sign_in'"},
	} {
		got, err := newUserSignIn(tc.requested, tc.externalID, tc.sso, tc.passkeys)
		switch {
		case tc.refused == "" && (err != nil || got != tc.want):
			t.Errorf("%+v: %q, %v; want %q", tc, got, err, tc.want)
		case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
			t.Errorf("%+v: %v; want it refused for %s", tc, err, tc.refused)
		}
	}
}
