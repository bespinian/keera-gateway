package control

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
)

// Who may do what, checked at the handler rather than at the predicate.
//
// The predicates in authn are covered exhaustively by that package's own tests,
// so what is left - and what these are about - is whether each handler actually
// asks. A handler that forgot to call CanAdminOrg is not a failing unit test
// anywhere: it is a passing one in authn, a green build, and an administrator of
// one customer reading another customer's projects.
//
// Every case here refuses before the store is reached, which is why a nil store
// does not panic. That is itself worth pinning: an authorization check that runs
// after the read has already done the read.

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

// invoke runs one handler with no store behind it.
func invoke(h handler, p *authn.Principal, method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, httpx.ControlPrefix+target, nil)
	} else {
		r = httptest.NewRequest(method, httpx.ControlPrefix+target, strings.NewReader(body))
	}
	h(w, r, p)
	return w
}

// Reading across tenants is the failure that does not announce itself: nothing
// errors, a screen simply has somebody else's rows on it. Every list route
// resolves the organisation through one function, and this is what says that
// none of them skipped it.
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

// Naming your own organisation is not the same request as naming somebody
// else's, and only one of them is refused. Without this the test above would
// pass on a handler that refused every org_id it was given.
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

	// And leaving it off gets you your own, rather than everybody's.
	got, ok = s.scopeOrg(httptest.NewRecorder(), p, "")
	if !ok || got != "org_mine" {
		t.Errorf("scopeOrg(\"\") = %q, %v; want the caller's own organisation", got, ok)
	}
}

// An organisation's email domain decides which tenant a sign-in lands in, so
// setting one is how a customer would be handed somebody else's people. It is
// operator-only for that reason, and an organisation's own administrator is
// exactly the caller who must not reach it.
func TestOnlyAnOperatorCanChangeAnOrganisationsIdentityMapping(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))

	for _, p := range []*authn.Principal{member("org_1"), admin("org_1")} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, httpx.ControlPrefix+"/v1/orgs/org_1",
			strings.NewReader(`{"email_domain":"example.ch"}`))
		r.SetPathValue("id", "org_1")

		s.updateOrg(w, r, p)

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

	// An administrator of the wrong one is refused earlier, by the scope check,
	// and the distinction matters: the answer must not depend on the caller
	// happening to be an administrator somewhere.
	t.Run("an administrator of another organisation", func(t *testing.T) {
		w := invoke(s.createProject, admin("org_mine"), http.MethodPost, "/v1/projects",
			`{"org_id":"org_other","name":"Theirs"}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
}

// Changing and deleting a project name the project in the path and nothing else, so
// the organisation they belong to comes from the row rather than from the
// request. A caller who administers no organisation is still refused without
// the read - a nil store here is what says so.
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

// An empty name is a mistake, not a request to clear it - a project with no
// name is a row nobody can identify on any screen. A change with nothing in
// it is a mistake too.
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

// A session that belongs to no organisation - which is what a person whose
// tenant was deleted underneath them has - must read nothing rather than
// everything. The check is "is this caller unrestricted", and an account with an
// empty org id is not the same thing as an operator even though both have no
// organisation of their own.
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
