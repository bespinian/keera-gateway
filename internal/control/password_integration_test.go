package control

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/authn/ldaptest"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Password sign-in end to end, against ldaptest's directory: the panel's
// form, the `keera login` hand-over, and the directory being asked again.

const aliceSubject = "5b0f4c2e-1d3a-4f6b-9c8d-7e6f5a4b3c2d"

type passwordEnv struct {
	*passkeyEnv
	dir *ldaptest.Directory
}

func newPasswordEnv(t *testing.T) *passwordEnv {
	t.Helper()
	st, ctx := testStore(t)
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Corp", EmailDomain: "corp.example.ch"},
		store.OrgTemplate{}); err != nil {
		t.Fatal(err)
	}
	alice := ldaptest.Person("alice", "alice@corp.example.ch", aliceSubject,
		"cn=keera-admins,ou=groups,dc=corp,dc=example,dc=ch")
	dir := &ldaptest.Directory{Entries: []*ldap.Entry{alice},
		Passwords: map[string]string{alice.DN: "right"}}
	cfg := ldaptest.Config("corp")
	cfg.Mapping.AdminGroups = []string{"CN=keera-admins,OU=groups,DC=corp,DC=example,DC=ch"}
	box, err := secret.New("a-secret-key-long-enough-to-be-real")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Secrets: box, PublicURL: passkeyOrigin,
		Providers: authn.Providers{ldaptest.New(t, cfg, dir)},
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &passwordEnv{passkeyEnv: &passkeyEnv{t: t, ts: ts, st: st, ctx: ctx}, dir: dir}
}

func TestAPasswordSignsInThroughTheDirectory(t *testing.T) {
	e := newPasswordEnv(t)
	b := e.browser()
	status, out := b.do("POST", "/auth/password", map[string]string{
		"provider": "corp", "username": "alice", "password": "right", "next": "/users",
	}, "")
	if status != http.StatusOK || out["redirect"] != "/users" {
		t.Fatalf("sign-in: %d %v", status, out)
	}
	status, me := b.me()
	if status != http.StatusOK || me["email"] != "alice@corp.example.ch" || me["role"] != "admin" {
		t.Fatalf("me: %d %v", status, me)
	}
	// The subject is kept, so the directory is asked again later.
	if linked, err := e.st.HasRefreshToken(e.ctx, "corp:"+aliceSubject); err != nil || !linked {
		t.Errorf("HasRefreshToken = %v, %v; want the directory linked", linked, err)
	}

	// Taken out of the directory, the next due check signs her out.
	e.dir.Change(func(d *ldaptest.Directory) { d.Entries = nil })
	if _, err := e.st.Pool().Exec(context.Background(),
		"UPDATE identities SET directory_checked_at = now() - interval '1 hour' WHERE external_id = $1",
		"corp:"+aliceSubject); err != nil {
		t.Fatal(err)
	}
	if status, _ := b.me(); status != http.StatusUnauthorized {
		t.Errorf("me after removal: %d, want 401", status)
	}
}

func TestAPasswordSignInHandsOverToTheTerminal(t *testing.T) {
	e := newPasswordEnv(t)
	b := e.browser()
	verifier := authn.NewFlow().Verifier
	q := url.Values{
		"provider":      {"corp"},
		"cli_redirect":  {"http://127.0.0.1:53682/callback"},
		"cli_challenge": {authn.CLIChallenge(verifier)},
		"cli_state":     {"terminal-state"},
	}
	status, out := b.do("GET", "/auth/login?"+q.Encode(), nil, "")
	loc, _ := out["location"].(string)
	prefix := "/#password-sign-in=corp."
	// .cli tells the form it is signing in the command line.
	if status != http.StatusFound || !strings.HasPrefix(loc, prefix) || !strings.HasSuffix(loc, ".cli") {
		t.Fatalf("login: %d %v, want a redirect to the panel's form", status, out)
	}
	flow := strings.TrimSuffix(strings.TrimPrefix(loc, prefix), ".cli")

	// A typo does not end the sign-in the terminal waits on.
	try := func(password string) (int, map[string]any) {
		return b.do("POST", "/auth/password", map[string]string{
			"provider": "corp", "flow": flow, "username": "alice", "password": password,
		}, "")
	}
	if status, out := try("wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d %v", status, out)
	}
	status, out = try("right")
	to, _ := out["redirect"].(string)
	if status != http.StatusOK || !strings.HasPrefix(to, "http://127.0.0.1:53682/callback?") ||
		!strings.Contains(to, "state=terminal-state") {
		t.Fatalf("right password: %d %v, want the loopback hand-over", status, out)
	}
	// It works once.
	if status, out := try("right"); status != http.StatusUnauthorized {
		t.Errorf("second use: %d %v, want 401", status, out)
	}
}
