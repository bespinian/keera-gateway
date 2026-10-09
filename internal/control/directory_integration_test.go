package control

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/oidctest"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A person signed in through a directory, with a terminal token and a refresh
// token whose check is due. What these cover is that the next request asks the
// directory, and that its answer is what decides.

type directorySetup struct {
	srv   *Server
	ts    *httptest.Server
	st    *store.Store
	idp   *oidctest.IDP
	user  store.User
	token string
}

func signedInThroughADirectory(t *testing.T, refreshToken string) directorySetup {
	t.Helper()
	st, ctx := testStore(t, "users", "cli_codes", "cli_tokens", "sessions")
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("creating the organisation: %v", err)
	}
	// The fake provider is called "test" and answers for subject sub-123.
	user, err := st.AddUser(ctx, "user_1", "org_1", "ada@example.ch", "test:sub-123", "admin")
	if err != nil {
		t.Fatalf("creating the person: %v", err)
	}

	idp := oidctest.New(t)
	provider, err := authn.NewOIDC(ctx, authn.OIDCConfig{
		Name: "test", IssuerURL: idp.URL, ClientID: "keera", ClientSecret: "secret",
		RedirectURL: "https://keera.example.ch/control/auth/callback",
		Mapping:     authn.RoleMapping{AdminGroups: []string{"keera-admins"}, Default: authn.RoleMember},
	}, idp.Client())
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	box, err := secret.New("a-secret-key-long-enough-to-be-real")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF", Secrets: box,
		Providers: authn.Providers{provider},
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if refreshToken != "" {
		sealed := box.Seal(refreshTokenName(user.ID), refreshToken)
		if err := st.RecordDirectoryCheck(ctx, user.ID, sealed); err != nil {
			t.Fatalf("storing the refresh token: %v", err)
		}
	}
	token, hash := authn.NewCLIToken()
	if err := st.CreateCLIToken(ctx, hash, user.ID, time.Now().Add(authn.CLITokenTTL)); err != nil {
		t.Fatalf("creating the token: %v", err)
	}
	return directorySetup{srv: srv, ts: ts, st: st, idp: idp, user: user, token: token}
}

// makeDue moves the last check back, as if the person signed in a while ago.
func (d directorySetup) makeDue(t *testing.T) {
	t.Helper()
	if _, err := d.st.Pool().Exec(context.Background(),
		"UPDATE users SET directory_checked_at = now() - interval '1 hour' WHERE id = $1",
		d.user.ID); err != nil {
		t.Fatal(err)
	}
}

func TestADirectoryThatRefusesEndsEverySignIn(t *testing.T) {
	d := signedInThroughADirectory(t, "rt-1")
	d.makeDue(t)
	d.idp.RefuseRefresh = "invalid_grant"

	if got := asToken(t, d.ts, d.token, nil); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 once the directory refuses", got)
	}
	// The token is gone, not just refused once.
	d.idp.RefuseRefresh = ""
	if got := asToken(t, d.ts, d.token, nil); got != http.StatusUnauthorized {
		t.Errorf("status = %d on the next request, want 401", got)
	}
	if linked, _ := d.st.HasRefreshToken(context.Background(), d.user.ID); linked {
		t.Error("the refresh token is still stored")
	}
}

func TestADirectoryCheckAppliesTheCurrentRole(t *testing.T) {
	d := signedInThroughADirectory(t, "rt-1")
	d.makeDue(t)
	d.idp.Claims = func(m map[string]any) { m["groups"] = []string{"engineering"} }
	d.idp.RefreshToken = "rt-2"

	var me struct {
		Role string `json:"role"`
	}
	if got := asToken(t, d.ts, d.token, &me); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if me.Role != "member" {
		t.Errorf("role = %q, want member now that the admin group is gone", me.Role)
	}
	// The rotated refresh token is the one spent next time.
	d.makeDue(t)
	if got := asToken(t, d.ts, d.token, nil); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if got := d.idp.LastForm().Get("refresh_token"); got != "rt-2" {
		t.Errorf("refresh token spent = %q, want the rotated one", got)
	}
}

func TestADirectoryCheckRunsOnlyWhenDue(t *testing.T) {
	d := signedInThroughADirectory(t, "rt-1")
	for range 3 {
		if got := asToken(t, d.ts, d.token, nil); got != http.StatusOK {
			t.Fatalf("status = %d", got)
		}
	}
	if n := d.idp.Refreshes(); n != 0 {
		t.Errorf("refreshes = %d right after sign-in, want 0", n)
	}
	d.makeDue(t)
	for range 3 {
		if got := asToken(t, d.ts, d.token, nil); got != http.StatusOK {
			t.Fatalf("status = %d", got)
		}
	}
	if n := d.idp.Refreshes(); n != 1 {
		t.Errorf("refreshes = %d, want 1 for one due check", n)
	}
}

func TestADirectoryOutageKeepsPeopleSignedIn(t *testing.T) {
	d := signedInThroughADirectory(t, "rt-1")
	d.makeDue(t)
	d.idp.FailRefresh = true

	if got := asToken(t, d.ts, d.token, nil); got != http.StatusOK {
		t.Errorf("status = %d, want 200 while the directory is down", got)
	}
}

func TestATerminalTokenIsShortWithoutARefreshToken(t *testing.T) {
	for _, tc := range []struct {
		refreshToken string
		want         time.Duration
	}{
		{"rt-1", authn.CLITokenTTL},
		{"", authn.SessionTTL},
	} {
		d := signedInThroughADirectory(t, tc.refreshToken)
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		got, err := d.srv.cliTokenTTL(req, d.user)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("refresh token %q: ttl = %v, want %v", tc.refreshToken, got, tc.want)
		}
	}
}

// The callback finishes only a sign-in this browser started. Otherwise a link
// to the callback of someone else's sign-in would sign a victim in to the
// sender's account.
func TestASignInFinishesOnlyInTheBrowserThatStartedIt(t *testing.T) {
	d := signedInThroughADirectory(t, "")
	get := func(u string, c *http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if c != nil {
			req.AddCookie(c)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	cookie := func(resp *http.Response, name string) *http.Cookie {
		for _, c := range resp.Cookies() {
			if c.Name == name && c.Value != "" {
				return c
			}
		}
		return nil
	}

	resp := get(d.ts.URL+httpx.ControlPrefix+"/auth/login?provider=test", nil)
	started := cookie(resp, loginCookie)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || started == nil {
		t.Fatalf("login = %d, Location %q, cookie %v", resp.StatusCode, resp.Header.Get("Location"), started)
	}
	d.idp.Claims = func(m map[string]any) { m["nonce"] = to.Query().Get("nonce") }
	callback := d.ts.URL + httpx.ControlPrefix + "/auth/callback?code=c&state=" +
		url.QueryEscape(to.Query().Get("state"))

	resp = get(callback, nil)
	if cookie(resp, sessionCookie) != nil || !strings.Contains(resp.Header.Get("Location"), "sign_in_error") {
		t.Errorf("another browser finished the sign-in: Location %q", resp.Header.Get("Location"))
	}
	resp = get(callback, started)
	if cookie(resp, sessionCookie) == nil || resp.Header.Get("Location") != "/" {
		t.Errorf("the browser that started it was not signed in: Location %q", resp.Header.Get("Location"))
	}
}
