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
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Signing up: somebody whose sign-in matches no organisation creates one, or
// joins the one that added their address. These go through the whole browser
// flow, from the sign-in link to the session.

type signupSetup struct {
	ts  *httptest.Server
	st  *store.Store
	idp *oidctest.IDP
}

func signupServer(t *testing.T, signUp bool) signupSetup {
	t.Helper()
	st, ctx := testStore(t, "users", "signups", "sessions", "login_flows")
	// Somebody else's organisation, with no domain. Without sign-up, every
	// sign-in would land in it, being the only one.
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank"},
		store.OrgTemplate{}); err != nil {
		t.Fatalf("creating the organisation: %v", err)
	}
	idp := oidctest.New(t)
	idp.RefreshToken = "rt-1"
	provider, err := authn.NewOIDC(ctx, authn.OIDCConfig{
		Name: "test", IssuerURL: idp.URL, ClientID: "keera", ClientSecret: "secret",
		RedirectURL: "https://keera.example.ch/control/auth/callback",
		Mapping:     authn.RoleMapping{Default: authn.RoleMember},
		SignUp:      signUp,
	}, idp.Client())
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	box, err := secret.New("a-secret-key-long-enough-to-be-real")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(ctx, st, registry.Options{Secrets: box}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, reg, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF", Secrets: box,
		Providers: authn.Providers{provider},
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return signupSetup{ts: ts, st: st, idp: idp}
}

// noRedirects answers a redirect instead of following it, so each step's
// cookies and target can be read.
var noRedirects = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// signIn goes through the provider as subject and email, and returns where
// the callback sent the browser and the cookies it set.
func (s signupSetup) signIn(t *testing.T, subject, email string) (string, []*http.Cookie) {
	t.Helper()
	resp, err := noRedirects.Get(s.ts.URL + httpx.ControlPrefix + "/auth/login?provider=test")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	authorize, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	s.idp.Claims = func(m map[string]any) {
		m["sub"], m["email"], m["nonce"] = subject, email, q.Get("nonce")
		// A sign-up provider for any domain must prove the address.
		m["email_verified"] = true
	}

	req, _ := http.NewRequest(http.MethodGet, s.ts.URL+httpx.ControlPrefix+
		"/auth/callback?code=c&state="+url.QueryEscape(q.Get("state")), nil)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	back, err := noRedirects.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = back.Body.Close()
	if back.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want a redirect", back.StatusCode)
	}
	return back.Header.Get("Location"), back.Cookies()
}

// call sends one request to a sign-up route with the cookies given.
func (s signupSetup) call(t *testing.T, method, body string, cookies []*http.Cookie,
	out any) (int, []*http.Cookie) {
	t.Helper()
	req, _ := http.NewRequest(method, s.ts.URL+httpx.ControlPrefix+"/auth/signup",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
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

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestASignInMatchingNoOrganisationCreatesOne(t *testing.T) {
	s := signupServer(t, true)
	ctx := context.Background()

	to, cookies := s.signIn(t, "sub-ada", "ada@new.example")
	if to != "/?sign_up=1" {
		t.Fatalf("sent to %q, want the sign-up screen; the only organisation is "+
			"somebody else's", to)
	}
	if cookieNamed(cookies, sessionCookie) != nil {
		t.Fatal("signed in before choosing an organisation")
	}
	signup := cookieNamed(cookies, signupCookie)
	if signup == nil {
		t.Fatal("no sign-up cookie")
	}

	var info struct {
		Email      string `json:"email"`
		InvitedOrg string `json:"invited_org"`
	}
	if got, _ := s.call(t, http.MethodGet, "", []*http.Cookie{signup}, &info); got != http.StatusOK {
		t.Fatalf("reading the sign-up: status %d", got)
	}
	if info.Email != "ada@new.example" || info.InvitedOrg != "" {
		t.Errorf("sign-up shows %+v, want only the address", info)
	}

	// The name is taken. Saying so would tell a stranger who the customers
	// are, so the address makes it unique instead.
	got, set := s.call(t, http.MethodPost, `{"name":"Example Bank"}`, []*http.Cookie{signup}, nil)
	if got != http.StatusOK {
		t.Fatalf("creating the organisation: status %d", got)
	}
	if cookieNamed(set, sessionCookie) == nil {
		t.Error("not signed in after creating the organisation")
	}

	user, err := s.st.UserByExternalID(ctx, "test:sub-ada")
	if err != nil {
		t.Fatalf("the person was not created: %v", err)
	}
	if user.Role != "admin" || user.OrgID == "org_1" {
		t.Errorf("person = %+v, want the administrator of a new organisation", user)
	}
	org, err := s.st.OrgByID(ctx, user.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	if !org.Limited || org.Name != "Example Bank (ada@new.example)" || org.EmailDomain != "" {
		t.Errorf("organisation = %+v, want a limited one with a unique name and no domain", org)
	}
	if linked, _ := s.st.HasRefreshToken(ctx, user.ID); !linked {
		t.Error("the refresh token was not kept, so the directory is never asked again")
	}

	// The sign-up is used up.
	if got, _ := s.call(t, http.MethodPost, `{"name":"Another"}`, []*http.Cookie{signup}, nil); got != http.StatusUnauthorized {
		t.Errorf("using the sign-up again: status %d, want 401", got)
	}
	// And the next sign-in goes straight in.
	if to, _ := s.signIn(t, "sub-ada", "ada@new.example"); to != "/" {
		t.Errorf("the next sign-in went to %q, want the panel", to)
	}
}

func TestAnInvitationIsJoinedOnlyWhenAccepted(t *testing.T) {
	s := signupServer(t, true)
	ctx := context.Background()
	if _, err := s.st.AddUser(ctx, "user_grace", "org_1", "grace@new.example", "", "member"); err != nil {
		t.Fatal(err)
	}

	to, cookies := s.signIn(t, "sub-grace", "grace@new.example")
	if to != "/?sign_up=1" {
		t.Fatalf("sent to %q, want the sign-up screen", to)
	}
	signup := cookieNamed(cookies, signupCookie)
	var info struct {
		InvitedOrg string `json:"invited_org"`
	}
	if got, _ := s.call(t, http.MethodGet, "", []*http.Cookie{signup}, &info); got != http.StatusOK {
		t.Fatalf("reading the sign-up: status %d", got)
	}
	if info.InvitedOrg != "Example Bank" {
		t.Errorf("invited by %q, want Example Bank", info.InvitedOrg)
	}
	if u, _ := s.st.UserByID(ctx, "user_grace"); u.ExternalID != "" {
		t.Fatal("the invitation was taken before it was accepted")
	}

	if got, _ := s.call(t, http.MethodPost, `{"join":true}`, []*http.Cookie{signup}, nil); got != http.StatusOK {
		t.Fatalf("joining: status %d", got)
	}
	user, err := s.st.UserByExternalID(ctx, "test:sub-grace")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "user_grace" || user.OrgID != "org_1" || user.Role != "member" {
		t.Errorf("person = %+v, want the invited row, with the role it was given", user)
	}
}

func TestWithoutSignUpTheOnlyOrganisationTakesEveryone(t *testing.T) {
	s := signupServer(t, false)
	if to, _ := s.signIn(t, "sub-ada", "ada@new.example"); to != "/" {
		t.Fatalf("sent to %q, want the panel", to)
	}
	user, err := s.st.UserByExternalID(context.Background(), "test:sub-ada")
	if err != nil || user.OrgID != "org_1" {
		t.Errorf("person = %+v, %v; want them in the only organisation", user, err)
	}
}
