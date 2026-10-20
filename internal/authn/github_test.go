package authn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// fakeGitHub answers as github.com and api.github.com both do, on one server.
type fakeGitHub struct {
	*httptest.Server
	user     map[string]any
	emails   []githubEmail
	orgs     []string
	teams    [][2]string // org, team
	status   int         // for every API call; 0 is 200
	verifier string      // the code_verifier the exchange sent
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		user:   map[string]any{"id": 42, "login": "alice", "name": "Alice Example"},
		emails: []githubEmail{{Email: "Alice@Example.ch", Primary: true, Verified: true}},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/login/oauth/access_token" {
		_ = r.ParseForm()
		f.verifier = r.PostForm.Get("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_token", "token_type": "bearer"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer gho_token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	var out any
	switch r.URL.Path {
	case "/user":
		out = f.user
	case "/user/emails":
		out = f.emails
	case "/user/orgs":
		var orgs []map[string]any
		for _, o := range f.orgs {
			orgs = append(orgs, map[string]any{"login": o})
		}
		out = orgs
	case "/user/teams":
		var teams []map[string]any
		for _, t := range f.teams {
			teams = append(teams, map[string]any{
				"slug": t[1], "organization": map[string]any{"login": t[0]}})
		}
		out = teams
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeGitHub) connect(cfg OIDCConfig) *OIDC {
	cfg.Name = "github"
	cfg.IssuerURL = GitHubIssuer
	cfg.ClientID = "keera"
	cfg.ClientSecret = "secret"
	cfg.RedirectURL = "https://keera.example.ch/control/auth/callback"
	return newGitHub(cfg, f.Client(), f.URL, f.URL)
}

func TestGitHubNeedsNoDiscovery(t *testing.T) {
	// github.com publishes no discovery document, so asking for one would
	// stop the start.
	o, err := NewOIDC(context.Background(), OIDCConfig{
		Name: "github", IssuerURL: "https://github.com/", ClientID: "keera",
		ClientSecret: "secret", RedirectURL: "https://keera.example.ch/control/auth/callback",
	}, &http.Client{Transport: refuseAll{}})
	if err != nil {
		t.Fatal(err)
	}
	if !o.IsGitHub() {
		t.Fatal("a github.com issuer did not make a GitHub provider")
	}
	if got := o.LogoutURL("https://keera.example.ch"); got != "" {
		t.Errorf("LogoutURL = %q, want none: GitHub has no end-session endpoint", got)
	}

	flow := NewFlow()
	u, err := url.Parse(o.AuthCodeURL(flow))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Errorf("sign-in goes to %s%s, want github.com/login/oauth/authorize", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("code_challenge") != s256(flow.Verifier) || q.Get("code_challenge_method") != "S256" {
		t.Errorf("no PKCE challenge in %s", u)
	}
	if q.Get("scope") != "user:email" {
		t.Errorf("scope = %q, want only user:email", q.Get("scope"))
	}
	if q.Has("nonce") {
		t.Error("a nonce was sent, but GitHub returns no id_token to check it in")
	}
}

type refuseAll struct{}

func (refuseAll) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("no request expected, got one to " + r.URL.String())
}

func TestGitHubSignIn(t *testing.T) {
	f := newFakeGitHub(t)
	o := f.connect(OIDCConfig{Mapping: RoleMapping{Default: RoleMember}})
	flow := NewFlow()
	id, err := o.Exchange(context.Background(), "the-code", flow)
	if err != nil {
		t.Fatal(err)
	}
	if f.verifier != flow.Verifier {
		t.Error("the exchange did not send the PKCE verifier")
	}
	want := Identity{Provider: "github", Subject: "42", Email: "alice@example.ch",
		Name: "Alice Example", RefreshToken: "gho_token"}
	if id.Provider != want.Provider || id.Subject != want.Subject || id.Email != want.Email ||
		id.Name != want.Name || id.RefreshToken != want.RefreshToken {
		t.Errorf("identity = %+v, want %+v", id, want)
	}

	// Without a name, the login stands in.
	f.user["name"] = ""
	if id, err = o.Exchange(context.Background(), "the-code", NewFlow()); err != nil || id.Name != "alice" {
		t.Errorf("name = %q (%v), want the login", id.Name, err)
	}
}

func TestGitHubPicksAVerifiedAddress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains []string
		emails  []githubEmail
		want    string // empty: refused
	}{
		{"primary", nil, []githubEmail{
			{Email: "alice@other.ch", Verified: true},
			{Email: "alice@example.ch", Primary: true, Verified: true},
		}, "alice@example.ch"},
		{"a verified work address beside a private primary", []string{"example.ch"}, []githubEmail{
			{Email: "alice@private.ch", Primary: true, Verified: true},
			{Email: "alice@example.ch", Verified: true},
		}, "alice@example.ch"},
		{"an unverified address is never used", []string{"example.ch"}, []githubEmail{
			{Email: "alice@private.ch", Primary: true, Verified: true},
			{Email: "alice@example.ch"},
		}, ""},
		{"the noreply address names nobody", nil, []githubEmail{
			{Email: "42+alice@users.noreply.github.com", Primary: true, Verified: true},
		}, ""},
		{"no verified address", nil, []githubEmail{
			{Email: "alice@example.ch", Primary: true},
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.emails = tc.emails
			o := f.connect(OIDCConfig{Mapping: RoleMapping{Default: RoleMember}, Domains: tc.domains})
			id, err := o.Exchange(context.Background(), "the-code", NewFlow())
			switch {
			case tc.want == "" && err == nil:
				t.Errorf("signed in as %s, want refused", id.Email)
			case tc.want != "" && (err != nil || id.Email != tc.want):
				t.Errorf("email = %q (%v), want %s", id.Email, err, tc.want)
			}
		})
	}
}

func TestGitHubGroupsAreOrgsAndTeams(t *testing.T) {
	f := newFakeGitHub(t)
	f.orgs = []string{"bespinian"}
	f.teams = [][2]string{{"bespinian", "keera-admins"}}

	o := f.connect(OIDCConfig{Mapping: RoleMapping{Default: RoleMember}})
	id, err := o.Exchange(context.Background(), "the-code", NewFlow())
	if err != nil {
		t.Fatal(err)
	}
	if id.Groups != nil {
		t.Errorf("groups = %v without a group mapping, want none asked for", id.Groups)
	}

	o = f.connect(OIDCConfig{Mapping: RoleMapping{
		Default: RoleMember, AdminGroups: []string{"bespinian/keera-admins"}}})
	if !slices.Contains(o.oauth.Scopes, "read:org") {
		t.Errorf("scopes = %v, want read:org where groups decide a role", o.oauth.Scopes)
	}
	id, err = o.Exchange(context.Background(), "the-code", NewFlow())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(id.Groups, ","); got != "bespinian,bespinian/keera-admins" {
		t.Errorf("groups = %s", got)
	}
	if role := o.Mapping().RoleFor(id.Email, id.Groups); role != RoleAdmin {
		t.Errorf("role = %s, want admin from the team", role)
	}
}

func TestGitHubRecheck(t *testing.T) {
	f := newFakeGitHub(t)
	o := f.connect(OIDCConfig{Mapping: RoleMapping{Default: RoleMember}})

	id, fresh, err := o.Recheck(context.Background(), "gho_token")
	if err != nil || !fresh || id.Subject != "42" || id.RefreshToken != "gho_token" {
		t.Errorf("Recheck = %+v, %v, %v; want a fresh identity keeping the token", id, fresh, err)
	}

	// A revoked token is an answer about the person.
	if _, _, err := o.Recheck(context.Background(), "gho_revoked"); !errors.Is(err, ErrDirectoryRefused) {
		t.Errorf("revoked token: err = %v, want ErrDirectoryRefused", err)
	}

	// The rate limit is not.
	f.status = http.StatusForbidden
	if _, _, err := o.Recheck(context.Background(), "gho_token"); err == nil || errors.Is(err, ErrDirectoryRefused) {
		t.Errorf("rate limited: err = %v, want an error that keeps the sign-in", err)
	}

	// An account that lost its last verified address can no longer be placed.
	f.status = 0
	f.emails = nil
	if _, _, err := o.Recheck(context.Background(), "gho_token"); !errors.Is(err, ErrDirectoryRefused) {
		t.Errorf("no address left: err = %v, want ErrDirectoryRefused", err)
	}
}
