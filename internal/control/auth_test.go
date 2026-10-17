package control

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
)

// ?next= lets a URL a stranger wrote decide where a browser goes right after
// sign-in, when an off-site redirect is most likely to be trusted. Only a
// same-site path survives.
func TestSafeRedirectKeepsOnlySameSitePaths(t *testing.T) {
	for _, tc := range []struct {
		next string
		want string
	}{
		{"/projects", "/projects"},
		{"/usage?by=project", "/usage?by=project"},
		{"/", "/"},
		{"", ""},
		// The protocol-relative form, and its backslash spelling: Go keeps a
		// backslash as a path character, but browsers turn it into "//".
		{"//evil.example", ""},
		{`/\evil.example`, ""},
		{`/\/evil.example`, ""},
		{`\\evil.example`, ""},
		// An absolute URL, and a scheme-relative one with credentials in it.
		{"https://evil.example/", ""},
		{"http://evil.example/", ""},
		{"//user@evil.example/", ""},
		{"projects", ""},
		{"javascript:alert(1)", ""},
	} {
		if got := safeRedirect(tc.next); got != tc.want {
			t.Errorf("safeRedirect(%q) = %q, want %q", tc.next, got, tc.want)
		}
	}
}

// The error behind a 500 can name tables, constraints and internal hosts, so
// the caller gets the request id, not the reason.
func TestInternalErrorsDoNotLeakTheirCause(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	// What the middleware would have put there.
	w.Header().Set("X-Request-Id", "req_0123456789")

	s.fail(w, errors.New(`ERROR: relation "api_keys" does not exist `+
		`(dial tcp 10.0.0.5:5432, user keera_admin)`))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	body := w.Body.String()
	for _, leak := range []string{"api_keys", "10.0.0.5", "keera_admin", "relation"} {
		if strings.Contains(body, leak) {
			t.Errorf("body = %q, want it not to carry %q", body, leak)
		}
	}
	if !strings.Contains(body, "req_0123456789") {
		t.Errorf("body = %q, want the request id so the log line can be found", body)
	}
}

// Where a group decides the administrator role, every sign-in reapplies it,
// including a demotion for somebody taken out of the group.
func TestRoleAtSignInFollowsTheDirectoryWhenGroupsDecide(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  string
		fromIDP authn.Role
		want    authn.Role
		change  bool
	}{
		{"promotes", "member", authn.RoleAdmin, authn.RoleAdmin, true},
		{"demotes", "admin", authn.RoleMember, authn.RoleMember, true},
		{"leaves a match alone", "admin", authn.RoleAdmin, authn.RoleAdmin, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, change := roleAtSignIn(tc.stored, tc.fromIDP, true)
			if got != tc.want || change != tc.change {
				t.Errorf("roleAtSignIn(%q, %q, true) = %q, %v; want %q, %v",
					tc.stored, tc.fromIDP, got, change, tc.want, tc.change)
			}
		})
	}
}

// Where no group decides it, the administrator role was set in Keera, and the
// next sign-in must not undo it. Google Workspace sends no groups claim, so
// this is every Google deployment.
//
// The operator role is the exception: it comes only from KEERA_OPERATORS or
// an operator group, so sign-in both grants and removes it.
func TestRoleAtSignInKeepsWhatKeeraHoldsWhenNoGroupDecides(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  string
		fromIDP authn.Role
		want    authn.Role
		change  bool
	}{
		{"an assigned administrator survives the default", "admin", authn.RoleMember, authn.RoleAdmin, false},
		{"a pre-created row keeps the role it was added with", "admin", authn.RoleAdmin, authn.RoleAdmin, false},
		{"a member stays a member", "member", authn.RoleMember, authn.RoleMember, false},
		{"KEERA_OPERATORS still promotes", "member", authn.RoleOperator, authn.RoleOperator, true},
		{"and an administrator too", "admin", authn.RoleOperator, authn.RoleOperator, true},
		{"dropping out of it demotes", "operator", authn.RoleMember, authn.RoleMember, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, change := roleAtSignIn(tc.stored, tc.fromIDP, false)
			if got != tc.want || change != tc.change {
				t.Errorf("roleAtSignIn(%q, %q, false) = %q, %v; want %q, %v",
					tc.stored, tc.fromIDP, got, change, tc.want, tc.change)
			}
		})
	}
}
