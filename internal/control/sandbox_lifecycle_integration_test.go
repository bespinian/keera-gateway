package control

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/sandbox"
)

// failingDriver fails every create, as podman does when an image pull breaks
// off, and remembers what it was asked to remove.
type failingDriver struct {
	stubDriver
	mu         sync.Mutex
	detached   bool
	terminated []string
}

func (d *failingDriver) Create(ctx context.Context, _ sandbox.Spec) (sandbox.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.detached = ctx.Done() == nil
	return sandbox.Status{}, errors.New("pulling the image broke off")
}

func (d *failingDriver) Terminate(_ context.Context, ref sandbox.Ref) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.terminated = append(d.terminated, ref.Name)
	return nil
}

func TestAFailedCreateLeavesNoAgentMachineBehind(t *testing.T) {
	st, ctx := sandboxStore(t)
	class := policy.SandboxClass{
		OrgID: "org_1", Name: "standard", Image: "example/sandbox:1",
		Isolation: policy.IsolationIsolated, CPU: 4000, Memory: 16384, Disk: 51200,
		DefaultTTLSeconds: 4 * 3600, MaxTTLSeconds: 24 * 3600,
		Purposes: []policy.Purpose{policy.PurposeEngineer, policy.PurposeAgent},
	}
	if err := st.UpsertSandboxClass(ctx, &class); err != nil {
		t.Fatal(err)
	}
	if err := st.PutGuardrail(ctx, policy.ScopeOrg, "org_1", policy.Limits{
		AllowedRepos: []string{"acme"},
	}); err != nil {
		t.Fatal(err)
	}
	driver := &failingDriver{}
	m := sandbox.NewManager(st, driver, sandbox.ManagerOptions{Git: &fakeForge{}})
	ts := sandboxServer(t, st, m)

	for _, tc := range []struct {
		purpose policy.Purpose
		body    map[string]any
	}{
		{policy.PurposeAgent, map[string]any{"task": "fix the build"}},
		{policy.PurposeEngineer, map[string]any{"authorized_keys": []string{"ssh-ed25519 AAAA test"}}},
	} {
		body := map[string]any{
			"org_id": "org_1", "name": string(tc.purpose), "class": "standard",
			"purpose": tc.purpose, "repo": "https://git.example.ch/acme/app.git",
		}
		for k, v := range tc.body {
			body[k] = v
		}
		if code := call(t, ts, http.MethodPost, httpx.ControlPrefix+"/v1/sandboxes", body, nil); code < 400 {
			t.Fatalf("%s: a failed create answered %d", tc.purpose, code)
		}
	}
	if !driver.detached {
		t.Error("the driver was given the request's context, which a client hanging up cancels")
	}
	// The engineer's row stays live, and terminating it removes the machine.
	if len(driver.terminated) != 1 || driver.terminated[0] != "agent" {
		t.Errorf("removed %v, want only the agent's sandbox", driver.terminated)
	}
}
