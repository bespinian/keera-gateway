package control

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Who may do what, checked at the handler. authn tests the predicates; these
// test that each handler actually asks.
//
// Every case refuses before the store is reached, so a nil store does not
// panic: a check that ran after the read would.

// Two people who are not operators, in one organisation: every caller who has
// to be kept inside their own tenant.
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

// routePatterns are the server's routes, read once.
var routePatterns = sync.OnceValue(func() []string {
	s := newServer()
	s.Handler()
	return s.routes
})

// invoke runs one handler as p, with no store behind it.
func invoke(h handler, p *authn.Principal, method, target, body string) *httptest.ResponseRecorder {
	return serve(context.Background(), h, p, method, target, body)
}

// serve runs one handler as p, routed by the server's own patterns so it gets
// the path values the router would give it. Authentication and the CSRF check
// do not run, so what is under test is the handler's own authorization.
func serve(ctx context.Context, h handler, p *authn.Principal,
	method, target, body string,
) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequestWithContext(ctx, method, httpx.ControlPrefix+target, rd)
	mux := http.NewServeMux()
	for _, pattern := range routePatterns() {
		m, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(m+" "+httpx.ControlPrefix+path, func(w http.ResponseWriter, r *http.Request) {
			h(w, r, p)
		})
	}
	if _, pattern := mux.Handler(r); pattern == "" {
		panic("no route for " + method + " " + target)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// Each handler refuses what its caller may not do, before the store is
// reached. Every case is one request, and the status and words it gets.
func TestHandlersRefuseWhatTheCallerMayNotDo(t *testing.T) {
	s := newServer()
	// A session whose organisation was deleted. An empty org id does not make
	// an account an operator.
	orphan := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin, UserID: "user_1"}
	// A member with no person behind it, so no keys of their own.
	nobody := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_1"}
	// An operator signed in through the panel, with a user row in org_1.
	signedIn := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleOperator, OrgID: "org_1"}
	const (
		get, post, patch, del = http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete
		forbidden, bad        = http.StatusForbidden, http.StatusBadRequest
	)

	for _, tc := range []struct {
		name                 string
		h                    handler
		p                    *authn.Principal
		method, target, body string
		status               int
		want                 string
	}{
		// An email domain decides which tenant a sign-in lands in, so setting
		// it could take somebody else's people.
		{"a member sets the domain", s.updateOrg, member("org_1"), patch, "/v1/orgs/org_1",
			`{"email_domain":"example.ch"}`, forbidden, "only an operator"},
		{"an administrator sets the domain", s.updateOrg, admin("org_1"), patch, "/v1/orgs/org_1",
			`{"email_domain":"example.ch"}`, forbidden, "only an operator"},
		// Creating a tenant is not something a tenant does.
		{"a member creates an organisation", s.createOrg, member("org_1"), post, "/v1/orgs",
			`{"name":"Theirs"}`, forbidden, ""},
		{"an administrator creates an organisation", s.createOrg, admin("org_1"), post, "/v1/orgs",
			`{"name":"Theirs"}`, forbidden, ""},
		// Deleting a tenant takes its people and keys with it.
		{"an administrator deletes an organisation", s.deleteOrg, admin("org_1"), del, "/v1/orgs/org_2",
			"", forbidden, "only an operator"},
		{"a member deletes an organisation", s.deleteOrg, member("org_1"), del, "/v1/orgs/org_2",
			"", forbidden, "only an operator"},
		// It would sign them out mid-request and could remove the last operator.
		{"an operator deletes their own organisation", s.deleteOrg, signedIn, del, "/v1/orgs/org_1",
			"", forbidden, "signed in to"},
		// The panel sends what was typed. A URL saved as a domain would match no
		// sign-in, so the control plane cleans it rather than trusting the client.
		{"a URL as a domain", s.createOrg, operator(), post, "/v1/orgs",
			`{"name":"Bank","email_domain":"https://example.ch/"}`, bad, "example.ch"},
		// The operator key belongs to no organisation, so it has to name one.
		// Defaulting would pick a tenant for it.
		{"an operator creates a project in no organisation", s.createProject, operator(), post,
			"/v1/projects", `{"name":"Platform"}`, bad, "org_id"},
		{"an operator lists keys of no organisation", s.listKeys, operator(), get, "/v1/keys",
			"", bad, "org_required"},
		{"an orphan lists keys", s.listKeys, orphan, get, "/v1/keys",
			"", forbidden, "not attached to an organisation"},
		{"an orphan lists projects", s.listProjects, orphan, get, "/v1/projects",
			"", forbidden, "not attached to an organisation"},
		{"an orphan lists people", s.listUsers, orphan, get, "/v1/users",
			"", forbidden, "not attached to an organisation"},

		// Only an administrator issues keys, so every key has the project and
		// guardrails one chose. A member rotates the key they were given instead.
		{"a member issues a key", s.createKey, member("org_1"), post, "/v1/keys",
			`{"org_id":"org_1","name":"mine"}`, forbidden, "only an administrator"},
		{"a member issues a key for themselves", s.createKey, member("org_1"), post, "/v1/keys",
			`{"org_id":"org_1","user_id":"user_member","name":"mine"}`, forbidden, "only an administrator"},
		{"a member issues a subscription key", s.createKey, member("org_1"), post, "/v1/keys",
			`{"org_id":"org_1","user_id":"user_member","kind":"subscription"}`, forbidden, "only an administrator"},
		// The refusal does not depend on whom the key is for.
		{"a member issues a key elsewhere", s.createKey, member("org_1"), post, "/v1/keys",
			`{"org_id":"org_2","user_id":"user_member"}`, forbidden, ""},
		{"an administrator issues a key elsewhere", s.createKey, admin("org_1"), post, "/v1/keys",
			`{"org_id":"org_2","user_id":"user_member"}`, forbidden, ""},
		{"a subscription key while they are off", s.createKey, admin("org_1"), post, "/v1/keys",
			`{"org_id":"org_1","user_id":"user_1","kind":"subscription"}`,
			http.StatusNotImplemented, "KEERA_CLAUDE_SUBSCRIPTIONS"},

		// A project is a set of guardrails: whoever makes one can make a budget.
		{"a member creates a project", s.createProject, member("org_1"), post, "/v1/projects",
			`{"org_id":"org_1","name":"Platform"}`, forbidden, "administrator"},
		// Refused by the scope check, so the answer does not depend on being an
		// administrator somewhere.
		{"an administrator creates a project elsewhere", s.createProject, admin("org_mine"), post,
			"/v1/projects", `{"org_id":"org_other","name":"Theirs"}`, forbidden, ""},
		// A project's organisation comes from its row, so a caller who
		// administers none is refused before the read.
		{"a member renames a project", s.updateProject, member("org_1"), patch, "/v1/projects/project_1",
			`{"name":"Payments"}`, forbidden, "administrator"},
		{"a member deletes a project", s.deleteProject, member("org_1"), del, "/v1/projects/project_1",
			"", forbidden, "administrator"},
		// Nobody could tell a project without a name apart. A change with
		// nothing in it is a mistake too.
		{"an empty project name", s.updateProject, admin("org_1"), patch, "/v1/projects/project_1",
			`{"name":""}`, bad, ""},
		{"a blank project name", s.updateProject, admin("org_1"), patch, "/v1/projects/project_1",
			`{"name":"   "}`, bad, ""},
		{"a project change of nothing", s.updateProject, admin("org_1"), patch, "/v1/projects/project_1",
			`{}`, bad, ""},

		// The operator role crosses organisations, so it comes only from
		// KEERA_OPERATORS or an operator group. Nobody grants it through the API.
		{"adding an operator", s.addUser, operator(), post, "/v1/users",
			`{"org_id":"org_1","email":"a@example.ch","role":"operator"}`, forbidden, "KEERA_OPERATORS"},
		// Refused before the row is read: it does not depend on who is changed.
		{"making someone an operator", s.updateUser, operator(), patch, "/v1/users/user_1",
			`{"role":"operator"}`, forbidden, "KEERA_OPERATORS"},
		// An administrator could name somebody from another tenant's directory.
		{"an administrator links a subject", s.addUser, admin("org_1"), post, "/v1/users",
			`{"org_id":"org_1","email":"a@example.ch","external_id":"entra:victim"}`, forbidden, "first sign-in"},
		// Without an identity provider such an account could never sign in.
		{"a default account without an identity provider", s.addUser, admin("org_1"), post, "/v1/users",
			`{"org_id":"org_1","email":"a@example.ch","sign_in":""}`, bad, "passkey"},
		{"an SSO account without an identity provider", s.addUser, admin("org_1"), post, "/v1/users",
			`{"org_id":"org_1","email":"a@example.ch","sign_in":"sso"}`, bad, "passkey"},

		// A check reaches a backend on the model's own credential, so it is
		// for whoever runs the model. A member runs none.
		{"a member checks a model", s.checkModel, member("org_1"), post,
			"/v1/models/keera-code/check?org_id=org_1", "", forbidden, "only an administrator"},
		// A fallback grouping would answer another question, with the caller's
		// string in the CSV filename.
		{"an unknown grouping", s.usage, member("org_1"), get,
			`/v1/usage?format=csv&group_by=x.bat%22%3Bx%3D%22`, "", bad, "group_by"},
		// A member reads only their own keys' requests. Naming someone else is
		// refused, not quietly answered with their own.
		{"a member reads someone's requests", s.requests, member("org_1"), get,
			"/v1/requests?user_id=user_other", "", forbidden, "their own keys"},
		{"a member streams someone's requests", s.requestStream, member("org_1"), get,
			"/v1/requests/stream?user_id=user_other", "", forbidden, "their own keys"},
		{"a member reads someone's sessions", s.sessions, member("org_1"), get,
			"/v1/sessions?user_id=user_other", "", forbidden, "their own keys"},
		{"nobody reads the request log", s.requests, nobody, get, "/v1/requests", "", forbidden, ""},
		{"nobody streams the request log", s.requestStream, nobody, get, "/v1/requests/stream", "", forbidden, ""},
		{"nobody reads sessions", s.sessions, nobody, get, "/v1/sessions", "", forbidden, ""},
		{"nobody reads a session", s.session, nobody, get, "/v1/sessions/1", "", forbidden, ""},
		{"nobody reads the map", s.trafficMap, nobody, get, "/v1/map", "", forbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := invoke(tc.h, tc.p, tc.method, tc.target, tc.body)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("%d %s, want %d saying %q", w.Code, w.Body, tc.status, tc.want)
			}
		})
	}
}

// Reading across tenants fails silently: a screen just shows somebody else's
// rows. Every list route must resolve the organisation through one function.
func TestNoListRouteCanBePointedAtAnotherOrganisation(t *testing.T) {
	s := newServer()

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
	s := newServer()
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

// Two organisations cannot share a name or a domain. That is an ordinary
// mistake, so it gets a conflict saying what to change, not an internal error.
func TestATakenNameOrDomainIsAConflict(t *testing.T) {
	s := newServer()
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
