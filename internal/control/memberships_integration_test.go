package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/oidctest"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// One person in several organisations: a row and a role in each, one
// identity, and a session that moves between them.

type orgsSetup struct {
	ts   *httptest.Server
	st   *store.Store
	idps map[string]*oidctest.IDP
}

// membershipServer has two organisations: Bank owns example.ch, Insurer has
// no domain. Two providers vouch for example.ch, "test" and "other". With
// adminGroups, the directory decides who is an administrator.
func membershipServer(t *testing.T, adminGroups bool) orgsSetup {
	t.Helper()
	st, ctx := testStore(t, "users", "sessions", "login_flows", "cli_codes", "cli_tokens")
	for _, o := range []store.Org{
		{ID: "org_a", Name: "Bank", EmailDomain: "example.ch"},
		{ID: "org_b", Name: "Insurer"},
	} {
		if _, err := st.CreateOrg(ctx, o, store.OrgTemplate{}); err != nil {
			t.Fatalf("creating %s: %v", o.Name, err)
		}
	}
	mapping := authn.RoleMapping{Default: authn.RoleMember,
		OperatorEmails: []string{"op@example.ch"}}
	if adminGroups {
		mapping.AdminGroups = []string{"keera-admins"}
	}
	s := orgsSetup{st: st, idps: map[string]*oidctest.IDP{}}
	var providers authn.Providers
	for _, name := range []string{"test", "other"} {
		idp := oidctest.New(t)
		p, err := authn.NewOIDC(ctx, authn.OIDCConfig{
			Name: name, IssuerURL: idp.URL, ClientID: "keera", ClientSecret: "secret",
			RedirectURL: "https://keera.example.ch/control/auth/callback",
			Mapping:     mapping, Domains: []string{"example.ch"},
		}, idp.Client())
		if err != nil {
			t.Fatalf("NewOIDC: %v", err)
		}
		s.idps[name] = idp
		providers = append(providers, p)
	}
	box, err := secret.New("a-secret-key-long-enough-to-be-real")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF", Secrets: box, Providers: providers,
	}, slog.New(slog.DiscardHandler))
	s.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(s.ts.Close)
	return s
}

// signIn goes through provider as subject and email, in groups, sending the
// cookies given along. It returns where the callback sent the browser and the
// session cookie, which is nil when the sign-in was refused.
func (s orgsSetup) signIn(t *testing.T, provider, subject, email string, groups []string,
	cookies ...*http.Cookie) (string, *http.Cookie) {
	t.Helper()
	resp, err := noRedirects.Get(s.ts.URL + httpx.ControlPrefix + "/auth/login?provider=" + provider)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	authorize, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	s.idps[provider].Claims = func(m map[string]any) {
		m["sub"], m["email"], m["nonce"] = subject, email, q.Get("nonce")
		m["groups"] = groups
	}
	req, _ := http.NewRequest(http.MethodGet, s.ts.URL+httpx.ControlPrefix+
		"/auth/callback?code=c&state="+url.QueryEscape(q.Get("state")), nil)
	for _, c := range append(resp.Cookies(), cookies...) {
		req.AddCookie(c)
	}
	back, err := noRedirects.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = back.Body.Close()
	return back.Header.Get("Location"), cookieNamed(back.Cookies(), sessionCookie)
}

// whoAmI is GET /v1/me for a session.
type whoAmI struct {
	OrgID string             `json:"org_id"`
	Role  string             `json:"role"`
	CSRF  string             `json:"csrf"`
	Orgs  []store.Membership `json:"orgs"`
}

func (s orgsSetup) me(t *testing.T, session *http.Cookie) (whoAmI, int) {
	t.Helper()
	var me whoAmI
	status, _ := s.do(t, http.MethodGet, "/v1/me", "", session, "", &me)
	return me, status
}

// do sends one request as a session, and decodes a 200 into out.
func (s orgsSetup) do(t *testing.T, method, path, body string, session *http.Cookie, csrf string,
	out any) (int, []*http.Cookie) {
	t.Helper()
	req, _ := http.NewRequest(method, s.ts.URL+httpx.ControlPrefix+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	if session != nil {
		req.AddCookie(session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s: %v", raw, err)
		}
	}
	return resp.StatusCode, resp.Cookies()
}

// switchTo moves a session to orgID and returns the status and the cookies
// the answer set.
func (s orgsSetup) switchTo(t *testing.T, session *http.Cookie, orgID string) (int, []*http.Cookie) {
	t.Helper()
	me, _ := s.me(t, session)
	return s.do(t, http.MethodPost, "/v1/me/org", `{"org_id":"`+orgID+`"}`, session, me.CSRF, nil)
}

func (s orgsSetup) roleIn(t *testing.T, orgID, externalID string) string {
	t.Helper()
	ms, err := s.st.Memberships(context.Background(), externalID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.OrgID == orgID {
			return m.Role
		}
	}
	return ""
}

func TestAPersonWorksInSeveralOrganisationsWithARoleInEach(t *testing.T) {
	s := membershipServer(t, false)
	ctx := context.Background()
	// Insurer invites Ada, who belongs to the Bank by her domain.
	if _, err := s.st.AddUser(ctx, "user_inv", "org_b", "ada@example.ch", "", "admin"); err != nil {
		t.Fatal(err)
	}

	_, session := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil)
	if session == nil {
		t.Fatal("the sign-in was refused")
	}
	me, _ := s.me(t, session)
	if me.OrgID != "org_a" || me.Role != "member" {
		t.Fatalf("signed in to %s as %s, want the Bank's member", me.OrgID, me.Role)
	}
	if len(me.Orgs) != 2 || me.Orgs[1].OrgID != "org_b" || !me.Orgs[1].Invited {
		t.Fatalf("orgs = %+v, want the Bank and an invitation from Insurer", me.Orgs)
	}

	status, cookies := s.switchTo(t, session, "org_b")
	if status != http.StatusOK {
		t.Fatalf("switching: status %d", status)
	}
	if c := cookieNamed(cookies, lastOrgCookie); c == nil || c.Value != "org_b" {
		t.Errorf("the browser does not remember Insurer: %v", cookies)
	}
	me, _ = s.me(t, session)
	if me.OrgID != "org_b" || me.Role != "admin" {
		t.Fatalf("after switching: in %s as %s, want Insurer's admin", me.OrgID, me.Role)
	}
	for _, m := range me.Orgs {
		if m.Invited {
			t.Errorf("%s is still an invitation after joining it", m.OrgName)
		}
	}

	// A new sign-in in this browser lands where it was last.
	_, again := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil,
		&http.Cookie{Name: lastOrgCookie, Value: "org_b"})
	if me, _ := s.me(t, again); me.OrgID != "org_b" {
		t.Errorf("signed in again to %s, want Insurer, the one this browser was last in", me.OrgID)
	}
	// Elsewhere, the last one used anywhere.
	if status, _ := s.switchTo(t, again, "org_a"); status != http.StatusOK {
		t.Fatalf("switching back: status %d", status)
	}
	_, elsewhere := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil)
	if me, _ := s.me(t, elsewhere); me.OrgID != "org_a" {
		t.Errorf("signed in on another browser to %s, want the Bank, used last", me.OrgID)
	}
}

func TestAPersonKeepsTheProviderTheyFirstSignedInWith(t *testing.T) {
	s := membershipServer(t, false)
	if _, session := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil); session == nil {
		t.Fatal("the first sign-in was refused")
	}
	where, session := s.signIn(t, "other", "sub-x", "ada@example.ch", nil)
	if session != nil || !strings.Contains(where, "sign_in_error") {
		t.Fatalf("a second provider signed the address in: sent to %q", where)
	}
	if !strings.Contains(where, "through+test") {
		t.Errorf("the error does not name the provider to use: %q", where)
	}
	if _, session := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil); session == nil {
		t.Error("the first provider no longer signs in")
	}
}

func TestSwitchingNeedsAnOrganisationThePersonIsIn(t *testing.T) {
	s := membershipServer(t, false)
	_, session := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil)
	if status, _ := s.switchTo(t, session, "org_b"); status != http.StatusNotFound {
		t.Errorf("switching to an organisation she is not in: status %d, want 404", status)
	}

	// Turned off at Insurer: still listed nowhere, and refused.
	ctx := context.Background()
	u, err := s.st.AddUser(ctx, "user_b", "org_b", "ada@example.ch", "test:sub-ada", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.Pool().Exec(ctx, "UPDATE users SET disabled_at = now() WHERE id = $1",
		u.ID); err != nil {
		t.Fatal(err)
	}
	if me, _ := s.me(t, session); len(me.Orgs) != 0 {
		t.Errorf("orgs = %+v, want none to switch to", me.Orgs)
	}
	if status, _ := s.switchTo(t, session, "org_b"); status != http.StatusForbidden {
		t.Errorf("switching to where she is turned off: status %d, want 403", status)
	}
}

func TestTheDirectoryDecidesTheAdminRoleOnlyInItsOwnOrganisation(t *testing.T) {
	s := membershipServer(t, true)
	ctx := context.Background()
	if _, err := s.st.AddUser(ctx, "user_inv", "org_b", "ada@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	admins := []string{"keera-admins"}
	_, session := s.signIn(t, "test", "sub-ada", "ada@example.ch", admins)
	if status, _ := s.switchTo(t, session, "org_b"); status != http.StatusOK {
		t.Fatalf("switching: status %d", status)
	}
	// Signing in again lands at Insurer, whose roles its own administrators set.
	_, again := s.signIn(t, "test", "sub-ada", "ada@example.ch", admins,
		&http.Cookie{Name: lastOrgCookie, Value: "org_b"})
	if me, _ := s.me(t, again); me.OrgID != "org_b" || me.Role != "member" {
		t.Errorf("at Insurer as %s in %s, want a member: the Bank's directory decided", me.Role, me.OrgID)
	}
	if got := s.roleIn(t, "org_a", "test:sub-ada"); got != "admin" {
		t.Errorf("role at the Bank = %q, want admin from the directory", got)
	}
}

func TestTheOperatorRoleGoesWhereThePersonGoes(t *testing.T) {
	s := membershipServer(t, false)
	ctx := context.Background()
	if _, err := s.st.AddUser(ctx, "user_inv", "org_b", "op@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	_, session := s.signIn(t, "test", "sub-op", "op@example.ch", nil)
	if status, _ := s.switchTo(t, session, "org_b"); status != http.StatusOK {
		t.Fatalf("switching: status %d", status)
	}
	if me, _ := s.me(t, session); me.Role != "operator" {
		t.Errorf("role after switching = %q, want operator", me.Role)
	}
}

func TestSigningOutEverywhereEndsEveryOrganisationsSessions(t *testing.T) {
	s := membershipServer(t, false)
	ctx := context.Background()
	if _, err := s.st.AddUser(ctx, "user_inv", "org_b", "ada@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	_, first := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil)
	_, second := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil)
	if status, _ := s.switchTo(t, second, "org_b"); status != http.StatusOK {
		t.Fatalf("switching: status %d", status)
	}
	me, _ := s.me(t, first)
	if status, _ := s.do(t, http.MethodPost, "/auth/logout?all=1", "", first, me.CSRF,
		nil); status != http.StatusOK {
		t.Fatalf("signing out: status %d", status)
	}
	if _, status := s.me(t, second); status != http.StatusUnauthorized {
		t.Errorf("the session at Insurer still works: status %d", status)
	}
}

// A refresh token belongs to the identity, so a session in any of the
// person's organisations is checked against the directory.
func TestADirectoryCheckReachesEveryOrganisation(t *testing.T) {
	s := membershipServer(t, false)
	ctx := context.Background()
	if _, err := s.st.AddUser(ctx, "user_inv", "org_b", "ada@example.ch", "", "member"); err != nil {
		t.Fatal(err)
	}
	s.idps["test"].RefreshToken = "rt-1"
	_, session := s.signIn(t, "test", "sub-ada", "ada@example.ch", nil)
	if status, _ := s.switchTo(t, session, "org_b"); status != http.StatusOK {
		t.Fatalf("switching: status %d", status)
	}
	if _, err := s.st.Pool().Exec(ctx, `UPDATE identities
		SET directory_checked_at = now() - $1::interval WHERE external_id = 'test:sub-ada'`,
		(2 * authn.DirectoryCheckEvery).String()); err != nil {
		t.Fatal(err)
	}
	s.idps["test"].RefuseRefresh = "invalid_grant"
	if _, status := s.me(t, session); status != http.StatusUnauthorized {
		t.Errorf("status at Insurer = %d after the directory refused, want 401", status)
	}
}
