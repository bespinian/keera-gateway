package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/passkeytest"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The passkey ceremonies end to end: an administrator creates the account,
// the link adds the first passkey, and the passkey signs in. The browser and
// its authenticator are passkeytest, which signs what a real one would.

const passkeyOrigin = "http://localhost:8080"

type passkeyEnv struct {
	t   *testing.T
	ts  *httptest.Server
	st  *store.Store
	ctx context.Context
}

func newPasskeyEnv(t *testing.T, domain string) *passkeyEnv {
	t.Helper()
	st, ctx := streamStore(t)
	if _, err := st.Pool().Exec(ctx,
		"TRUNCATE users, sessions, login_flows, cli_codes, cli_tokens RESTART IDENTITY CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank", EmailDomain: domain},
		store.OrgTemplate{}); err != nil {
		t.Fatal(err)
	}
	rp, err := authn.NewRelyingParty(passkeyOrigin)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF", Passkeys: rp, PublicURL: passkeyOrigin,
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &passkeyEnv{t: t, ts: ts, st: st, ctx: ctx}
}

// browser is a client with its own cookies, like one person's browser.
type browser struct {
	env  *passkeyEnv
	c    *http.Client
	csrf string
}

func (e *passkeyEnv) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{env: e, c: &http.Client{
		Jar: jar,
		// The redirects are what is being tested, so they are not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends one request. auth is "operator" for the operator key, or empty for
// the browser's own cookies.
func (b *browser) do(method, path string, in any, auth string) (int, map[string]any) {
	b.env.t.Helper()
	var body *bytes.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, b.env.ts.URL+httpx.ControlPrefix+path, body)
	req.Header.Set("Content-Type", "application/json")
	if auth == "operator" {
		req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	} else if b.csrf != "" {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.env.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		out = map[string]any{"location": res.Header.Get("Location")}
	}
	return res.StatusCode, out
}

// me reads who the browser is signed in as, and keeps the CSRF token.
func (b *browser) me() (int, map[string]any) {
	status, me := b.do("GET", "/v1/me", nil, "")
	if status == http.StatusOK {
		b.csrf, _ = me["csrf"].(string)
	}
	return status, me
}

// decode turns one field of a response back into a typed value.
func decode[T any](t *testing.T, v any) T {
	t.Helper()
	raw, _ := json.Marshal(v)
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return out
}

// createPasskeyAccount adds a person as the operator and returns the token of
// their set-up link.
func (e *passkeyEnv) createPasskeyAccount(email string) (string, string) {
	e.t.Helper()
	status, out := e.browser().do("POST", "/v1/users", map[string]string{
		"org_id": "org_1", "email": email, "sign_in": "passkey",
	}, "operator")
	if status != http.StatusCreated {
		e.t.Fatalf("creating %s: %d %v", email, status, out)
	}
	link := decode[passkeyLinkOut](e.t, out["passkey_link"])
	prefix := passkeyOrigin + "/#passkey-setup="
	if !strings.HasPrefix(link.URL, prefix) {
		e.t.Fatalf("link = %q, want it under %s", link.URL, prefix)
	}
	if ext, _ := out["external_id"].(string); !authn.IsPasskeyAccount(ext) {
		e.t.Errorf("external_id = %q, want a passkey account", ext)
	}
	return out["id"].(string), strings.TrimPrefix(link.URL, prefix)
}

// setUp registers a passkey from a link in a fresh browser, which ends up
// signed in.
func (e *passkeyEnv) setUp(token string, a *passkeytest.Authenticator) (*browser, int, map[string]any) {
	e.t.Helper()
	b := e.browser()
	status, opts := b.do("POST", "/auth/passkey/setup/options", map[string]string{"token": token}, "")
	if status != http.StatusOK {
		return b, status, opts
	}
	o := decode[authn.CreationOptions](e.t, opts["options"])
	a.UserHandle = []byte(mustDecode(e.t, o.User.ID))
	status, out := b.do("POST", "/auth/passkey/setup", map[string]any{
		"token": token, "challenge_id": opts["challenge_id"], "name": "laptop",
		"credential": a.Register(o.Challenge),
	}, "")
	return b, status, out
}

// signIn signs a fresh browser in with a passkey.
func (e *passkeyEnv) signIn(a *passkeytest.Authenticator) (*browser, int, map[string]any) {
	e.t.Helper()
	return e.signInTo(a, "")
}

// signInTo signs in from the panel, which asks to go on to next.
func (e *passkeyEnv) signInTo(a *passkeytest.Authenticator, next string) (*browser, int, map[string]any) {
	e.t.Helper()
	b := e.browser()
	status, opts := b.do("POST", "/auth/passkey/options", map[string]string{"next": next}, "")
	if status != http.StatusOK {
		e.t.Fatalf("options: %d %v", status, opts)
	}
	o := decode[authn.RequestOptions](e.t, opts["options"])
	status, out := b.do("POST", "/auth/passkey/sign-in", map[string]any{
		"flow": opts["flow"], "credential": a.Sign(o.Challenge),
	}, "")
	return b, status, out
}

func mustDecode(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAPasskeyAccountSetsUpAndSignsIn(t *testing.T) {
	e := newPasskeyEnv(t, "")
	userID, token := e.createPasskeyAccount("ada@example.ch")
	a := passkeytest.New(passkeyOrigin, "localhost")

	b, status, out := e.setUp(token, a)
	if status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}
	// The set-up signs the person in.
	if status, me := b.me(); status != http.StatusOK || me["user_id"] != userID ||
		me["passkey_account"] != true {
		t.Fatalf("after set-up: %d %v", status, me)
	}
	// The link works once.
	if _, status, _ := e.setUp(token, passkeytest.New(passkeyOrigin, "localhost")); status != http.StatusUnauthorized {
		t.Errorf("a used link gave %d, want 401", status)
	}

	b, status, out = e.signIn(a)
	if status != http.StatusOK || out["redirect"] != "/" {
		t.Fatalf("sign-in: %d %v", status, out)
	}
	if status, me := b.me(); status != http.StatusOK || me["email"] != "ada@example.ch" {
		t.Fatalf("after sign-in: %d %v", status, me)
	}

	// A passkey that is not registered signs nobody in.
	if _, status, _ := e.signIn(passkeytest.New(passkeyOrigin, "localhost")); status != http.StatusUnauthorized {
		t.Errorf("an unknown passkey gave %d, want 401", status)
	}
	// Nor does a registered one used on another site.
	a.Origin = "https://keera-login.example"
	if _, status, _ := e.signIn(a); status != http.StatusUnauthorized {
		t.Errorf("a phished sign-in gave %d, want 401", status)
	}
}

func TestASecondPasskeyIsAddedWhileSignedIn(t *testing.T) {
	e := newPasskeyEnv(t, "")
	_, token := e.createPasskeyAccount("ada@example.ch")
	first := passkeytest.New(passkeyOrigin, "localhost")
	b, status, out := e.setUp(token, first)
	if status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}
	b.me()

	// The only passkey cannot be removed by its owner: that would lock them out.
	status, list := b.do("GET", "/v1/passkeys", nil, "")
	keys := decode[[]store.Passkey](t, list["data"])
	if status != http.StatusOK || len(keys) != 1 || keys[0].Name != "laptop" {
		t.Fatalf("passkeys: %d %v", status, list)
	}
	if status, _ := b.do("DELETE", "/v1/passkeys/"+keys[0].ID, nil, ""); status != http.StatusConflict {
		t.Errorf("removing the last passkey gave %d, want 409", status)
	}

	status, opts := b.do("POST", "/v1/passkeys/options", nil, "")
	if status != http.StatusOK {
		t.Fatalf("options: %d %v", status, opts)
	}
	o := decode[authn.CreationOptions](t, opts["options"])
	// The browser is told which passkey exists, so it is not registered twice.
	if len(o.ExcludeCredentials) != 1 {
		t.Errorf("excluded = %v, want the first passkey", o.ExcludeCredentials)
	}
	second := passkeytest.NewEd25519(passkeyOrigin, "localhost")
	status, out = b.do("POST", "/v1/passkeys", map[string]any{
		"challenge_id": opts["challenge_id"], "name": "phone",
		"credential": second.Register(o.Challenge),
	}, "")
	if status != http.StatusCreated {
		t.Fatalf("adding: %d %v", status, out)
	}
	if _, status, _ := e.signIn(second); status != http.StatusOK {
		t.Errorf("the second passkey gave %d", status)
	}
	if status, _ := b.do("DELETE", "/v1/passkeys/"+keys[0].ID, nil, ""); status != http.StatusOK {
		t.Errorf("removing one of two gave %d, want 200", status)
	}
	if _, status, _ := e.signIn(first); status != http.StatusUnauthorized {
		t.Errorf("a removed passkey gave %d, want 401", status)
	}
}

// Nobody proved a passkey account's address, so it must never pick up the
// operator role, whatever is stored.
func TestAPasskeySignInDropsTheOperatorRole(t *testing.T) {
	e := newPasskeyEnv(t, "")
	userID, token := e.createPasskeyAccount("ada@example.ch")
	a := passkeytest.New(passkeyOrigin, "localhost")
	if _, status, out := e.setUp(token, a); status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}
	if err := e.st.SetUserRole(e.ctx, userID, "operator"); err != nil {
		t.Fatal(err)
	}
	b, status, out := e.signIn(a)
	if status != http.StatusOK {
		t.Fatalf("sign-in: %d %v", status, out)
	}
	if _, me := b.me(); me["role"] != "member" || me["unrestricted"] != false {
		t.Errorf("signed in as %v, want a member", me)
	}
}

func TestADirectoryAccountGetsNoPasskey(t *testing.T) {
	e := newPasskeyEnv(t, "")
	user, err := e.st.AddUser(e.ctx, "user_1", "org_1", "sso@example.ch", "google:1", "member")
	if err != nil {
		t.Fatal(err)
	}
	status, out := e.browser().do("POST", "/v1/users/"+user.ID+"/passkey-link", nil, "operator")
	if status != http.StatusConflict {
		t.Errorf("a link for a directory account gave %d %v, want 409", status, out)
	}
	// Someone added ahead of their first sign-in can still become one.
	fresh, _ := e.st.AddUser(e.ctx, "user_2", "org_1", "new@example.ch", "", "member")
	if status, out := e.browser().do("POST", "/v1/users/"+fresh.ID+"/passkey-link", nil,
		"operator"); status != http.StatusCreated {
		t.Errorf("a link for a fresh account gave %d %v", status, out)
	}
}

// An administrator vouches for the address, so only for their own domain.
func TestAnAdministratorKeepsToTheirDomain(t *testing.T) {
	e := newPasskeyEnv(t, "example.ch")
	admin, err := e.st.AddUser(e.ctx, "user_admin", "org_1", "boss@example.ch", "google:boss", "admin")
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	token, hash, csrf := authn.NewSession()
	if err := e.st.CreateSession(e.ctx, hash, admin.ID, csrf, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(e.ts.URL)
	b.c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: token, Path: "/"}})
	b.me()

	status, out := b.do("POST", "/v1/users", map[string]string{
		"email": "ceo@another.ch", "sign_in": "passkey",
	}, "")
	if status != http.StatusForbidden {
		t.Errorf("another domain gave %d %v, want 403", status, out)
	}
	if status, out := b.do("POST", "/v1/users", map[string]string{
		"email": "ada@EXAMPLE.ch", "sign_in": "passkey",
	}, ""); status != http.StatusCreated {
		t.Errorf("the organisation's domain gave %d %v, want 201", status, out)
	}
	// The operator may use any.
	if status, out := e.browser().do("POST", "/v1/users", map[string]string{
		"org_id": "org_1", "email": "ceo@another.ch", "sign_in": "passkey",
	}, "operator"); status != http.StatusCreated {
		t.Errorf("the operator gave %d %v, want 201", status, out)
	}
}

// A deep link survives a passkey sign-in, as it does single sign-on, but only
// to a page of this gateway.
func TestAPasskeySignInGoesOnToWhereThePersonWasGoing(t *testing.T) {
	e := newPasskeyEnv(t, "")
	_, token := e.createPasskeyAccount("ada@example.ch")
	a := passkeytest.New(passkeyOrigin, "localhost")
	if _, status, out := e.setUp(token, a); status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}
	for next, want := range map[string]string{
		"/keys?org=org_1":      "/keys?org=org_1",
		"https://evil.example": "/",
		"//evil.example":       "/",
	} {
		_, status, out := e.signInTo(a, next)
		if status != http.StatusOK || out["redirect"] != want {
			t.Errorf("next %q: %d %v, want a redirect to %q", next, status, out, want)
		}
	}
}

func TestADisabledPersonCannotSignInWithAPasskey(t *testing.T) {
	e := newPasskeyEnv(t, "")
	userID, token := e.createPasskeyAccount("ada@example.ch")
	a := passkeytest.New(passkeyOrigin, "localhost")
	if _, status, out := e.setUp(token, a); status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}
	if _, err := e.st.DisableUser(e.ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := e.signIn(a); status == http.StatusOK {
		t.Error("a disabled person signed in")
	}
	// Disabling removed the passkey, so enabling them does not bring it back.
	if err := e.st.EnableUser(e.ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := e.signIn(a); status != http.StatusUnauthorized {
		t.Errorf("after enabling, the old passkey gave %d, want 401", status)
	}
}

func TestTheCommandLineSignsInWithAPasskey(t *testing.T) {
	e := newPasskeyEnv(t, "")
	_, token := e.createPasskeyAccount("ada@example.ch")
	a := passkeytest.New(passkeyOrigin, "localhost")
	if _, status, out := e.setUp(token, a); status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}

	verifier := authn.NewCLIVerifier()
	q := url.Values{
		"provider":      {"passkey"},
		"cli_redirect":  {"http://127.0.0.1:4242/callback"},
		"cli_challenge": {authn.CLIChallenge(verifier)},
		"cli_state":     {"cli-state"},
	}
	b := e.browser()
	status, out := b.do("GET", "/auth/login?"+q.Encode(), nil, "")
	loc, _ := out["location"].(string)
	state, ok := strings.CutPrefix(loc, "/#passkey-sign-in=")
	if status != http.StatusFound || !ok {
		t.Fatalf("login: %d %q", status, loc)
	}
	status, opts := b.do("POST", "/auth/passkey/options", map[string]string{"flow": state}, "")
	if status != http.StatusOK || opts["flow"] != state {
		t.Fatalf("options: %d %v", status, opts)
	}
	o := decode[authn.RequestOptions](t, opts["options"])
	status, out = b.do("POST", "/auth/passkey/sign-in", map[string]any{
		"flow": state, "credential": a.Sign(o.Challenge),
	}, "")
	to, _ := out["redirect"].(string)
	if status != http.StatusOK || !strings.HasPrefix(to, "http://127.0.0.1:4242/callback?") {
		t.Fatalf("sign-in: %d %v", status, out)
	}
	back, _ := url.Parse(to)
	if back.Query().Get("state") != "cli-state" {
		t.Errorf("the hand-over lost its state: %s", to)
	}
	var tok map[string]any
	if status := redeem(t, e.ts, map[string]string{
		"code": back.Query().Get("code"), "verifier": verifier,
	}, &tok); status != http.StatusOK || !strings.HasPrefix(tok["token"].(string), authn.CLITokenPrefix) {
		t.Errorf("redeeming: %d %v", status, tok)
	}
}

// A cross-site form cannot send JSON, so it cannot sign a browser in.
func TestThePasskeyRoutesTakeOnlyJSON(t *testing.T) {
	e := newPasskeyEnv(t, "")
	res, err := http.Post(e.ts.URL+httpx.ControlPrefix+"/auth/passkey/sign-in", "text/plain",
		strings.NewReader(`{"flow":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a form post gave %d, want 415", res.StatusCode)
	}
}

// A passkey outlives every session, so adding one needs a fresh browser
// sign-in: not an old session, and not a command-line token.
func TestAddingAPasskeyNeedsARecentBrowserSignIn(t *testing.T) {
	e := newPasskeyEnv(t, "")
	userID, token := e.createPasskeyAccount("ada@example.ch")
	b, status, out := e.setUp(token, passkeytest.New(passkeyOrigin, "localhost"))
	if status != http.StatusOK {
		t.Fatalf("set-up: %d %v", status, out)
	}
	b.me()
	if status, _ := b.do("POST", "/v1/passkeys/options", nil, ""); status != http.StatusOK {
		t.Fatalf("a fresh session gave %d, want 200", status)
	}
	if _, err := e.st.Pool().Exec(e.ctx,
		"UPDATE sessions SET created_at = now() - interval '1 hour' WHERE user_id = $1", userID); err != nil {
		t.Fatal(err)
	}
	if status, out := b.do("POST", "/v1/passkeys/options", nil, ""); status != http.StatusForbidden ||
		!strings.Contains(fmt.Sprint(out), "sign_in_again") {
		t.Errorf("an hour-old session gave %d %v, want 403 sign_in_again", status, out)
	}

	cli, hash := authn.NewCLIToken()
	if err := e.st.CreateCLIToken(e.ctx, hash, userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", e.ts.URL+httpx.ControlPrefix+"/v1/passkeys/options", nil)
	req.Header.Set("Authorization", "Bearer "+cli)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a command-line token gave %d, want 403", res.StatusCode)
	}
}
