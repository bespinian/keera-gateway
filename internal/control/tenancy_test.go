package control

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Who may do what, checked at the handler. authn tests the predicates; these
// test that each handler actually asks.
//
// Every case refuses before the store is reached, so a nil store does not
// panic. A check that runs after the read has already done the read.

// Two people who are not operators, in one organisation. Between them they are
// every caller who has to be kept inside their own tenant.
func member(org string) *authn.Principal {
	return &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: org, UserID: "user_member",
	}
}

func admin(org string) *authn.Principal {
	return &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: org, UserID: "user_admin",
	}
}

func operator() *authn.Principal {
	return &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}
}

// invoke runs one handler with no store behind it. A target like /v1/orgs/org_1
// also sets the id path value, as the router would.
func invoke(h handler, p *authn.Principal, method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, httpx.ControlPrefix+target, nil)
	} else {
		r = httptest.NewRequest(method, httpx.ControlPrefix+target, strings.NewReader(body))
	}
	path, _, _ := strings.Cut(target, "?")
	if parts := strings.Split(path, "/"); len(parts) == 4 {
		r.SetPathValue("id", parts[3])
	}
	h(w, r, p)
	return w
}

// Reading across tenants fails silently: a screen just shows somebody else's
// rows. Every list route must resolve the organisation through one function.
func TestNoListRouteCanBePointedAtAnotherOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	routes := []struct {
		name string
		h    handler
		path string
	}{
		{"keys", s.listKeys, "/v1/keys?org_id=org_other"},
		{"projects", s.listProjects, "/v1/projects?org_id=org_other"},
		{"users", s.listUsers, "/v1/users?org_id=org_other"},
	}
	for _, route := range routes {
		for _, p := range []*authn.Principal{member("org_mine"), admin("org_mine")} {
			t.Run(route.name+"/"+string(p.Role), func(t *testing.T) {
				w := invoke(route.h, p, http.MethodGet, route.path, "")
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", w.Code)
				}
				if !strings.Contains(w.Body.String(), "its own organisation") {
					t.Errorf("body = %q, want it to say why", w.Body.String())
				}
			})
		}
	}
}

// Naming your own organisation is not refused. Without this, the test above
// would pass on a handler that refused every org_id.
func TestNamingYourOwnOrganisationIsNotRefused(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	p := admin("org_mine")

	got, ok := s.scopeOrg(w, p, "org_mine")
	if !ok {
		t.Fatalf("an administrator was refused their own organisation: %s", w.Body)
	}
	if got != "org_mine" {
		t.Errorf("scoped to %q, want org_mine", got)
	}

	// Leaving it off gets your own, not everybody's.
	got, ok = s.scopeOrg(httptest.NewRecorder(), p, "")
	if !ok || got != "org_mine" {
		t.Errorf("scopeOrg(\"\") = %q, %v; want the caller's own organisation", got, ok)
	}
}

// An organisation's email domain decides which tenant a sign-in lands in, so
// an administrator setting it could take somebody else's people. Only an
// operator may.
func TestOnlyAnOperatorCanChangeAnOrganisationsIdentityMapping(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, p := range []*authn.Principal{member("org_1"), admin("org_1")} {
		w := invoke(s.updateOrg, p, http.MethodPatch, "/v1/orgs/org_1", `{"email_domain":"example.ch"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d for a %s, want 403", w.Code, p.Role)
		}
		if !strings.Contains(w.Body.String(), "only an operator") {
			t.Errorf("body = %q, want it to say who may", w.Body.String())
		}
	}
}

// Creating a tenant is not something a tenant does.
func TestOnlyAnOperatorCanCreateAnOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	for _, p := range []*authn.Principal{member("org_1"), admin("org_1")} {
		w := invoke(s.createOrg, p, http.MethodPost, "/v1/orgs", `{"name":"Theirs"}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d for a %s, want 403", w.Code, p.Role)
		}
	}
}

// A project is a set of guardrails: whoever can make one can make a budget and a
// rate limit. A member of the organisation is not that person.
func TestCreatingAProjectNeedsAnAdministratorOfThatOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	t.Run("a member of the right organisation", func(t *testing.T) {
		w := invoke(s.createProject, member("org_1"), http.MethodPost, "/v1/projects",
			`{"org_id":"org_1","name":"Platform"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "administrator") {
			t.Errorf("body = %q, want it to name the role that may", w.Body.String())
		}
	})

	// An administrator of another one is refused earlier, by the scope check,
	// so the answer does not depend on being an administrator somewhere.
	t.Run("an administrator of another organisation", func(t *testing.T) {
		w := invoke(s.createProject, admin("org_mine"), http.MethodPost, "/v1/projects",
			`{"org_id":"org_other","name":"Theirs"}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
}

// Changing and deleting a project name only the project, so its organisation
// comes from the row. A caller who administers no organisation is refused
// before the read (the store is nil).
func TestChangingAProjectNeedsAnAdministrator(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, tc := range []struct {
		name   string
		h      handler
		method string
		body   string
	}{
		{"update", s.updateProject, http.MethodPatch, `{"name":"Payments"}`},
		{"delete", s.deleteProject, http.MethodDelete, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := invoke(tc.h, member("org_1"), tc.method, "/v1/projects/project_1", tc.body)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", w.Code)
			}
			if !strings.Contains(w.Body.String(), "administrator") {
				t.Errorf("body = %q, want it to name the role that may", w.Body.String())
			}
		})
	}
}

// An empty name is a mistake, not a request to clear it: nobody could tell the
// project apart. A change with nothing in it is a mistake too.
func TestChangingAProjectRequiresAName(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`, `{}`} {
		w := invoke(s.updateProject, admin("org_1"), http.MethodPatch, "/v1/projects/project_1", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400", w.Code, body)
		}
	}
}

// The operator key is not attached to any organisation, so it has to name one.
// Defaulting to something would be picking a tenant for it.
func TestAnUnscopedCallerMustNameTheOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := invoke(s.createProject, operator(), http.MethodPost, "/v1/projects", `{"name":"Platform"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "org_id") {
		t.Errorf("body = %q, want it to name the missing field", w.Body.String())
	}
}

// A session with no organisation (its tenant was deleted) must read nothing,
// not everything. An empty org id does not make an account an operator.
func TestAnAccountAttachedToNoOrganisationReadsNothing(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	orphan := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, UserID: "user_1",
	}

	for _, h := range []handler{s.listKeys, s.listProjects, s.listUsers} {
		w := invoke(h, orphan, http.MethodGet, "/v1/keys", "")
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "not attached to an organisation") {
			t.Errorf("body = %q, want it to say what is wrong with the account",
				w.Body.String())
		}
	}
}

// Keys are always one organisation's. An operator who names none is asked for
// one, not given an empty list.
func TestAnOperatorListingKeysNamesAnOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := invoke(s.listKeys, operator(), http.MethodGet, "/v1/keys", "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "org_required") {
		t.Errorf("status = %d, body = %q, want 400 org_required", w.Code, w.Body.String())
	}
}

// The panel sends what was typed. A URL saved as a domain would match no
// sign-in, so the control plane refuses it rather than trusting the client.
func TestAnOrganisationsDomainIsCleanedByTheControlPlane(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := invoke(s.createOrg, operator(), http.MethodPost, "/v1/orgs",
		`{"name":"Bank","email_domain":"https://example.ch/"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "example.ch") {
		t.Errorf("status = %d, body = %q, want 400 naming the domain meant", w.Code, w.Body.String())
	}
}

// Deleting a tenant takes its people and keys with it, so its own
// administrator may not.
func TestDeleteOrgIsOperatorOnly(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, p := range []*authn.Principal{admin("org_1"), member("org_1")} {
		w := invoke(s.deleteOrg, p, http.MethodDelete, "/v1/orgs/org_2", "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d for a %s, want 403", w.Code, p.Role)
		}
		if !strings.Contains(w.Body.String(), "only an operator") {
			t.Errorf("body = %q, want it to say who may delete one", w.Body.String())
		}
	}
}

// An operator signed in through the panel has a user row in some
// organisation. Deleting it would sign them out mid-request and could remove
// the last operator, so it is refused before the store (which is nil).
func TestOperatorCannotDeleteTheirOwnOrg(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	signedIn := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleOperator, OrgID: "org_1"}

	w := invoke(s.deleteOrg, signedIn, http.MethodDelete, "/v1/orgs/org_1", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "signed in to") {
		t.Errorf("body = %q, want it to explain the way out", w.Body.String())
	}
}

// Two organisations cannot share a name or a domain. That is an ordinary
// mistake, so it gets a conflict that says what to change, not an internal
// error.
func TestATakenNameOrDomainIsAConflict(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	for _, c := range []struct {
		err  error
		code string
	}{
		{store.ErrOrgNameTaken, "org_name_taken"},
		{store.ErrDomainTaken, "domain_taken"},
	} {
		w := httptest.NewRecorder()
		s.failOrg(w, fmt.Errorf("setting it: %w", c.err))

		if w.Code != http.StatusConflict {
			t.Fatalf("%s: status = %d, want 409", c.code, w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, c.code) ||
			!strings.Contains(body, "another organisation") {
			t.Errorf("body = %q, want %s and what holds it", body, c.code)
		}
	}
}

// Only an administrator issues keys, so every key has the project and
// guardrails one chose. A member's own key would step around their project's.
// They rotate the key they were given instead. The store is nil: the refusal
// comes first.
func TestMemberCannotIssueAKey(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, body := range []string{
		`{"org_id":"org_1","name":"mine"}`,
		`{"org_id":"org_1","user_id":"user_member","name":"mine"}`,
		`{"org_id":"org_1","user_id":"user_member","kind":"subscription"}`,
	} {
		w := invoke(s.createKey, member("org_1"), http.MethodPost, "/v1/keys", body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", body, w.Code)
		}
		if !strings.Contains(w.Body.String(), "only an administrator") {
			t.Errorf("body = %q, want it to say who may issue one", w.Body.String())
		}
	}
}

// Somebody signed in to one organisation must not issue a key in another, and
// the refusal must not depend on the person they attributed it to being theirs.
func TestKeysCannotBeIssuedIntoAnotherOrganisation(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, p := range []*authn.Principal{member("org_1"), admin("org_1")} {
		w := invoke(s.createKey, p, http.MethodPost, "/v1/keys", `{"org_id":"org_2","user_id":"user_member"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d for a %s, want 403", w.Code, p.Role)
		}
	}
}

// The operator role crosses organisations, so it comes only from
// KEERA_OPERATORS or an operator group, which a customer does not control. No
// caller grants it through the API, an operator included.
func TestOperatorRoleCannotBeGrantedThroughTheAPI(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	t.Run("upsert", func(t *testing.T) {
		w := invoke(s.addUser, operator(), http.MethodPost, "/v1/users",
			`{"org_id":"org_1","email":"a@example.ch","role":"operator"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "KEERA_OPERATORS") {
			t.Errorf("body = %q, want it to name what does grant it", w.Body.String())
		}
	})
	// The store is nil: the refusal comes before the row is read, because it
	// does not depend on who is being changed.
	t.Run("set role", func(t *testing.T) {
		w := invoke(s.updateUser, operator(), http.MethodPatch, "/v1/users/user_1", `{"role":"operator"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "KEERA_OPERATORS") {
			t.Errorf("body = %q, want it to name what does grant it", w.Body.String())
		}
	})
}

// A subject decides whose sign-in becomes a person. An administrator could
// name somebody from another tenant's directory and pull them in, so only an
// operator may set it. The store is nil: the refusal comes first.
func TestOnlyAnOperatorLinksAPersonToASubject(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := invoke(s.addUser, admin("org_1"), http.MethodPost, "/v1/users",
		`{"org_id":"org_1","email":"a@example.ch","external_id":"entra:victim"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "first sign-in") {
		t.Errorf("body = %q, want it to say how the link is made instead", w.Body.String())
	}
}

// On a gateway with no identity provider, an account that signs in through one
// could never be used, so adding one is refused rather than left to wait.
func TestNoIdentityProviderAccountWithoutAnIdentityProvider(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	for _, signIn := range []string{"", "sso"} {
		w := invoke(s.addUser, admin("org_1"), http.MethodPost, "/v1/users",
			`{"org_id":"org_1","email":"a@example.ch","sign_in":"`+signIn+`"}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "passkey") {
			t.Errorf("sign_in %q: status = %d %s; want 400 pointing at passkeys", signIn, w.Code, w.Body)
		}
	}
}

// Without Claude subscriptions, an administrator cannot issue a key for them.
func TestNoSubscriptionKeyWhileSubscriptionsAreOff(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	w := invoke(s.createKey, admin("org_1"), http.MethodPost, "/v1/keys",
		`{"org_id":"org_1","user_id":"user_1","kind":"subscription"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "KEERA_CLAUDE_SUBSCRIPTIONS") {
		t.Errorf("status = %d %s; want 400 naming the setting", w.Code, w.Body)
	}
}

// A key lasts for a lifetime from now or until a moment, never both, and a
// moment already past would issue a key that is dead on arrival.
func TestKeyExpiry(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	past := time.Now().Add(-time.Hour)
	month, zero, negative := int64(30*24*3600), int64(0), int64(-3600)
	for _, tc := range []struct {
		in     *int64
		at     *time.Time
		ok     bool
		expiry bool
	}{
		{nil, nil, true, false},
		{&month, nil, true, true},
		{nil, &future, true, true},
		{&zero, nil, false, false},
		{&negative, nil, false, false},
		{&month, &future, false, false},
		{nil, &past, false, false},
	} {
		w := httptest.NewRecorder()
		got, ok := keyExpiry(w, tc.in, tc.at)
		if ok != tc.ok || (got != nil) != tc.expiry {
			t.Errorf("keyExpiry(%v, %v) = %v, %v; want ok %v, expiry %v",
				tc.in, tc.at, got, ok, tc.ok, tc.expiry)
		}
		if !ok && w.Code != http.StatusBadRequest {
			t.Errorf("keyExpiry(%v, %v) answered %d, want 400", tc.in, tc.at, w.Code)
		}
	}
	if got, _ := keyExpiry(httptest.NewRecorder(), nil, &future); !got.Equal(future) {
		t.Errorf("expires_at = %v, want %v", got, future)
	}
}
