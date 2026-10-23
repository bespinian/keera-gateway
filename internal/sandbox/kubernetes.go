package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The Kubernetes driver writes one Sandbox object per sandbox, and the
// agent-sandbox controller (sigs.k8s.io/agent-sandbox, agents.x-k8s.io/v1beta1)
// runs it.
//
// The controller gives us a stable identity, a volume that survives a suspend,
// and a scheduled end - without us owning a controller:
//
//	spec.shutdownTime    when it tears the sandbox down, even if the gateway is gone
//	spec.shutdownPolicy  whether the object survives that
//	spec.operatingMode   Running or Suspended - the compute, not the volume
//	volumeClaimTemplates the home directory, kept across a suspend
//	status.conditions    Ready, PodScheduled, Suspended, Finished
//
// Because the controller owns the expiry, a gateway that crashes for good
// leaves no sandboxes running.

const (
	sandboxAPIGroup   = "agents.x-k8s.io"
	sandboxAPIVersion = "v1beta1"
	sandboxAPIPath    = "/apis/" + sandboxAPIGroup + "/" + sandboxAPIVersion
)

// The labels on every object a driver writes, Kubernetes objects and podman
// containers alike. They let a platform engineer see whose sandbox it is
// without a login to the panel.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelName      = "app.kubernetes.io/name"
	labelSandboxID = "keera.dev/sandbox-id"
	labelOrg       = "keera.dev/org"
	labelProject   = "keera.dev/project"
	labelClass     = "keera.dev/class"
	labelPurpose   = "keera.dev/purpose"
	// annotationOwner is an email address. It is an annotation because a
	// label value cannot hold an @, and an address should not be selectable.
	annotationOwner = "keera.dev/owner"
)

type KubernetesOptions struct {
	Kube KubeConfig
	// Namespace is where sandboxes are created. Keep it separate from the
	// gateway's own: a sandbox's API key is in its pod spec, so anybody who can
	// read pods there can read every sandbox's key.
	Namespace string
	// Runtimes maps an isolation tier to this cluster's RuntimeClass names. A
	// tier with no entry is refused at creation, never downgraded.
	Runtimes map[policy.Isolation]string
	// StorageClass provisions the home volume. Empty uses the cluster default,
	// which may be too slow for build output.
	StorageClass string
	// ServiceAccount is what a sandbox pod runs as. Empty is the namespace's
	// default, with its token not mounted.
	ServiceAccount string
	// ImagePullSecrets are passed to the pod for a registry that needs one.
	ImagePullSecrets []string
	// Warm switches warm pools on. It needs the agent-sandbox warm-pool
	// extension (extensions.agents.x-k8s.io/v1beta1), so it is its own
	// setting: without the extension, every pool reconcile would fail.
	Warm bool
	Log  *slog.Logger
}

type Kubernetes struct {
	c    *kubeClient
	opts KubernetesOptions
}

// NewKubernetes builds the driver and checks it can reach the API server. It
// lists sandboxes rather than asking for the version, because that proves at
// start that the Sandbox CRD is installed and this service account may use it.
func NewKubernetes(ctx context.Context, opts KubernetesOptions) (*Kubernetes, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Namespace == "" {
		// Allowed, so an install that never set it still starts, but warned
		// about.
		opts.Namespace = inClusterNamespace()
		opts.Log.Warn("KEERA_SANDBOX_NAMESPACE is not set, so sandboxes run in the "+
			"gateway's namespace, where anyone who can read pods can read their API keys; "+
			"give them a namespace of their own", "namespace", opts.Namespace)
	}
	c, err := newKubeClient(opts.Kube)
	if err != nil {
		return nil, err
	}
	k := &Kubernetes{c: c, opts: opts}

	var probe struct {
		Items []struct{} `json:"items"`
	}
	if err := c.get(ctx, k.collection()+"?limit=1", &probe); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("the Sandbox API (%s/%s) is not installed on this cluster; "+
				"sandboxes need the agent-sandbox controller "+
				"(https://github.com/kubernetes-sigs/agent-sandbox): %w",
				sandboxAPIGroup, sandboxAPIVersion, err)
		}
		return nil, fmt.Errorf("reaching the Sandbox API in namespace %s: %w", opts.Namespace, err)
	}
	return k, nil
}

func (k *Kubernetes) Name() string { return "kubernetes" }

// Capabilities reports what this driver does. The isolation tier comes from
// the runtime mapping, so the driver cannot claim a tier it cannot deliver.
func (k *Kubernetes) Capabilities() Capabilities {
	return capabilitiesFor(k.opts.Runtimes, k.opts.Warm)
}

// collection is the path of the sandboxes in the namespace.
func (k *Kubernetes) collection() string {
	return sandboxAPIPath + "/namespaces/" + url.PathEscape(k.opts.Namespace) + "/sandboxes"
}

// object is the path of one Sandbox.
func (k *Kubernetes) object(ref Ref) string { return k.sandboxPath(objectName(ref)) }

func (k *Kubernetes) sandboxPath(name string) string {
	return k.collection() + "/" + url.PathEscape(name)
}

// objectName is the Kubernetes name for a sandbox: the developer's name plus
// the tail of the id.
//
// The name alone is not unique across organisations, and the id alone is
// unreadable in `kubectl get pods`. It depends only on the Ref, so a restarted
// gateway finds what it left running.
func objectName(ref Ref) string {
	name := ref.Name
	if name == "" {
		name = "sandbox"
	}
	return name + "-" + shortID(ref.ID)
}

// shortID is the end of an id without its prefix, which is enough to tell
// objects apart in a name the cluster limits in length.
func shortID(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return id
}

/* --------------------------------------------------------------- the object */

// kubeSandbox is the part of the Sandbox resource this driver reads or
// writes. The rest belongs to the controller.
type kubeSandbox struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   kubeMeta          `json:"metadata"`
	Spec       kubeSpec          `json:"spec"`
	Status     kubeSandboxStatus `json:"status"`
}

type kubeMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type kubeSpec struct {
	ShutdownTime   string        `json:"shutdownTime,omitempty"`
	ShutdownPolicy string        `json:"shutdownPolicy,omitempty"`
	OperatingMode  string        `json:"operatingMode,omitempty"`
	Service        *bool         `json:"service,omitempty"`
	PodTemplate    kubePodTmpl   `json:"podTemplate"`
	VolumeClaims   []kubePVCTmpl `json:"volumeClaimTemplates,omitempty"`
}

type kubePodTmpl struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     kubePod  `json:"spec"`
}

type kubePod struct {
	RuntimeClassName             *string          `json:"runtimeClassName,omitempty"`
	ServiceAccountName           string           `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool            `json:"automountServiceAccountToken"`
	SecurityContext              *kubePodSecurity `json:"securityContext,omitempty"`
	Containers                   []kubeContainer  `json:"containers"`
	Volumes                      []kubeVolume     `json:"volumes,omitempty"`
	ImagePullSecrets             []kubeNameRef    `json:"imagePullSecrets,omitempty"`
	// RestartPolicy is Never, so a sandbox whose process died shows as
	// finished instead of quietly restarting into an empty shell.
	RestartPolicy string `json:"restartPolicy,omitempty"`
}

type kubePodSecurity struct {
	RunAsNonRoot   *bool        `json:"runAsNonRoot,omitempty"`
	RunAsUser      *int64       `json:"runAsUser,omitempty"`
	RunAsGroup     *int64       `json:"runAsGroup,omitempty"`
	FSGroup        *int64       `json:"fsGroup,omitempty"`
	SeccompProfile *kubeSeccomp `json:"seccompProfile,omitempty"`
}

type kubeSeccomp struct {
	Type string `json:"type"`
}

type kubeContainer struct {
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	Env             []kubeEnv        `json:"env,omitempty"`
	Ports           []kubePort       `json:"ports,omitempty"`
	Resources       kubeResources    `json:"resources"`
	VolumeMounts    []kubeMount      `json:"volumeMounts,omitempty"`
	SecurityContext *kubeCtrSecurity `json:"securityContext,omitempty"`
	ReadinessProbe  *kubeProbe       `json:"readinessProbe,omitempty"`
}

type kubeCtrSecurity struct {
	AllowPrivilegeEscalation *bool     `json:"allowPrivilegeEscalation"`
	Capabilities             *kubeCaps `json:"capabilities,omitempty"`
}

type kubeCaps struct {
	Drop []string `json:"drop,omitempty"`
	Add  []string `json:"add,omitempty"`
}

type kubeProbe struct {
	TCPSocket           *kubeTCPProbe `json:"tcpSocket,omitempty"`
	InitialDelaySeconds int           `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int           `json:"periodSeconds,omitempty"`
	FailureThreshold    int           `json:"failureThreshold,omitempty"`
}

type kubeTCPProbe struct {
	Port int `json:"port"`
}

type kubeEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type kubePort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type kubeResources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type kubeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type kubeVolume struct {
	Name     string        `json:"name"`
	EmptyDir *kubeEmptyDir `json:"emptyDir,omitempty"`
}

type kubeEmptyDir struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type kubeNameRef struct {
	Name string `json:"name"`
}

type kubePVCTmpl struct {
	Metadata kubeMeta    `json:"metadata"`
	Spec     kubePVCSpec `json:"spec"`
}

type kubePVCSpec struct {
	AccessModes      []string      `json:"accessModes"`
	StorageClassName *string       `json:"storageClassName,omitempty"`
	Resources        kubeResources `json:"resources"`
}

// kubeSandboxStatus is the observed half of a Sandbox.
type kubeSandboxStatus struct {
	ServiceFQDN string          `json:"serviceFQDN,omitempty"`
	Conditions  []kubeCondition `json:"conditions,omitempty"`
	PodIPs      []string        `json:"podIPs,omitempty"`
	NodeName    string          `json:"nodeName,omitempty"`
}

type kubeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

func conditionOf(conds []kubeCondition, typ string) (kubeCondition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return kubeCondition{}, false
}

// Create writes one Sandbox object and returns without waiting for it to be
// ready. The panel and the CLI poll the row.
func (k *Kubernetes) Create(ctx context.Context, spec Spec) (Status, error) {
	runtime, err := runtimeFor(spec.Class, k.opts.Runtimes, k.Name())
	if err != nil {
		return Status{}, err
	}
	// The manager set spec.Backing, so the row and the object agree on which
	// kind of object this is.
	if spec.Claimed() {
		return k.createClaimed(ctx, spec)
	}
	obj := k.build(spec, runtime)

	var created kubeSandbox
	err = k.c.post(ctx, k.collection(), obj, &created)
	if isConflict(err) {
		// Already there, which is what was asked for. This makes a retry after
		// a dropped connection harmless.
		return k.Status(ctx, spec.Ref)
	}
	if err != nil {
		return Status{}, err
	}
	return k.statusOf(spec.Ref, &created), nil
}

// labelsFor is the label set on every object for a sandbox, shared with the
// claim path and podman so they cannot drift.
func labelsFor(spec Spec) map[string]string {
	labels := map[string]string{
		labelManagedBy: "keera",
		labelName:      "keera-sandbox",
		labelSandboxID: spec.ID,
		labelClass:     spec.Class.Name,
		labelPurpose:   string(spec.Purpose),
	}
	if spec.Org != "" {
		labels[labelOrg] = spec.Org
	}
	if spec.Project != "" {
		labels[labelProject] = spec.Project
	}
	return labels
}

func annotationsFor(spec Spec) map[string]string {
	annotations := map[string]string{}
	if spec.Owner != "" {
		annotations[annotationOwner] = spec.Owner
	}
	return annotations
}

// volumeClaimsFor is the home volume claim, or nil for a class with no disk.
// Shared with the claim path.
func volumeClaimsFor(class policy.SandboxClass, storageClass string,
	labels map[string]string,
) []kubePVCTmpl {
	if class.Disk <= 0 {
		return nil
	}
	return []kubePVCTmpl{{
		Metadata: kubeMeta{Name: homeVolume, Labels: labels},
		Spec: kubePVCSpec{
			AccessModes:      []string{"ReadWriteOnce"},
			StorageClassName: nullableString(storageClass),
			Resources: kubeResources{
				Requests: map[string]string{"storage": mibQuantity(class.Disk)},
			},
		},
	}}
}

// build is the Sandbox object for a spec. Warm pool templates use it too, so
// warm and cold sandboxes are the same machine.
func (k *Kubernetes) build(spec Spec, runtimeClass string) *kubeSandbox {
	labels := labelsFor(spec)

	pod := kubePod{
		ServiceAccountName: k.opts.ServiceAccount,
		// The workload runs whatever a model wrote and has no business with
		// the API server. Set here so it holds however the sandbox was made.
		AutomountServiceAccountToken: new(false),
		SecurityContext: &kubePodSecurity{
			RunAsNonRoot:   new(true),
			RunAsUser:      new(int64(sandboxUID)),
			RunAsGroup:     new(int64(sandboxUID)),
			FSGroup:        new(int64(sandboxUID)),
			SeccompProfile: &kubeSeccomp{Type: "RuntimeDefault"},
		},
		Containers:    []kubeContainer{sandboxContainer(spec)},
		RestartPolicy: "Never",
	}
	if runtimeClass != "" {
		pod.RuntimeClassName = &runtimeClass
	}
	for _, name := range k.opts.ImagePullSecrets {
		pod.ImagePullSecrets = append(pod.ImagePullSecrets, kubeNameRef{Name: name})
	}

	obj := &kubeSandbox{
		APIVersion: sandboxAPIGroup + "/" + sandboxAPIVersion,
		Kind:       "Sandbox",
		Metadata: kubeMeta{
			Name:        objectName(spec.Ref),
			Namespace:   k.opts.Namespace,
			Labels:      labels,
			Annotations: annotationsFor(spec),
		},
		Spec: kubeSpec{
			OperatingMode: "Running",
			// A headless Service gives the sandbox a DNS name that resolves
			// inside the cluster. The driver itself dials the pod IP.
			Service:     new(true),
			PodTemplate: kubePodTmpl{Metadata: kubeMeta{Labels: labels}, Spec: pod},
		},
	}

	if !spec.Expires.IsZero() {
		obj.Spec.ShutdownTime = formatExpiry(spec.Expires)
		obj.Spec.ShutdownPolicy = shutdownPolicy(spec.Purpose)
	}

	// A class with no disk gets an emptyDir: there is somewhere to write, and
	// nothing survives a suspend.
	if claims := volumeClaimsFor(spec.Class, k.opts.StorageClass, labels); claims != nil {
		obj.Spec.VolumeClaims = claims
	} else {
		obj.Spec.PodTemplate.Spec.Volumes = []kubeVolume{{
			Name: homeVolume, EmptyDir: &kubeEmptyDir{SizeLimit: "8Gi"},
		}}
	}
	return obj
}

// podContainerName names the one container in a sandbox pod. A claim names it
// too when it hands over the environment.
const podContainerName = "sandbox"

func sandboxContainer(spec Spec) kubeContainer {
	c := kubeContainer{
		Name:  podContainerName,
		Image: spec.Class.Image,
		Env:   envList(spec.Env),
		// Only sshd is declared: every attach goes through it.
		Ports: []kubePort{{Name: "ssh", ContainerPort: PortSSH, Protocol: "TCP"}},
		Resources: kubeResources{
			// Requests equal limits, so every sandbox is Guaranteed: a noisy
			// neighbour cannot make it slow in the afternoon.
			Requests: resourceMap(spec.Class),
			Limits:   resourceMap(spec.Class),
		},
		VolumeMounts: []kubeMount{{Name: homeVolume, MountPath: homePath}},
		SecurityContext: &kubeCtrSecurity{
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &kubeCaps{Drop: []string{"ALL"}},
			// The root filesystem stays writable wherever the sandbox user
			// has rights, such as home and /tmp. The isolation tier holds
			// the boundary.
		},
		// Ready when sshd answers, which the entrypoint makes mean setup is
		// done.
		ReadinessProbe: &kubeProbe{
			TCPSocket:           &kubeTCPProbe{Port: PortSSH},
			InitialDelaySeconds: 2,
			PeriodSeconds:       3,
			FailureThreshold:    40,
		},
	}
	// An agent's sandbox runs no sshd, so it is ready once it has started.
	if spec.Purpose == policy.PurposeAgent {
		c.ReadinessProbe = nil
	}
	return c
}

func formatExpiry(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// shutdownPolicy decides what happens to the object when its time runs out.
// An agent's is deleted: the task is over, and its result is a branch. An
// engineer's is retained: its volume holds work in progress.
func shutdownPolicy(p policy.Purpose) string {
	if p == policy.PurposeAgent {
		return "Delete"
	}
	return "Retain"
}

func (k *Kubernetes) Status(ctx context.Context, ref Ref) (Status, error) {
	if ref.Claimed() {
		return k.claimStatus(ctx, ref)
	}
	var obj kubeSandbox
	if err := k.c.get(ctx, k.object(ref), &obj); err != nil {
		return Status{}, kubeNotFound(err)
	}
	return k.statusOf(ref, &obj), nil
}

// statusOf collapses the controller's mode and conditions into one state.
// The cases are in order of precedence: a sandbox whose time is up is not
// "not ready yet", and a pod that failed while suspending is a failure.
func (k *Kubernetes) statusOf(ref Ref, obj *kubeSandbox) Status {
	st := Status{
		Ref:     ref,
		Node:    obj.Status.NodeName,
		Address: firstAddress(obj.Status.PodIPs, obj.Status.ServiceFQDN),
	}

	conds := obj.Status.Conditions
	ready, hasReady := conditionOf(conds, "Ready")
	finished, hasFinished := conditionOf(conds, "Finished")
	scheduled, hasScheduled := conditionOf(conds, "PodScheduled")

	switch {
	case hasReady && ready.Reason == "SandboxExpired":
		st.State, st.Detail = policy.SandboxExpired, "its lifetime ran out"
	case hasFinished && finished.Status == "True" && finished.Reason == "PodFailed":
		st.State, st.Detail = policy.SandboxFailed, detailOf(finished, "the sandbox process failed")
	case hasFinished && finished.Status == "True":
		// Nothing is left to attach to, but the detail says it exited rather
		// than crashed.
		st.State, st.Detail = policy.SandboxFailed, "the sandbox process exited"
		st.Exited = true
	case obj.Spec.OperatingMode == "Suspended":
		st.State, st.Detail = policy.SandboxSuspended, "suspended; its volume is kept"
	case hasReady && ready.Status == "True":
		st.State = policy.SandboxReady
	case hasScheduled && scheduled.Status == "False":
		// The scheduler's message, verbatim: it says why the sandbox is still
		// starting better than anything we could write.
		st.State, st.Detail = policy.SandboxPending, detailOf(scheduled, "waiting to be scheduled")
	case hasReady:
		st.State, st.Detail = policy.SandboxPending, detailOf(ready, "starting")
	default:
		st.State, st.Detail = policy.SandboxPending, "accepted; waiting for the controller"
	}
	return st
}

func firstAddress(podIPs []string, serviceFQDN string) string {
	if len(podIPs) > 0 {
		return podIPs[0]
	}
	return serviceFQDN
}

func detailOf(c kubeCondition, fallback string) string {
	switch {
	case c.Message != "":
		return c.Message
	case c.Reason != "":
		return c.Reason
	default:
		return fallback
	}
}

// Suspend stops the sandbox's pod and keeps its volume. Like Resume it is a
// merge patch of one field, so two gateways doing it at once agree.
func (k *Kubernetes) Suspend(ctx context.Context, ref Ref) error {
	return k.setMode(ctx, ref, "Suspended")
}

func (k *Kubernetes) Resume(ctx context.Context, ref Ref) error {
	return k.setMode(ctx, ref, "Running")
}

// Revive gives an expired sandbox its new environment and end, and starts it
// on the volume it kept. The container is sent whole, because a merge patch
// replaces a list.
func (k *Kubernetes) Revive(ctx context.Context, spec Spec) error {
	until := formatExpiry(spec.Expires)
	if spec.Claimed() {
		// A claim carries its environment itself, and the bound Sandbox runs.
		patch := map[string]any{"spec": map[string]any{
			"env":       claimEnvList(spec.Env),
			"lifecycle": map[string]any{"shutdownTime": until},
		}}
		if err := kubeNotFound(k.c.patch(ctx, k.claimPath(spec.Ref), patch)); err != nil {
			return err
		}
		return k.setMode(ctx, spec.Ref, "Running")
	}
	patch := map[string]any{"spec": map[string]any{
		"operatingMode": "Running",
		"shutdownTime":  until,
		"podTemplate": map[string]any{"spec": map[string]any{
			"containers": []kubeContainer{sandboxContainer(spec)},
		}},
	}}
	return kubeNotFound(k.c.patch(ctx, k.object(spec.Ref), patch))
}

func (k *Kubernetes) setMode(ctx context.Context, ref Ref, mode string) error {
	// A claim has no operatingMode, so a claimed sandbox is patched through
	// the Sandbox it is bound to.
	path := k.object(ref)
	if ref.Claimed() {
		bound, err := k.boundSandbox(ctx, ref)
		if err != nil {
			return err
		}
		path = bound
	}
	patch := map[string]any{"spec": map[string]any{"operatingMode": mode}}
	return kubeNotFound(k.c.patch(ctx, path, patch))
}

// Extend moves the shutdown time. For a claim it moves the claim's own
// lifetime: deleting the claim is what returns the sandbox, so the claim must
// not end before its Sandbox.
func (k *Kubernetes) Extend(ctx context.Context, ref Ref, until time.Time) error {
	end := formatExpiry(until)
	if ref.Claimed() {
		patch := map[string]any{"spec": map[string]any{
			"lifecycle": map[string]any{"shutdownTime": end},
		}}
		return kubeNotFound(k.c.patch(ctx, k.claimPath(ref), patch))
	}
	patch := map[string]any{"spec": map[string]any{"shutdownTime": end}}
	return kubeNotFound(k.c.patch(ctx, k.object(ref), patch))
}

// Terminate removes the sandbox and, through owner references, its pod,
// Service and volume. A sandbox already gone is success: a user's terminate
// and the sweep's expiry race, and the loser must not fail.
func (k *Kubernetes) Terminate(ctx context.Context, ref Ref) error {
	// For a pooled sandbox, delete the claim. Deleting the bound Sandbox would
	// drain the pool and leave the claim pointing at nothing.
	path := k.object(ref)
	if ref.Claimed() {
		path = k.claimPath(ref)
	}
	if err := k.c.delete(ctx, path); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// Dial connects to the pod IP directly. The port-forward subresource would
// work from anywhere, but costs an upgrade through the API server per
// connection and needs pods/portforward, which reaches any pod in the
// namespace. Dialing directly needs the gateway in the same cluster and the
// sandbox namespace's NetworkPolicy to admit it, as the Helm chart sets up.
func (k *Kubernetes) Dial(ctx context.Context, ref Ref, port int) (net.Conn, error) {
	st, err := k.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	if st.State != policy.SandboxReady || st.Address == "" {
		return nil, fmt.Errorf("%w: it is %s", ErrNotReady, st.State)
	}
	return dialSandbox(ctx, ref, port, net.JoinHostPort(st.Address, strconv.Itoa(port)))
}

func envList(env map[string]string) []kubeEnv {
	if len(env) == 0 {
		return nil
	}
	out := make([]kubeEnv, 0, len(env))
	// Sorted, so the same spec always gives the same pod template and the
	// controller sees no change.
	for _, n := range slices.Sorted(maps.Keys(env)) {
		out = append(out, kubeEnv{Name: n, Value: env[n]})
	}
	return out
}

func resourceMap(c policy.SandboxClass) map[string]string {
	m := map[string]string{}
	if c.CPU > 0 {
		m["cpu"] = fmt.Sprintf("%dm", c.CPU)
	}
	if c.Memory > 0 {
		m["memory"] = mibQuantity(c.Memory)
	}
	return m
}

// mibQuantity renders MiB as a Kubernetes quantity, in Gi where it divides
// evenly, so it reads like the catalogue ("16Gi", not "16384Mi").
func mibQuantity(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGi", mib/1024)
	}
	return fmt.Sprintf("%dMi", mib)
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// boundSandbox is the path of the Sandbox a claim is bound to. An unbound
// claim is ErrNotReady, not ErrNotFound: the pool has not answered yet, so the
// answer is "wait", not "gone".
func (k *Kubernetes) boundSandbox(ctx context.Context, ref Ref) (string, error) {
	c, err := k.claim(ctx, ref)
	if err != nil {
		return "", err
	}
	if c.Status.Sandbox.Name == "" {
		return "", fmt.Errorf("%w: the claim has not been bound to a sandbox yet", ErrNotReady)
	}
	return k.sandboxPath(c.Status.Sandbox.Name), nil
}
