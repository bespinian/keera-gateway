package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Deleting a class must not end its sandboxes. An extend or resume then gets
// the lifetimes of a class that states none, however long it has lived.
func TestStandInForADeletedClassKeepsALifetime(t *testing.T) {
	created := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	// Already much older than the default ceiling: that must not make the next
	// lifetime longer.
	expires := created.Add(20 * 24 * time.Hour)
	sb := store.Sandbox{
		OrgID: "org_1", Class: "gone", Image: "i", Isolation: policy.IsolationStandard,
		CPU: 2000, Memory: 4096, CreatedAt: created, ExpiresAt: &expires,
	}
	class := standIn(sb)

	if got := clampTTL(class, 0, policy.ResolvedSandbox{}); got != catalog.DefaultSandboxTTL {
		t.Errorf("no lifetime asked for gives %s, want the default %s", got, catalog.DefaultSandboxTTL)
	}
	if got := clampTTL(class, 30*24*time.Hour, policy.ResolvedSandbox{}); got != catalog.DefaultSandboxMaxTTL {
		t.Errorf("30 days asked for gives %s, want the default ceiling %s", got, catalog.DefaultSandboxMaxTTL)
	}
	ceiling := policy.ResolvedSandbox{MaxSandboxTTLSeconds: 3600}
	if got := clampTTL(class, 0, ceiling); got != time.Hour {
		t.Errorf("under a 1h ceiling it gives %s, want 1h", got)
	}
	if class.Image != "i" || class.CPU != 2000 || class.Memory != 4096 {
		t.Errorf("the stand-in is not the machine the row recorded: %+v", class)
	}
}

// An agent runs no sshd, so it would never pass a pool member's readiness
// probe. Only an engineer's sandbox is claimed from a pool.
func TestOnlyEngineersClaimFromAWarmPool(t *testing.T) {
	warm := policy.SandboxClass{Name: "standard", Warm: 2}
	cases := []struct {
		class   policy.SandboxClass
		purpose policy.Purpose
		pools   bool
		want    Backing
	}{
		{warm, policy.PurposeEngineer, true, BackingClaim},
		{warm, policy.PurposeAgent, true, BackingSandbox},
		{warm, policy.PurposeEngineer, false, BackingSandbox},
		{policy.SandboxClass{Name: "cold"}, policy.PurposeEngineer, true, BackingSandbox},
	}
	for _, c := range cases {
		if got := backingFor(c.class, c.purpose, c.pools); got != c.want {
			t.Errorf("%s sandbox of %s (pools %v): %s, want %s",
				c.purpose, c.class.Name, c.pools, got, c.want)
		}
	}
}

// An agent's task runs again on resume, so an agent sandbox is never
// suspended.
func TestAnAgentSandboxIsNotSuspended(t *testing.T) {
	m := NewManager(nil, nil, ManagerOptions{})
	sb := store.Sandbox{Name: "fix-it", Purpose: policy.PurposeAgent, State: policy.SandboxReady}

	err := m.Suspend(context.Background(), sb)
	if _, ok := errors.AsType[*ErrRefused](err); !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// An agent with no task or no repository would fail later, after its key was
// minted, so it is refused before anything is created.
func TestAnAgentSandboxNeedsATaskAndARepository(t *testing.T) {
	m := NewManager(nil, nil, ManagerOptions{})
	for name, req := range map[string]CreateRequest{
		"no task": {Name: "a", Purpose: policy.PurposeAgent, Repo: "https://example.com/r.git"},
		"no repo": {Name: "a", Purpose: policy.PurposeAgent, Task: "fix the tests"},
	} {
		_, err := m.admit(context.Background(), req)
		if _, ok := errors.AsType[*ErrRefused](err); !ok {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}

// KEERA_BASE_URL decides where a sandbox's key goes, and the gateway leaves it
// unset where it has no public address. A caller's own must not fill the gap.
func TestACallerCannotSetAKeeraVariable(t *testing.T) {
	m := NewManager(nil, nil, ManagerOptions{})
	_, err := m.admit(context.Background(), CreateRequest{
		Name: "a", Purpose: policy.PurposeEngineer,
		Env: map[string]string{"KEERA_BASE_URL": "https://elsewhere.example"},
	})
	if _, ok := errors.AsType[*ErrRefused](err); !ok {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// A sandbox key is a standard key. Its agent must not be offered a
// subscription model, which the gateway refuses that key.
func TestASandboxIsNotOfferedASubscriptionModel(t *testing.T) {
	resolved := policy.Resolve(policy.Key{OrgID: "org_1", Kind: policy.KeyStandard}, nil, nil, nil)
	catalogue := []policy.Model{
		{Alias: "keera-code", Kind: policy.KindChat, Enabled: true},
		{Alias: "claude", Kind: policy.KindChat, Enabled: true, Subscription: true},
		{Alias: "embed", Kind: policy.KindEmbedding, Enabled: true},
		{Alias: "off", Kind: policy.KindChat},
	}
	got := chatModelsFor(resolved, catalogue)
	if len(got) != 1 || got[0].Alias != "keera-code" {
		t.Errorf("offered %+v, want only keera-code", got)
	}
}
