package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The sandbox routes against a real database.
//
// What is worth covering here is not the driver - internal/sandbox tests that
// against a stand-in API server - but the two things only this layer decides:
// who may do what, and what a deployment with no driver says instead of
// failing obscurely.

func sandboxStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	st, ctx := testStore(t, "sandboxes", "sandbox_classes", "guardrails", "api_keys", "users", "projects")
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_1", Name: "Example Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	class := policy.SandboxClass{
		OrgID: "org_1", Name: "standard", Description: "the usual machine", Image: "example/sandbox:1",
		Isolation: policy.IsolationIsolated, CPU: 4000, Memory: 16384, Disk: 51200,
		DefaultTTL: 4 * time.Hour, MaxTTL: 24 * time.Hour,
	}
	if err := st.UpsertSandboxClass(ctx, &class); err != nil {
		t.Fatalf("UpsertSandboxClass: %v", err)
	}
	return st, ctx
}

func sandboxServer(t *testing.T, st *store.Store, m *sandbox.Manager) *httptest.Server {
	t.Helper()
	srv := New(st, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF", Sandboxes: m,
	}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// call is getJSON with a method and a body, for the routes that are not GETs.
func call(t *testing.T, ts *httptest.Server, method, path string, body, into any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if into != nil {
		_ = json.NewDecoder(res.Body).Decode(into)
	}
	return res.StatusCode
}

func TestSandboxCatalogueIsReadableWithNoDriver(t *testing.T) {
	// An operator setting a deployment up wants to see what they have declared
	// before the driver works, so the catalogue is readable either way.
	st, _ := sandboxStore(t)
	ts := sandboxServer(t, st, nil)

	var res struct {
		Data   []policy.SandboxClass `json:"data"`
		Driver map[string]any        `json:"driver"`
	}
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sandbox-classes?org_id=org_1", &res); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if len(res.Data) != 1 || res.Data[0].Name != "standard" {
		t.Fatalf("classes = %+v", res.Data)
	}
	if res.Driver != nil {
		t.Error("a deployment with no driver should not describe one")
	}
}

func TestSandboxRoutesRefuseClearlyWithNoDriver(t *testing.T) {
	// "Not found" on a path a client was told to use is the most confusing of
	// all possible answers: it reads as a version mismatch, and the truth -
	// this deployment has sandboxes switched off - is something an operator can
	// act on.
	st, _ := sandboxStore(t)
	ts := sandboxServer(t, st, nil)

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes",
		map[string]any{"org_id": "org_1", "name": "x", "class": "standard"}, &envelope)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", code)
	}
	if envelope.Error.Code != "sandboxes_disabled" {
		t.Errorf("code = %q", envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "KEERA_SANDBOX_DRIVER") {
		t.Errorf("the refusal should name the setting to change: %q", envelope.Error.Message)
	}

	// The attach surface says the same thing rather than 404ing.
	if code := getJSON(t, ts, httpx.SandboxPrefix+"/v1/anything/tcp/22", nil); code !=
		http.StatusNotImplemented {
		t.Errorf("attach status = %d, want 501", code)
	}
}

// A class belongs to one organisation, so a change names it.
func TestSandboxClassBelongsToAnOrganisation(t *testing.T) {
	st, ctx := sandboxStore(t)
	ts := sandboxServer(t, st, nil)

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	code := call(t, ts, http.MethodPut, httpx.ControlPrefix+"/v1/sandbox-classes/scratch",
		map[string]any{"image": "example/sandbox:2"}, &envelope)
	if code != http.StatusBadRequest || !strings.Contains(envelope.Error.Message, "organisation") {
		t.Fatalf("a class of no organisation = %d %q, want 400 asking for one",
			code, envelope.Error.Message)
	}

	// Validated the same way the file is.
	const path = "/v1/sandbox-classes/scratch?org_id=org_1"
	code = call(t, ts, http.MethodPut, httpx.ControlPrefix+path,
		map[string]any{"image": "example/sandbox:2", "isolation": "kata"}, &envelope)
	if code != http.StatusBadRequest {
		t.Fatalf("an unknown isolation tier should be refused, got %d", code)
	}
	var saved policy.SandboxClass
	code = call(t, ts, http.MethodPut, httpx.ControlPrefix+path,
		map[string]any{"image": "example/sandbox:2", "cpu_millis": 2000, "memory_mib": 4096}, &saved)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if saved.CPU != 2000 || saved.Memory != 4096 || saved.OrgID != "org_1" {
		t.Errorf("saved %+v", saved)
	}
	// What is left out gets the file's defaults.
	if saved.DefaultTTL != 4*time.Hour || saved.Isolation != policy.IsolationIsolated {
		t.Errorf("defaults not filled in: %+v", saved)
	}
	// What the list returns can be written back unchanged.
	again := saved
	code = call(t, ts, http.MethodPut, httpx.ControlPrefix+path, saved, &again)
	if code != http.StatusOK || again.CPU != saved.CPU || again.MaxTTL != saved.MaxTTL {
		t.Errorf("writing back what was read = %d %+v", code, again)
	}
	if _, err := st.SandboxClass(ctx, "org_1", "scratch"); err != nil {
		t.Errorf("SandboxClass: %v", err)
	}

	// A class that came from the template is the organisation's to change.
	code = call(t, ts, http.MethodPut, httpx.ControlPrefix+"/v1/sandbox-classes/standard?org_id=org_1",
		map[string]any{"image": "other:1"}, &saved)
	if code != http.StatusOK || saved.Image != "other:1" {
		t.Errorf("changing the organisation's class = %d %+v", code, saved)
	}
}

// A sandbox mints a key, and the project on that key decides six things that
// belong to whoever owns the project: the system prompt every request carries, the
// budget it is charged to, the rate limit it consumes, the sandbox quota it
// counts against, and the models it may reach. The foreign key on the column
// says only that the project exists somewhere.
func TestSandboxRefusesAnotherOrgsProject(t *testing.T) {
	st, ctx := sandboxStore(t)
	if _, err := st.CreateOrg(ctx, store.Org{ID: "org_2", Name: "Other Bank"}, store.OrgTemplate{}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_other", OrgID: "org_2", Name: "Their Platform"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_ours", OrgID: "org_1", Name: "Our Platform"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	m := sandbox.NewManager(st, stubDriver{}, sandbox.ManagerOptions{
		Log: slog.New(slog.DiscardHandler),
	})
	ts := sandboxServer(t, st, m)

	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes",
		map[string]any{
			"org_id": "org_1", "project_id": "project_other", "name": "cross",
			"class": "standard", "authorized_keys": []string{"ssh-ed25519 AAAA test"},
		}, &envelope)
	// 404, as for a project that does not exist, so another tenant's project ids
	// cannot be probed.
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	// And nothing was created on the way to refusing - not the sandbox, and not
	// the key, which is the one that would have outlived the mistake.
	list, err := st.ListSandboxes(ctx, store.SandboxQuery{All: true})
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a refused request left %d sandboxes behind", len(list))
	}
	var keys int
	if err := st.Pool().QueryRow(ctx,
		"SELECT count(*) FROM api_keys WHERE org_id = $1", "org_1").Scan(&keys); err != nil {
		t.Fatalf("counting keys: %v", err)
	}
	if keys != 0 {
		t.Errorf("a refused request left %d keys behind", keys)
	}

	// The organisation's own project is accepted, and the key carries it - which
	// is the whole point of naming one.
	var sb store.Sandbox
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes",
		map[string]any{
			"org_id": "org_1", "project_id": "project_ours", "name": "ours",
			"class": "standard", "authorized_keys": []string{"ssh-ed25519 AAAA test"},
		}, &sb); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if sb.ProjectID != "project_ours" {
		t.Errorf("sandbox project = %q", sb.ProjectID)
	}
	var keyProject string
	if err := st.Pool().QueryRow(ctx,
		"SELECT COALESCE(project_id,'') FROM api_keys WHERE id = $1", sb.KeyID,
	).Scan(&keyProject); err != nil {
		t.Fatalf("reading the minted key: %v", err)
	}
	if keyProject != "project_ours" {
		t.Errorf("the minted key is on project %q, want project_ours", keyProject)
	}
}

// A project's sandbox guardrail narrows the organisation's, as it does for keys.
// Each count is held to its own level's limit: a project capped at one must not
// cap the whole organisation at one.
func TestSandboxHonoursProjectGuardrail(t *testing.T) {
	st, ctx := sandboxStore(t)
	if _, err := st.CreateProject(ctx, store.Project{ID: "project_small", OrgID: "org_1", Name: "Small"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		MaxSandboxes: new(5),
	}); err != nil {
		t.Fatalf("PutGuardrail org: %v", err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeProject, "project_small", policy.Limits{
		MaxSandboxes: new(1), MaxSandboxTTLSeconds: new(3600),
	}); err != nil {
		t.Fatalf("PutGuardrail project: %v", err)
	}
	m := sandbox.NewManager(st, stubDriver{}, sandbox.ManagerOptions{
		Log: slog.New(slog.DiscardHandler),
	})
	ts := sandboxServer(t, st, m)

	create := func(name, project string, into any) int {
		t.Helper()
		body := map[string]any{
			"org_id": "org_1", "name": name, "class": "standard", "ttl": "8h",
			"authorized_keys": []string{"ssh-ed25519 AAAA test"},
		}
		if project != "" {
			body["project_id"] = project
		}
		return call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", body, into)
	}

	var first store.Sandbox
	if code := create("first", "project_small", &first); code != http.StatusCreated {
		t.Fatalf("first project sandbox: status = %d, want 201", code)
	}
	if first.ExpiresAt == nil || time.Until(*first.ExpiresAt) > time.Hour+time.Minute {
		t.Errorf("expires_at = %v, want within the project's one-hour ceiling", first.ExpiresAt)
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if code := create("second", "project_small", &envelope); code == http.StatusCreated {
		t.Fatal("a second project sandbox was created past the project's limit of one")
	}
	if !strings.Contains(envelope.Error.Message, "your project") {
		t.Errorf("message = %q, want the project's limit named", envelope.Error.Message)
	}

	// The organisation still has room under its own limit of five.
	if code := create("third", "", nil); code != http.StatusCreated {
		t.Errorf("sandbox outside the project: status = %d, want 201", code)
	}
}

// A sandbox is a machine of the deployment's, so an organisation that signed
// itself up gets one only once it has paid, or an operator lifts the limit.
func TestALimitedOrganisationGetsNoSandbox(t *testing.T) {
	st, ctx := sandboxStore(t)
	limited := true
	if _, err := st.UpdateOrg(ctx, "org_1", store.OrgChange{Limited: &limited}); err != nil {
		t.Fatalf("UpdateOrg: %v", err)
	}
	m := sandbox.NewManager(st, stubDriver{}, sandbox.ManagerOptions{
		Log: slog.New(slog.DiscardHandler),
	})
	ts := sandboxServer(t, st, m)
	body := map[string]any{
		"org_id": "org_1", "name": "first", "class": "standard",
		"authorized_keys": []string{"ssh-ed25519 AAAA test"},
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", body, &envelope); code != http.StatusForbidden || envelope.Error.Code != "org_limited" {
		t.Fatalf("status = %d, code %q; want 403 org_limited", code, envelope.Error.Code)
	}

	lifted := false
	if _, err := st.UpdateOrg(ctx, "org_1", store.OrgChange{Limited: &lifted}); err != nil {
		t.Fatalf("UpdateOrg: %v", err)
	}
	if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", body, nil); code != http.StatusCreated {
		t.Errorf("after the limit was lifted: status = %d, want 201", code)
	}
}

func TestSandboxVisibilityAndAttachRules(t *testing.T) {
	st, ctx := sandboxStore(t)

	// Two people in one organisation, each with a sandbox.
	mine, err := st.AddUser(ctx, "usr_1", "org_1", "mine@example.ch", "", "member")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	theirs, err := st.AddUser(ctx, "usr_2", "org_1", "theirs@example.ch", "", "member")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	for _, u := range []struct {
		id, name, user string
	}{{"sbx_mine", "mine", mine.ID}, {"sbx_theirs", "theirs", theirs.ID}} {
		expires := time.Now().Add(time.Hour)
		if _, err := st.CreateSandbox(ctx, store.Sandbox{
			ID: u.id, OrgID: "org_1", UserID: u.user, Owner: u.name + "@example.ch",
			Name: u.name, Class: "standard", Purpose: policy.PurposeEngineer,
			State: policy.SandboxReady, CPU: 4000, Memory: 16384, ExpiresAt: &expires,
		}); err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
	}

	// An administrator may see both, because the quota and the bill are theirs.
	admin := &authnPrincipalAdmin
	if !mayHandleSandbox(admin, store.Sandbox{OrgID: "org_1", UserID: theirs.ID}) {
		t.Error("an administrator should be able to terminate a sandbox in their organisation")
	}
	// And may not get inside one. A sandbox holds a working copy of somebody's
	// source code; "can see the bill" has never implied "can read the desk".
	if canAttach(admin, store.Sandbox{OrgID: "org_1", UserID: theirs.ID}) {
		t.Error("an administrator should not be able to open a shell in somebody else's sandbox")
	}

	member := &authnPrincipalMember
	if !canAttach(member, store.Sandbox{OrgID: "org_1", UserID: member.UserID}) {
		t.Error("a person should be able to attach to their own sandbox")
	}
	if canAttach(member, store.Sandbox{OrgID: "org_1", UserID: theirs.ID}) {
		t.Error("a person should not be able to attach to a colleague's")
	}
	// A sandbox attributed to nobody - one created with the operator key, for a
	// pipeline - is an operator's. Falling back to "anyone in the organisation"
	// would make a shared sandbox a shared shell.
	if canAttach(member, store.Sandbox{OrgID: "org_1"}) {
		t.Error("a sandbox attributed to nobody is not everybody's")
	}
}

func TestAttachRefusesABadPortAndAMissingUpgrade(t *testing.T) {
	st, ctx := sandboxStore(t)
	expires := time.Now().Add(time.Hour)
	if _, err := st.CreateSandbox(ctx, store.Sandbox{
		ID: "sbx_attach", OrgID: "org_1", Name: "attach", Class: "standard",
		Purpose: policy.PurposeEngineer, State: policy.SandboxReady, ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// A manager with a driver that can do nothing: what is under test is the
	// two refusals in front of it.
	m := sandbox.NewManager(st, stubDriver{}, sandbox.ManagerOptions{
		Log: slog.New(slog.DiscardHandler),
	})
	ts := sandboxServer(t, st, m)

	// No upgrade header: this route carries a byte stream, not JSON.
	if code := getJSON(t, ts, httpx.SandboxPrefix+"/v1/sbx_attach/tcp/22", nil); code !=
		http.StatusUpgradeRequired {
		t.Errorf("status = %d, want 426", code)
	}
	// A privileged port that is not the shell: a sandbox's own services should
	// not be reachable from outside it by accident.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		ts.URL+httpx.SandboxPrefix+"/v1/sbx_attach/tcp/80", nil)
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	req.Header.Set("Upgrade", "keera-sandbox/1")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestSandboxListNarrowsToTheReader(t *testing.T) {
	st, ctx := sandboxStore(t)
	expires := time.Now().Add(time.Hour)
	for _, id := range []string{"sbx_a", "sbx_b"} {
		if _, err := st.CreateSandbox(ctx, store.Sandbox{
			ID: id, OrgID: "org_1", Name: strings.TrimPrefix(id, "sbx_"),
			Class: "standard", Purpose: policy.PurposeEngineer,
			State: policy.SandboxReady, ExpiresAt: &expires,
		}); err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
	}
	ts := sandboxServer(t, st, nil)

	// The operator key is unrestricted, so it sees the organisation's.
	var res struct {
		Data []store.Sandbox `json:"data"`
	}
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sandboxes?org_id=org_1", &res); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if len(res.Data) != 2 {
		t.Fatalf("got %d sandboxes, want 2", len(res.Data))
	}

	// The list is live sandboxes by default: "what have I got running" is the
	// question somebody types this to ask.
	if err := st.ObserveSandbox(ctx, "sbx_a", store.SandboxObservation{
		State: policy.SandboxTerminated,
	}); err != nil {
		t.Fatalf("ObserveSandbox: %v", err)
	}
	res.Data = nil
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sandboxes?org_id=org_1", &res); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if len(res.Data) != 1 {
		t.Errorf("got %d live sandboxes, want 1", len(res.Data))
	}
	res.Data = nil
	if code := getJSON(t, ts,
		httpx.ControlPrefix+"/v1/sandboxes?org_id=org_1&all=1", &res); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if len(res.Data) != 2 {
		t.Errorf("with all=1 got %d, want 2", len(res.Data))
	}
}

// The two principals the visibility rules are checked against. An
// administrator of org_1, and a member of it with a sandbox of their own.
var (
	authnPrincipalAdmin = authn.Principal{
		Via: authn.MethodSession, UserID: "usr_admin",
		Email: "admin@example.ch", Role: authn.RoleAdmin, OrgID: "org_1",
	}
	authnPrincipalMember = authn.Principal{
		Via: authn.MethodSession, UserID: "usr_1",
		Email: "mine@example.ch", Role: authn.RoleMember, OrgID: "org_1",
	}
)

// stubDriver accepts everything and runs nothing. It exists so that the routes
// in front of a driver can be tested without a cluster or a container runtime.
//
// It reports the strongest isolation tier so that no test is refused for a
// reason it was not written to be about - a stub that could deliver less than
// the catalogue asks for would fail every creation with a message about
// RuntimeClasses.
type stubDriver struct{}

func (stubDriver) Name() string { return "stub" }
func (stubDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Isolation: policy.IsolationVM, Tiers: policy.Isolations}
}
func (stubDriver) Create(_ context.Context, spec sandbox.Spec) (sandbox.Status, error) {
	return sandbox.Status{Ref: spec.Ref, State: policy.SandboxReady, Address: "10.0.0.1"}, nil
}
func (stubDriver) Status(context.Context, sandbox.Ref) (sandbox.Status, error) {
	return sandbox.Status{}, sandbox.ErrNotFound
}
func (stubDriver) Suspend(context.Context, sandbox.Ref) error { return errors.ErrUnsupported }
func (stubDriver) Resume(context.Context, sandbox.Ref) error  { return errors.ErrUnsupported }
func (stubDriver) Revive(context.Context, sandbox.Spec) error { return nil }
func (stubDriver) Extend(context.Context, sandbox.Ref, time.Time) error {
	return errors.ErrUnsupported
}
func (stubDriver) Terminate(context.Context, sandbox.Ref) error { return nil }
func (stubDriver) Dial(context.Context, sandbox.Ref, int) (net.Conn, error) {
	return nil, sandbox.ErrNotReady
}

func TestImageUnderAllowsTagsAndPathsOfAPrefix(t *testing.T) {
	prefixes := []string{"registry.example.ch/sandbox", "registry.example.ch/team/"}
	for image, want := range map[string]bool{
		"registry.example.ch/sandbox":              true,
		"registry.example.ch/sandbox:2":            true,
		"registry.example.ch/sandbox@sha256:abc":   true,
		"registry.example.ch/team/python:3":        true,
		"registry.example.ch/sandbox-private:1":    false,
		"registry.example.ch/sandbox/private:1":    false,
		"registry.example.ch/other:1":              false,
		"evil.example/registry.example.ch/sandbox": false,
	} {
		if got := imageUnder(image, prefixes); got != want {
			t.Errorf("imageUnder(%q) = %v, want %v", image, got, want)
		}
	}
}

// Sandbox pods pull with the deployment's registry credentials, so an
// administrator could otherwise run another tenant's private image.
func TestAnAdministratorPicksOnlyAllowedImages(t *testing.T) {
	st, _ := sandboxStore(t)
	srv := New(st, nil, nil, nil, Options{
		OperatorKey: testOperatorKey, Currency: "CHF",
		Template:      store.OrgTemplate{SandboxClasses: []policy.SandboxClass{{Image: "example/sandbox:2"}}},
		SandboxImages: []string{"registry.example.ch/team/"},
	}, slog.New(slog.DiscardHandler))

	put := func(p *authn.Principal, name, image string) int {
		raw, _ := json.Marshal(map[string]any{"image": image})
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPut,
			"/v1/sandbox-classes/"+name+"?org_id=org_1", bytes.NewReader(raw))
		r.SetPathValue("name", name)
		w := httptest.NewRecorder()
		srv.putSandboxClass(w, r, p)
		return w.Code
	}
	admin, operator := &authnPrincipalAdmin, &authn.Principal{Via: authn.MethodOperatorKey}
	for image, want := range map[string]int{
		"example/sandbox:2":                 http.StatusOK,
		"registry.example.ch/team/python:3": http.StatusOK,
		"registry.example.ch/private:1":     http.StatusForbidden,
	} {
		if code := put(admin, "scratch", image); code != want {
			t.Errorf("an administrator giving %s: %d, want %d", image, code, want)
		}
	}
	// An operator may give any image, and the class may keep it.
	if code := put(operator, "custom", "registry.example.ch/private:1"); code != http.StatusOK {
		t.Fatalf("an operator giving any image: %d", code)
	}
	if code := put(admin, "custom", "registry.example.ch/private:1"); code != http.StatusOK {
		t.Errorf("an administrator keeping a class's image: %d", code)
	}
}
