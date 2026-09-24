package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The driver against a fake API server. What matters is the JSON the driver
// sends and how it reads the answers, so the fake answers in the CRD's shapes
// and the tests assert on the JSON.

type fakeAPI struct {
	t    *testing.T
	srv  *httptest.Server
	objs map[string]json.RawMessage
	// seen records every request, so a test can assert what was called.
	seen []request
}

type request struct {
	Method string
	Path   string
	Body   map[string]any
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, objs: map[string]json.RawMessage{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := request{Method: r.Method, Path: r.URL.Path}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.Body)
		}
		f.seen = append(f.seen, rec)

		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, "/sandboxes") {
				// The reachability probe at startup.
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			body, ok := f.objs[r.URL.Path]
			if !ok {
				http.Error(w, `{"message":"not found","reason":"NotFound","code":404}`,
					http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		case http.MethodPost:
			// A create names the object in its body and lands one path deeper.
			name, _ := rec.Body["metadata"].(map[string]any)["name"].(string)
			f.objs[r.URL.Path+"/"+name] = raw
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(raw)
		case http.MethodPatch, http.MethodDelete:
			if _, ok := f.objs[r.URL.Path]; !ok && r.Method == http.MethodPatch {
				http.Error(w, `{"message":"not found","reason":"NotFound","code":404}`,
					http.StatusNotFound)
				return
			}
			if r.Method == http.MethodDelete {
				delete(f.objs, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// put seeds an object at a path, for the read paths.
func (f *fakeAPI) put(path, body string) { f.objs[path] = json.RawMessage(body) }

// The paths the fake stores objects at, written out so the tests pin them.
const (
	sandboxesPath = "/apis/agents.x-k8s.io/v1beta1/namespaces/sandboxes/sandboxes/"
	claimsPath    = "/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/sandboxes/sandboxclaims/"
)

// driver builds a driver against the fake. The fake is plain HTTP, so TLS is
// never used; Insecure just means no CA bundle is needed.
func (f *fakeAPI) driver(t *testing.T, opts KubernetesOptions) *Kubernetes {
	t.Helper()
	opts.Kube = KubeConfig{Server: f.srv.URL, Token: "test", Insecure: true}
	if opts.Namespace == "" {
		opts.Namespace = "sandboxes"
	}
	opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	k, err := NewKubernetes(context.Background(), opts)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	return k
}

func TestObjectName(t *testing.T) {
	// It depends only on the Ref, so a restarted gateway finds its sandboxes.
	ref := Ref{ID: "sbx_06c1k2rt8g3m4n5p6q7r8s9t0v", Name: "fix-login"}
	got := objectName(ref)
	if got != objectName(ref) {
		t.Fatal("objectName is not deterministic")
	}
	if !strings.HasPrefix(got, "fix-login-") {
		t.Errorf("objectName = %q; the developer's name has to lead, because this "+
			"is what somebody reads in kubectl get pods", got)
	}
	// The id's tail keeps two sandboxes with the same name apart.
	other := objectName(Ref{ID: "sbx_06c1k2rt8g3m4n5p6q7r8s9t0w", Name: "fix-login"})
	if got == other {
		t.Error("two sandboxes with the same name must get different object names")
	}
	if len(got) > 63 {
		t.Errorf("objectName = %q is %d characters; a DNS label is 63", got, len(got))
	}
	// A nameless ref must not produce a leading dash.
	if n := objectName(Ref{ID: "sbx_abcdefghij"}); strings.HasPrefix(n, "-") {
		t.Errorf("objectName with no name = %q", n)
	}
}

func TestCreateBuildsTheObject(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{
		Runtimes:     map[policy.Isolation]string{policy.IsolationVM: "kata-qemu"},
		StorageClass: "fast-local",
	})

	expires := time.Now().Add(2 * time.Hour)
	_, err := k.Create(context.Background(), Spec{
		Ref: Ref{ID: "sbx_test0001", Name: "fix-login"},
		Class: policy.SandboxClass{
			Name: "big", Image: "example/sandbox:1", Isolation: policy.IsolationVM,
			CPU: 4000, Memory: 16384, Disk: 51200,
		},
		Purpose: policy.PurposeEngineer,
		Owner:   "dev@example.ch",
		Org:     "org_1",
		Team:    "team_1",
		Env:     map[string]string{"KEERA_API_KEY": "keera_sk_x", "AAA": "1"},
		Expires: expires,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var created request
	for _, r := range f.seen {
		if r.Method == http.MethodPost {
			created = r
		}
	}
	if created.Body == nil {
		t.Fatal("nothing was created")
	}
	raw, _ := json.Marshal(created.Body)
	body := string(raw)

	for _, want := range []string{
		`"kind":"Sandbox"`,
		`"apiVersion":"agents.x-k8s.io/v1beta1"`,
		`"runtimeClassName":"kata-qemu"`,
		`"image":"example/sandbox:1"`,
		`"cpu":"4000m"`,
		`"memory":"16Gi"`,
		`"storage":"50Gi"`,
		`"storageClassName":"fast-local"`,
		// An engineer's sandbox keeps its volume when its time runs out.
		`"shutdownPolicy":"Retain"`,
		// Hardening that applies whatever the isolation tier is.
		`"allowPrivilegeEscalation":false`,
		`"automountServiceAccountToken":false`,
		`"runAsNonRoot":true`,
		`"drop":["ALL"]`,
		// Whose it is, for a platform engineer looking at a busy node.
		`"keera.dev/org":"org_1"`,
		`"keera.dev/owner":"dev@example.ch"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the created object does not contain %s\n%s", want, body)
		}
	}

	// The root filesystem stays writable, so people can install packages.
	if strings.Contains(body, `"readOnlyRootFilesystem":true`) {
		t.Error("a read-only root would make the sandbox useless to work in")
	}
}

func TestCreateSortsTheEnvironment(t *testing.T) {
	// The same spec must give the same object, or the controller sees a
	// change on every reconcile.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	spec := Spec{
		Ref:     Ref{ID: "sbx_test0002", Name: "envtest"},
		Class:   policy.SandboxClass{Name: "c", Image: "i", Isolation: policy.IsolationStandard},
		Purpose: policy.PurposeAgent,
		Env:     map[string]string{"ZZZ": "1", "AAA": "2", "MMM": "3"},
	}
	obj := k.build(spec, "")
	env := obj.Spec.PodTemplate.Spec.Containers[0].Env
	for i := 1; i < len(env); i++ {
		if env[i-1].Name > env[i].Name {
			t.Fatalf("environment is not sorted: %v", env)
		}
	}
}

func TestCreateRefusesUndeliverableIsolation(t *testing.T) {
	// Refused before anything is created, never quietly downgraded.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{}) // no runtime mapping at all
	_, err := k.Create(context.Background(), Spec{
		Ref:   Ref{ID: "sbx_test0003", Name: "vm"},
		Class: policy.SandboxClass{Name: "vm", Image: "i", Isolation: policy.IsolationVM},
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "KEERA_SANDBOX_RUNTIME_VM") {
		t.Errorf("the refusal should say which setting to fix: %v", err)
	}
	for _, r := range f.seen {
		if r.Method == http.MethodPost {
			t.Error("nothing should have been created")
		}
	}
}

func TestCapabilitiesFollowTheRuntimeMapping(t *testing.T) {
	// Derived from the mapping, so no tier is claimed without a runtime.
	f := newFakeAPI(t)
	cases := []struct {
		runtimes map[policy.Isolation]string
		want     policy.Isolation
	}{
		{nil, policy.IsolationStandard},
		{map[policy.Isolation]string{policy.IsolationIsolated: "gvisor"}, policy.IsolationIsolated},
		{map[policy.Isolation]string{
			policy.IsolationIsolated: "gvisor",
			policy.IsolationVM:       "kata-qemu",
		}, policy.IsolationVM},
	}
	for _, tc := range cases {
		k := f.driver(t, KubernetesOptions{Runtimes: tc.runtimes})
		if got := k.Capabilities().Isolation; got != tc.want {
			t.Errorf("with %v the strongest tier is %s, want %s", tc.runtimes, got, tc.want)
		}
	}
}

func TestStatusCollapsesConditions(t *testing.T) {
	ref := Ref{ID: "sbx_test0004", Name: "st"}
	cases := []struct {
		name   string
		obj    string
		state  policy.SandboxState
		detail string
	}{
		{
			"ready",
			`{"spec":{"operatingMode":"Running"},"status":{"podIPs":["10.1.2.3"],
			  "nodeName":"node-a","conditions":[{"type":"Ready","status":"True",
			  "reason":"DependenciesReady"}]}}`,
			policy.SandboxReady, "",
		},
		{
			// Expired outranks everything.
			"expired",
			`{"status":{"conditions":[{"type":"Ready","status":"False",
			  "reason":"SandboxExpired"}]}}`,
			policy.SandboxExpired, "its lifetime ran out",
		},
		{
			// Finished outranks Suspended: failing while suspending is a failure.
			"failed while suspending",
			`{"spec":{"operatingMode":"Suspended"},"status":{"conditions":[
			  {"type":"Finished","status":"True","reason":"PodFailed",
			   "message":"OOMKilled"}]}}`,
			policy.SandboxFailed, "OOMKilled",
		},
		{
			"suspended",
			`{"spec":{"operatingMode":"Suspended"},"status":{"conditions":[
			  {"type":"Ready","status":"False","reason":"SandboxSuspended"}]}}`,
			policy.SandboxSuspended, "suspended; its volume is kept",
		},
		{
			// The scheduler's own message, verbatim.
			"unschedulable",
			`{"status":{"conditions":[
			  {"type":"Ready","status":"False","reason":"DependenciesNotReady"},
			  {"type":"PodScheduled","status":"False","reason":"Unschedulable",
			   "message":"0/6 nodes are available: 6 Insufficient memory."}]}}`,
			policy.SandboxPending, "0/6 nodes are available: 6 Insufficient memory.",
		},
		{
			"nothing yet",
			`{"status":{}}`,
			policy.SandboxPending, "accepted; waiting for the controller",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			k := f.driver(t, KubernetesOptions{})
			f.put(sandboxesPath+objectName(ref), tc.obj)
			st, err := k.Status(context.Background(), ref)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if st.State != tc.state {
				t.Errorf("state = %s, want %s", st.State, tc.state)
			}
			if tc.detail != "" && st.Detail != tc.detail {
				t.Errorf("detail = %q, want %q", st.Detail, tc.detail)
			}
		})
	}
}

func TestStatusNotFound(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	_, err := k.Status(context.Background(), Ref{ID: "sbx_gone", Name: "gone"})
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTerminateIsIdempotent(t *testing.T) {
	// A user's terminate and the sweep race, and the loser must not fail.
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	if err := k.Terminate(context.Background(), Ref{ID: "sbx_gone", Name: "gone"}); err != nil {
		t.Fatalf("terminating a sandbox that is already gone should succeed: %v", err)
	}
}

func TestSuspendResumePatchTheMode(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	ref := Ref{ID: "sbx_test0005", Name: "sr"}
	path := sandboxesPath + objectName(ref)
	f.put(path, `{"spec":{"operatingMode":"Running"}}`)

	if err := k.Suspend(context.Background(), ref); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := k.Resume(context.Background(), ref); err != nil {
		t.Fatalf("resume: %v", err)
	}

	var modes []string
	for _, r := range f.seen {
		if r.Method != http.MethodPatch {
			continue
		}
		spec, _ := r.Body["spec"].(map[string]any)
		if m, ok := spec["operatingMode"].(string); ok {
			modes = append(modes, m)
		}
	}
	if len(modes) != 2 || modes[0] != "Suspended" || modes[1] != "Running" {
		t.Errorf("patched modes = %v, want [Suspended Running]", modes)
	}
}

func TestExtendMovesShutdownTime(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	ref := Ref{ID: "sbx_test0006", Name: "ext"}
	path := sandboxesPath + objectName(ref)
	f.put(path, `{"spec":{}}`)

	until := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	if err := k.Extend(context.Background(), ref, until); err != nil {
		t.Fatalf("extend: %v", err)
	}
	last := f.seen[len(f.seen)-1]
	spec, _ := last.Body["spec"].(map[string]any)
	if got := spec["shutdownTime"]; got != until.Format(time.RFC3339) {
		t.Errorf("shutdownTime = %v, want %s", got, until.Format(time.RFC3339))
	}
}

func TestAgentSandboxIsTerminatedOnExpiry(t *testing.T) {
	// An agent's task is over; an engineer's volume holds work in progress.
	if shutdownPolicy(policy.PurposeAgent) != "Delete" {
		t.Error("an expired agent sandbox should be terminated")
	}
	if shutdownPolicy(policy.PurposeEngineer) != "Retain" {
		t.Error("an expired engineer's sandbox should keep its volume")
	}
}

func TestValidPort(t *testing.T) {
	if !ValidPort(PortSSH) {
		t.Error("the shell port has to be reachable")
	}
	if !ValidPort(3000) {
		t.Error("an unprivileged port is a dev server somebody wants forwarded")
	}
	for _, bad := range []int{0, 22, 80, 443, 1023, 70000, -1} {
		if ValidPort(bad) {
			t.Errorf("port %d should not be reachable through the attach surface", bad)
		}
	}
}

// A sandbox runs unprivileged and cannot bind a privileged port. sshd on 22
// once failed this way, and the sandbox looked like it never started.
func TestTheShellPortIsUnprivileged(t *testing.T) {
	if PortSSH < 1024 {
		t.Fatalf("PortSSH = %d; a sandbox cannot bind a privileged port", PortSSH)
	}
}

func TestMibQuantity(t *testing.T) {
	// Gi where it divides evenly, as people write it in the catalogue.
	cases := map[int]string{1024: "1Gi", 16384: "16Gi", 512: "512Mi", 1536: "1536Mi"}
	for in, want := range cases {
		if got := mibQuantity(in); got != want {
			t.Errorf("mibQuantity(%d) = %q, want %q", in, got, want)
		}
	}
}
