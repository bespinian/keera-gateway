package control

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/ldaptest"
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
	s := newServer()
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

func TestTheClientAddressCannotBeForged(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	tests := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"no proxy", "203.0.113.5:4000", nil, "203.0.113.5"},
		// Without a proxy in front, the header is the client's own words.
		{
			"a header from an untrusted peer is ignored", "203.0.113.5:4000",
			[]string{"198.51.100.1"},
			"203.0.113.5",
		},
		{"behind a proxy", "10.0.0.2:4000", []string{"203.0.113.5"}, "203.0.113.5"},
		// The proxy appends whom it heard from; what the client wrote is to
		// the left of it.
		{
			"a forged entry is left of the real one", "10.0.0.2:4000",
			[]string{"198.51.100.1, 203.0.113.5"},
			"203.0.113.5",
		},
		{
			"through two proxies", "10.0.0.2:4000",
			[]string{"198.51.100.1, 203.0.113.5, 10.0.0.3"},
			"203.0.113.5",
		},
		{
			"over several header lines", "10.0.0.2:4000",
			[]string{"198.51.100.1", "203.0.113.5"},
			"203.0.113.5",
		},
		{"a proxy without the header", "10.0.0.2:4000", nil, "10.0.0.2"},
		{
			"an entry that is not an address stops the walk", "10.0.0.2:4000",
			[]string{"203.0.113.5, unknown"},
			"10.0.0.2",
		},
		{"only proxies", "10.0.0.2:4000", []string{"10.0.0.4, 10.0.0.3"}, "10.0.0.4"},
		{"an IPv4-mapped peer", "[::ffff:10.0.0.2]:4000", []string{"203.0.113.5"}, "203.0.113.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, trusted); got != tt.want {
				t.Errorf("clientIP = %s, want %s", got, tt.want)
			}
		})
	}
}

// A link anyone can write must not put words on the sign-in screen, which
// sits beside the field for the operator key.
func TestASignInErrorCannotComeFromALink(t *testing.T) {
	corp := ldaptest.New(t, ldaptest.Config("corp"), &ldaptest.Directory{})
	s := New(nil, nil, nil, nil, Options{
		OperatorKey: testOperatorKey,
		Providers:   authn.Providers{corp},
	}, slog.New(slog.DiscardHandler))
	const lure = "call 0800 123 456 and read out your key"
	for _, tc := range []struct {
		name string
		do   func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"provider error", s.callback, "/auth/callback?error=x&error_description=" +
			strings.ReplaceAll(lure, " ", "+")},
		{"provider name", s.login, "/auth/login?provider=" + strings.ReplaceAll(lure, " ", "+")},
	} {
		w := httptest.NewRecorder()
		tc.do(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := w.Header().Get("Location"); got != "/?sign_in_error=1" {
			t.Errorf("%s: sent to %q", tc.name, got)
		}
		var shown string
		for _, c := range w.Result().Cookies() {
			if c.Name == signInErrorCookie {
				shown = c.Value
			}
		}
		if shown == "" || strings.Contains(shown, "0800") {
			t.Errorf("%s: the panel would show %q", tc.name, shown)
		}
	}
}
