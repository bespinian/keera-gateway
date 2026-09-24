package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestEnsurePoolWritesTemplateAndPool(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{
		Warm:     true,
		Runtimes: map[policy.Isolation]string{policy.IsolationIsolated: "gvisor"},
	})

	class := policy.SandboxClass{
		Name: "standard", Image: "example/sandbox:1",
		Isolation: policy.IsolationIsolated, CPU: 4000, Memory: 16384, Disk: 51200,
		Warm: 2,
	}
	if err := k.EnsurePool(context.Background(), class); err != nil {
		t.Fatalf("ensure pool: %v", err)
	}

	var tmpl, pool string
	for _, r := range f.seen {
		if r.Method != http.MethodPost {
			continue
		}
		raw, _ := json.Marshal(r.Body)
		switch r.Body["kind"] {
		case "SandboxTemplate":
			tmpl = string(raw)
		case "SandboxWarmPool":
			pool = string(raw)
		}
	}
	if tmpl == "" || pool == "" {
		t.Fatalf("expected both objects; template=%q pool=%q", tmpl, pool)
	}

	// Without the two Overrides policies, the key, session and volume that
	// arrive on the claim would not be applied.
	for _, want := range []string{
		`"envVarsInjectionPolicy":"Overrides"`,
		`"volumeClaimTemplatesPolicy":"Overrides"`,
		`"runtimeClassName":"gvisor"`,
		`"image":"example/sandbox:1"`,
	} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("the template does not contain %s\n%s", want, tmpl)
		}
	}
	if !strings.Contains(pool, `"replicas":2`) {
		t.Errorf("the pool does not ask for two replicas\n%s", pool)
	}
	// Recreate would empty the pool whenever a class changed.
	if !strings.Contains(pool, `"type":"OnReplenish"`) {
		t.Errorf("the pool should replace members as they are handed out\n%s", pool)
	}
}

func TestEnsurePoolRemovesAZeroPool(t *testing.T) {
	// A pool of zero looks like a feature in use and holds nothing.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})
	if err := k.EnsurePool(context.Background(),
		policy.SandboxClass{Name: "cold", Image: "i", Warm: 0}); err != nil {
		t.Fatalf("ensure pool: %v", err)
	}
	var deletes int
	for _, r := range f.seen {
		if r.Method == http.MethodDelete {
			deletes++
		}
		if r.Method == http.MethodPost {
			t.Error("a class with warm: 0 should create nothing")
		}
	}
	if deletes != 2 {
		t.Errorf("got %d deletes, want the pool and its template", deletes)
	}
}

func TestCreateClaimCarriesTheCredentials(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})

	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err := k.Create(context.Background(), Spec{
		Ref: Ref{ID: "sbx_warm01", Name: "warm", Backing: BackingClaim},
		Class: policy.SandboxClass{
			Name: "standard", Image: "i", Isolation: policy.IsolationStandard,
			CPU: 2000, Memory: 4096, Warm: 2,
		},
		Purpose: policy.PurposeAgent,
		Org:     "org_1",
		Owner:   "dev@example.ch",
		Env:     map[string]string{"KEERA_API_KEY": "keera_sk_x"},
		Expires: expires,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var claim string
	for _, r := range f.seen {
		if r.Method == http.MethodPost && r.Body["kind"] == "SandboxClaim" {
			raw, _ := json.Marshal(r.Body)
			claim = string(raw)
		}
	}
	if claim == "" {
		t.Fatal("no claim was created")
	}
	for _, want := range []string{
		`"warmPoolRef":{"name":"keera-standard"}`,
		`"KEERA_API_KEY"`,
		// Named, so a sidecar never gets this sandbox's API key.
		`"containerName":"sandbox"`,
		// An agent's sandbox is terminated when its time runs out.
		`"shutdownPolicy":"Delete"`,
		`"keera.dev/org":"org_1"`,
	} {
		if !strings.Contains(claim, want) {
			t.Errorf("the claim does not contain %s\n%s", want, claim)
		}
	}
}

func TestClaimedStatusDistinguishesUnboundFromUnscheduled(t *testing.T) {
	// An unbound claim means the pool is too small, not the cluster.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})
	ref := Ref{ID: "sbx_warm02", Name: "unbound", Backing: BackingClaim}
	f.put(claimsPath+objectName(ref),
		`{"status":{"conditions":[{"type":"Bound","status":"False","reason":"PoolEmpty"}]}}`)

	st, err := k.Status(context.Background(), ref)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != policy.SandboxPending {
		t.Errorf("state = %s, want pending", st.State)
	}
	if st.Detail != "PoolEmpty" {
		t.Errorf("detail = %q, want the controller's own reason", st.Detail)
	}
}

func TestClaimedStatusReady(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})
	ref := Ref{ID: "sbx_warm03", Name: "bound", Backing: BackingClaim}
	f.put(claimsPath+objectName(ref),
		`{"spec":{"lifecycle":{"shutdownTime":"2026-09-14T18:00:00Z"}},
		  "status":{"sandbox":{"name":"pool-abc","podIPs":["10.1.2.9"]},
		  "conditions":[{"type":"Ready","status":"True"}]}}`)

	st, err := k.Status(context.Background(), ref)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != policy.SandboxReady {
		t.Errorf("state = %s, want ready", st.State)
	}
	if st.Address != "10.1.2.9" {
		t.Errorf("address = %q; the attach surface dials this", st.Address)
	}
	if st.Expires.IsZero() {
		t.Error("the claim's own lifetime should be read back")
	}
}

func TestExtendingAClaimMovesTheClaimsLifetime(t *testing.T) {
	// The claim's lifetime moves, because deleting the claim ends the sandbox.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})
	ref := Ref{ID: "sbx_warm04", Name: "ext", Backing: BackingClaim}
	path := claimsPath + objectName(ref)
	f.put(path, `{"spec":{}}`)

	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if err := k.Extend(context.Background(), ref, until); err != nil {
		t.Fatalf("extend: %v", err)
	}
	last := f.seen[len(f.seen)-1]
	if last.Path != path {
		t.Fatalf("patched %s, want the claim at %s", last.Path, path)
	}
	spec, _ := last.Body["spec"].(map[string]any)
	lifecycle, _ := spec["lifecycle"].(map[string]any)
	if got := lifecycle["shutdownTime"]; got != until.Format(time.RFC3339) {
		t.Errorf("shutdownTime = %v, want %s", got, until.Format(time.RFC3339))
	}
}

func TestTerminatingAClaimDeletesTheClaim(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})
	ref := Ref{ID: "sbx_warm05", Name: "del", Backing: BackingClaim}
	if err := k.Terminate(context.Background(), ref); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	last := f.seen[len(f.seen)-1]
	if !strings.Contains(last.Path, "/sandboxclaims/") {
		t.Errorf("deleted %s; deleting the bound Sandbox instead would take the pool "+
			"member out of circulation and leave a claim pointing at nothing", last.Path)
	}
}

func TestPrunePoolsRemovesThePoolsOfDeletedClasses(t *testing.T) {
	// EnsurePool never visits a deleted class, so without this its warm
	// sandboxes would hold their CPU and memory for ever.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Warm: true})
	const warm = "/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/sandboxes/"
	items := `{"items":[
		{"metadata":{"name":"keera-standard","labels":{"keera.dev/class":"standard"}}},
		{"metadata":{"name":"keera-large","labels":{"keera.dev/class":"large"}}}]}`
	f.put(warm+"sandboxwarmpools", items)
	f.put(warm+"sandboxtemplates", items)

	if err := k.PrunePools(context.Background(), map[string]bool{"standard": true}); err != nil {
		t.Fatalf("prune pools: %v", err)
	}
	var deleted []string
	for _, r := range f.seen {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.Path)
		}
	}
	want := []string{warm + "sandboxwarmpools/keera-large", warm + "sandboxtemplates/keera-large"}
	if strings.Join(deleted, " ") != strings.Join(want, " ") {
		t.Errorf("deleted %v, want only the pool and template of the deleted class %v", deleted, want)
	}
}
