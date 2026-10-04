package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		`{"org_id":"org_a","user_id":"user_alice","name":"new laptop"}`, nil)
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

func (d *recordingDriver) Revive(_ context.Context, spec sandbox.Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.env == nil {
		d.env = map[string]map[string]string{}
	}
	d.env[spec.Name] = spec.Env
	return nil
}

func (d *recordingDriver) Status(_ context.Context, ref sandbox.Ref) (sandbox.Status, error) {
	return sandbox.Status{Ref: ref, State: policy.SandboxReady}, nil
}

// fakeForge numbers its tokens and remembers which were revoked. Like the real
// forges, it asks the guardrail about the repository's path first.
type fakeForge struct {
	mu      sync.Mutex
	minted  int
	revoked []string
}

func (f *fakeForge) Mint(_ context.Context, req sandbox.GitRequest) (sandbox.GitCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimSuffix(req.Repo[strings.LastIndexAny(req.Repo, ":")+1:], ".git")
	path = strings.TrimPrefix(strings.TrimPrefix(path, "//git.example.ch"), "/")
	if req.Allow == nil {
		return sandbox.GitCredential{}, &sandbox.ErrRefused{Reason: "no guardrail"}
	}
	if err := req.Allow(path); err != nil {
		return sandbox.GitCredential{}, err
	}
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
	if err := st.PutPolicy(t.Context(), policy.ScopeOrg, "org_1", policy.Limits{
		AllowedRepos: []string{"acme"},
	}); err != nil {
		t.Fatal(err)
	}

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
		"repo": "https://git.example.ch/acme/forbidden", "task": "fix the login",
	}, &refusal); code != http.StatusConflict || refusal.Error.Message != "the forge says no" {
		t.Errorf("a refused repository = %d %q", code, refusal.Error.Message)
	}
}

func TestOffboardStopsAPersonsSandboxes(t *testing.T) {
	st, ctx := sandboxStore(t)
	git := &fakeForge{}
	m := sandbox.NewManager(st, &recordingDriver{}, sandbox.ManagerOptions{Git: git})
	user, err := st.AddUser(ctx, "usr_1", "org_1", "leaver@example.ch", "", "member")
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

// One forge credential reaches every tenant's repositories. A sandbox may only
// check out what its organisation's guardrail allows, and taking a repository
// off the list stops the next refresh too.
func TestASandboxOnlyGetsTheRepositoriesItsGuardrailAllows(t *testing.T) {
	st, ctx := sandboxStore(t)
	driver := &recordingDriver{}
	m := sandbox.NewManager(st, driver, sandbox.ManagerOptions{
		PublicURL: "http://gateway.internal:8080", Git: &fakeForge{},
	})
	ts := sandboxServer(t, st, m)
	create := func(name, repo string) (int, string) {
		var out struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", map[string]any{
			"org_id": "org_1", "name": name, "class": "standard", "purpose": "agent", "repo": repo,
			"task": "fix the login",
		}, &out)
		return code, out.Error.Message
	}

	// Nothing set: no repository at all.
	if code, msg := create("none", "https://git.example.ch/acme/app"); code != http.StatusConflict ||
		!strings.Contains(msg, "allowed_repos") {
		t.Errorf("with no list = %d %q, want a refusal naming the setting", code, msg)
	}

	put := func(repos ...string) {
		t.Helper()
		if err := st.PutPolicy(ctx, policy.ScopeOrg, "org_1", policy.Limits{
			AllowedRepos: repos,
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("acme")
	if code, msg := create("theirs", "https://git.example.ch/bankb/core"); code != http.StatusConflict ||
		!strings.Contains(msg, "bankb/core") {
		t.Errorf("another tenant's repository = %d %q, want a refusal", code, msg)
	}
	if code, msg := create("ours", "https://git.example.ch/acme/app"); code != http.StatusCreated {
		t.Fatalf("an allowed repository = %d %q", code, msg)
	}

	put("acme/other")
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
		ts.URL+httpx.SandboxPrefix+sandbox.GitCredentialPath, nil)
	req.Header.Set("Authorization", "Bearer "+driver.env["ours"]["KEERA_API_KEY"])
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("refreshing after the repository left the list = %d, want 409", res.StatusCode)
	}
}

// Only an operator decides which repositories an organisation reaches. Its
// administrator may still change the rest of its guardrail without losing
// that list.
func TestOnlyAnOperatorSetsAnOrganisationsRepositories(t *testing.T) {
	tn := twoTenants(t)
	admin := &authn.Principal{Via: authn.MethodSession, Role: authn.RoleAdmin, OrgID: "org_a"}
	operator := &authn.Principal{Via: authn.MethodOperatorKey}
	put := func(p *authn.Principal, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, httpx.ControlPrefix+"/v1/guardrails/org/org_a",
			strings.NewReader(body)).WithContext(tn.ctx)
		r.SetPathValue("scope", "org")
		r.SetPathValue("id", "org_a")
		tn.srv.putGuardrails(w, r, p)
		return w.Code
	}

	if code := put(operator, `{"allowed_repos":["acme"]}`); code != http.StatusOK {
		t.Fatalf("an operator setting the list = %d", code)
	}
	if code := put(admin, `{"allowed_repos":["*"]}`); code != http.StatusForbidden {
		t.Errorf("an administrator widening it = %d, want 403", code)
	}
	if code := put(admin, `{"rpm":60}`); code != http.StatusOK {
		t.Fatalf("an administrator setting a rate limit = %d", code)
	}
	lim, err := tn.srv.st.GetPolicy(tn.ctx, policy.ScopeOrg, "org_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(lim.AllowedRepos) != 1 || lim.AllowedRepos[0] != "acme" || lim.RPM == nil {
		t.Errorf("guardrail = repos %v, rpm %v; want the operator's list kept", lim.AllowedRepos, lim.RPM)
	}
	if code := put(operator, `{"allowed_repos":["acme/../x"]}`); code != http.StatusBadRequest {
		t.Errorf("a malformed entry = %d, want 400", code)
	}
}

// An expired engineer's sandbox has lost its key and repository credential,
// but kept its volume. Resuming it, by name, gives it new ones and a new
// lifetime.
func TestAnExpiredSandboxResumesWithANewKey(t *testing.T) {
	st, ctx := sandboxStore(t)
	driver := &recordingDriver{}
	git := &fakeForge{}
	m := sandbox.NewManager(st, driver, sandbox.ManagerOptions{Git: git})
	ts := sandboxServer(t, st, m)
	if err := st.PutPolicy(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedRepos: []string{"acme"},
	}); err != nil {
		t.Fatal(err)
	}

	var sb store.Sandbox
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", map[string]any{
		"org_id": "org_1", "name": "desk", "class": "standard",
		"repo":            "https://git.example.ch/acme/app.git",
		"authorized_keys": []string{"ssh-ed25519 AAAA test"},
	}, &sb); code != http.StatusCreated {
		t.Fatalf("creating the sandbox = %d", code)
	}
	oldKey := driver.env["desk"]["KEERA_API_KEY"]

	// Its time runs out.
	m.Sweep(ctx, sb.ExpiresAt.Add(time.Minute))
	expired, err := st.Sandbox(ctx, sb.ID)
	if err != nil || expired.State != policy.SandboxExpired {
		t.Fatalf("after the sweep: %+v, %v; want expired", expired.State, err)
	}
	if _, err := st.LookupKey(ctx, auth.Hash(oldKey)); err == nil {
		t.Error("an expired sandbox's key still works")
	}
	// A second sweep does not end it again.
	m.Sweep(ctx, sb.ExpiresAt.Add(2*time.Minute))
	if again, _ := st.Sandbox(ctx, sb.ID); again.State != policy.SandboxExpired {
		t.Errorf("a second sweep left it %s", again.State)
	}
	// Its name is still taken, so nobody else can get a second "desk".
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", map[string]any{
		"org_id": "org_1", "name": "desk", "class": "standard",
		"authorized_keys": []string{"ssh-ed25519 AAAA test"},
	}, nil); code == http.StatusCreated {
		t.Error("a second sandbox took the name of an expired one")
	}

	var resumed store.Sandbox
	if code := call(t, ts, http.MethodPost,
		httpx.ControlPrefix+"/v1/sandboxes/desk/resume?org_id=org_1", nil, &resumed,
	); code != http.StatusOK {
		t.Fatalf("resuming by name = %d", code)
	}
	if !resumed.State.Running() {
		t.Errorf("state after resume = %s", resumed.State)
	}
	if resumed.ExpiresAt == nil || !resumed.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at after resume = %v, want in the future", resumed.ExpiresAt)
	}
	newEnv := driver.env["desk"]
	if newEnv["KEERA_API_KEY"] == "" || newEnv["KEERA_API_KEY"] == oldKey {
		t.Fatal("the resumed sandbox was not given a new key")
	}
	if _, err := st.LookupKey(ctx, auth.Hash(newEnv["KEERA_API_KEY"])); err != nil {
		t.Errorf("the new key does not work: %v", err)
	}
	if newEnv["KEERA_GIT_TOKEN"] != "token-2" {
		t.Errorf("KEERA_GIT_TOKEN = %q, want a new one", newEnv["KEERA_GIT_TOKEN"])
	}
}

func TestResumingAnExpiredSandboxLetsTheSamePeopleIn(t *testing.T) {
	// Resuming an expired sandbox starts it again. On a machine with no disk
	// of its own nothing survives from before, so its ssh keys have to come
	// from the row, or nobody can open a shell in it.
	st, ctx := sandboxStore(t)
	driver := &recordingDriver{}
	m := sandbox.NewManager(st, driver, sandbox.ManagerOptions{})
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample mine@laptop"
	sb, err := m.Create(ctx, sandbox.CreateRequest{
		OrgID: "org_1", Owner: "mine@example.ch", Name: "desk", Class: "standard",
		AuthorizedKeys: []string{key},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
		State: policy.SandboxExpired, Detail: "expired",
	}); err != nil {
		t.Fatal(err)
	}
	row, err := st.Sandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	driver.env = nil
	if err := m.Resume(ctx, row, policy.ResolvedSandbox{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := driver.env["desk"]["KEERA_AUTHORIZED_KEYS"]; got != key {
		t.Errorf("the resumed sandbox was given the keys %q, want %q", got, key)
	}
}

// suspendingDriver can suspend, which the stub cannot.
type suspendingDriver struct{ recordingDriver }

func (*suspendingDriver) Suspend(context.Context, sandbox.Ref) error { return nil }

func TestOffboardCountsOnlyWhatItSuspended(t *testing.T) {
	// An engineer's expired sandbox is kept, like a suspended one, but saying
	// it was suspended would count it twice in "what happened to their work".
	st, ctx := sandboxStore(t)
	m := sandbox.NewManager(st, &suspendingDriver{}, sandbox.ManagerOptions{})
	user, err := st.AddUser(ctx, "usr_1", "org_1", "leaver@example.ch", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	for _, sb := range []store.Sandbox{
		{ID: "sbx_wip", Name: "wip", State: policy.SandboxReady},
		{ID: "sbx_old", Name: "old", State: policy.SandboxExpired},
	} {
		sb.OrgID, sb.UserID, sb.Class, sb.ExpiresAt = "org_1", user.ID, "standard", &expires
		sb.Purpose = policy.PurposeEngineer
		if _, err := st.CreateSandbox(ctx, sb); err != nil {
			t.Fatal(err)
		}
	}
	done, err := m.Offboard(ctx, user.ID)
	if err != nil || done.Suspended != 1 || done.Terminated != 0 || done.Failed != 0 {
		t.Errorf("Offboard = %+v, %v; want the running one suspended and nothing else", done, err)
	}
}
