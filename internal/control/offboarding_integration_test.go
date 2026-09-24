package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

func TestDisablingAPersonTurnsOffEverythingTheyHold(t *testing.T) {
	tn := twoTenants(t)
	carol := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a", UserID: "user_carol",
	}
	alice := &authn.Principal{
		Via: authn.MethodSession, Role: authn.RoleMember, OrgID: "org_a", UserID: "user_alice",
	}
	disable := func(p *authn.Principal, who string) int {
		return tn.call(tn.srv.disableUser, p, http.MethodPost, "/v1/users/"+who+"/disable", "",
			map[string]string{"id": who}).Code
	}

	// Only an administrator, only inside their organisation, and not themselves.
	if code := disable(alice, "user_bob"); code != http.StatusForbidden {
		t.Errorf("a member disabling a colleague = %d, want 403", code)
	}
	if code := disable(carol, "user_dave"); code == http.StatusOK {
		t.Error("an administrator disabled somebody in another organisation")
	}
	if code := disable(carol, "user_carol"); code != http.StatusForbidden {
		t.Errorf("disabling yourself = %d, want 403", code)
	}

	if code := disable(carol, "user_alice"); code != http.StatusOK {
		t.Fatalf("disabling alice = %d, want 200", code)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_alice")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("alice's key = %v, want revoked", err)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_bob")); err != nil {
		t.Errorf("bob's key stopped working: %v", err)
	}

	// Nobody can hand her a new one while she is disabled.
	w := tn.call(tn.srv.createKey, carol, http.MethodPost, "/v1/keys",
		`{"org_id":"org_a","user_id":"user_alice","alias":"new laptop"}`, nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "disabled") {
		t.Errorf("a key for a disabled person = %d %s, want 403 saying why", w.Code, w.Body)
	}

	w = tn.call(tn.srv.enableUser, carol, http.MethodPost, "/v1/users/user_alice/enable", "",
		map[string]string{"id": "user_alice"})
	if w.Code != http.StatusOK {
		t.Fatalf("enabling alice = %d: %s", w.Code, w.Body)
	}
	if _, err := tn.srv.st.LookupKey(tn.ctx, []byte("hash-of-key_alice")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("enabling brought her old key back: %v", err)
	}
}

// recordingDriver is stubDriver, keeping each sandbox's environment so a test
// can act as the sandbox.
type recordingDriver struct {
	stubDriver
	mu  sync.Mutex
	env map[string]map[string]string
}

func (d *recordingDriver) Create(ctx context.Context, spec sandbox.Spec) (sandbox.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.env == nil {
		d.env = map[string]map[string]string{}
	}
	d.env[spec.Name] = spec.Env
	return d.stubDriver.Create(ctx, spec)
}

func (d *recordingDriver) Status(_ context.Context, ref sandbox.Ref) (sandbox.Status, error) {
	return sandbox.Status{Ref: ref, State: policy.SandboxReady}, nil
}

// fakeForge numbers its tokens and remembers which were revoked.
type fakeForge struct {
	mu      sync.Mutex
	minted  int
	revoked []string
}

func (f *fakeForge) Mint(_ context.Context, req sandbox.GitRequest) (sandbox.GitCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(req.Repo, "forbidden") {
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: "the forge says no"}
	}
	f.minted++
	return sandbox.GitCredential{
		Repo: "https://git.example.ch/acme/app.git", Username: "keera",
		Token: fmt.Sprintf("token-%d", f.minted), ID: fmt.Sprint(f.minted),
		Expires: time.Now().Add(time.Hour),
	}, nil
}

func (f *fakeForge) Revoke(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
	return nil
}

func TestASandboxRefreshesItsRepositoryCredentialWithItsOwnKey(t *testing.T) {
	st, _ := sandboxStore(t)
	driver := &recordingDriver{}
	git := &fakeForge{}
	m := sandbox.NewManager(st, driver, sandbox.ManagerOptions{
		PublicURL: "http://gateway.internal:8080", Git: git,
	})
	ts := sandboxServer(t, st, m)

	var sb store.Sandbox
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", map[string]any{
		"org_id": "org_1", "name": "fix-login", "class": "standard", "purpose": "agent",
		"repo": "git@git.example.ch:acme/app.git", "task": "fix the login",
	}, &sb); code != http.StatusCreated {
		t.Fatalf("creating the sandbox = %d", code)
	}
	env := driver.env["fix-login"]
	if env["KEERA_GIT_TOKEN"] != "token-1" || env["KEERA_REPO"] != "https://git.example.ch/acme/app.git" {
		t.Errorf("the sandbox was told %q and %q", env["KEERA_GIT_TOKEN"], env["KEERA_REPO"])
	}
	if env["KEERA_GIT_CREDENTIAL_URL"] != "http://gateway.internal:8080/sandbox/v1/git-credential" {
		t.Errorf("KEERA_GIT_CREDENTIAL_URL = %q", env["KEERA_GIT_CREDENTIAL_URL"])
	}
	if sb.Repo != "https://git.example.ch/acme/app.git" {
		t.Errorf("the row records %q, not the address that was cloned", sb.Repo)
	}

	refresh := func(key string) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
			ts.URL+httpx.SandboxPrefix+sandbox.GitCredentialPath, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}

	code, body := refresh(env["KEERA_API_KEY"])
	if code != http.StatusOK || !strings.Contains(body, "username=keera\npassword=token-2\n") ||
		!strings.Contains(body, "password_expiry_utc=") {
		t.Fatalf("refresh = %d %q", code, body)
	}
	if strings.Join(git.revoked, ",") != "1" {
		t.Errorf("revoked %v after a refresh, want the token it replaced", git.revoked)
	}
	if code, _ := refresh("keera_sk_not-a-sandbox"); code != http.StatusUnauthorized {
		t.Errorf("a key of no sandbox = %d, want 401", code)
	}

	// Ending the sandbox takes the credential back, and its key with it.
	row, err := st.Sandbox(t.Context(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Terminate(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if strings.Join(git.revoked, ",") != "1,2" {
		t.Errorf("revoked %v after termination, want 1,2", git.revoked)
	}
	if code, _ := refresh(env["KEERA_API_KEY"]); code != http.StatusUnauthorized {
		t.Errorf("a terminated sandbox's key = %d, want 401", code)
	}
	if _, err := st.LookupKey(t.Context(), auth.Hash(env["KEERA_API_KEY"])); err == nil {
		t.Error("a terminated sandbox's key still works")
	}

	// A forge's refusal reaches the developer as it is.
	var refusal struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", map[string]any{
		"org_id": "org_1", "name": "nope", "class": "standard", "purpose": "agent",
		"repo": "https://git.example.ch/acme/forbidden",
	}, &refusal); code != http.StatusConflict || refusal.Error.Message != "the forge says no" {
		t.Errorf("a refused repository = %d %q", code, refusal.Error.Message)
	}
}

func TestOffboardStopsAPersonsSandboxes(t *testing.T) {
	st, ctx := sandboxStore(t)
	git := &fakeForge{}
	m := sandbox.NewManager(st, &recordingDriver{}, sandbox.ManagerOptions{Git: git})
	user, err := st.UpsertUser(ctx, "usr_1", "org_1", "leaver@example.ch", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	for _, sb := range []store.Sandbox{
		{ID: "sbx_agent", Name: "agent", Purpose: policy.PurposeAgent, GitCredentialID: "7"},
		{ID: "sbx_pending", Name: "pending", Purpose: policy.PurposeEngineer, State: policy.SandboxPending},
	} {
		sb.OrgID, sb.UserID, sb.Class, sb.ExpiresAt = "org_1", user.ID, "standard", &expires
		if sb.State == "" {
			sb.State = policy.SandboxReady
		}
		if _, err := st.CreateSandbox(ctx, sb); err != nil {
			t.Fatal(err)
		}
	}
	done, err := m.Offboard(ctx, user.ID)
	if err != nil || done.Terminated != 2 || done.Failed != 0 {
		t.Fatalf("Offboard = %+v, %v; want both terminated", done, err)
	}
	if strings.Join(git.revoked, ",") != "7" {
		t.Errorf("revoked %v, want the agent sandbox's credential", git.revoked)
	}
	if live, _ := st.ListSandboxes(ctx, store.SandboxQuery{UserID: user.ID}); len(live) != 0 {
		t.Errorf("%d sandboxes still live", len(live))
	}
}
