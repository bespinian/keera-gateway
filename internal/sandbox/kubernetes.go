package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"sort"
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
// leaves no sandboxes behind.

const (
	sandboxAPIGroup   = "agents.x-k8s.io"
	sandboxAPIVersion = "v1beta1"
	sandboxAPIPath    = "/apis/" + sandboxAPIGroup + "/" + sandboxAPIVersion
)

// The labels on every object this driver writes. They let a platform engineer
// see whose sandbox is on a node without a login to the panel.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelName      = "app.kubernetes.io/name"
	labelSandboxID = "keera.dev/sandbox-id"
	labelOrg       = "keera.dev/org"
	labelTeam      = "keera.dev/team"
	labelClass     = "keera.dev/class"
	labelPurpose   = "keera.dev/purpose"
	// annotationOwner is an email address. It is an annotation because a
	// label value cannot hold an @, and an address should not be selectable.
	annotationOwner = "keera.dev/owner"
)

// KubernetesOptions configures the driver.
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
	// ExtraLabels are stamped on every object, for a cluster's policy engine
	// or cost allocation.
	ExtraLabels map[string]string
	Log         *slog.Logger
}

// Kubernetes is the driver.
type Kubernetes struct {
	c    *kubeClient
	opts KubernetesOptions
	log  *slog.Logger
}

// NewKubernetes builds the driver and checks it can reach the API server.
//
// The check lists sandboxes rather than asking for the version, because that
// proves the API server is there, the Sandbox CRD is installed and this service
// account may use it - at start, not at the first request.
func NewKubernetes(ctx context.Context, opts KubernetesOptions) (*Kubernetes, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Namespace == "" {
		opts.Namespace = inClusterNamespace()
	}
	c, err := newKubeClient(opts.Kube)
	if err != nil {
		return nil, err
	}
	k := &Kubernetes{c: c, opts: opts, log: opts.Log}

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
	if opts.Kube.Insecure {
		k.log.Warn("sandbox: the Kubernetes API server's certificate is not being verified " +
			"(KEERA_SANDBOX_KUBE_INSECURE); this is for a development cluster and nothing else")
	}
	return k, nil
}

// Name returns "kubernetes".
func (k *Kubernetes) Name() string { return "kubernetes" }

// Capabilities reports what this driver does. The isolation tier comes from
// the runtime mapping, so the driver cannot claim a tier it cannot deliver.
func (k *Kubernetes) Capabilities() Capabilities {
	return Capabilities{
		Suspend: true, Isolation: strongestIsolation(k.opts.Runtimes),
		Warm: k.opts.Warm, Persistence: true,
	}
}

// collection is the path of the sandboxes in the namespace.
func (k *Kubernetes) collection() string {
	return sandboxAPIPath + "/namespaces/" + url.PathEscape(k.opts.Namespace) + "/sandboxes"
}

// object is the path of one Sandbox.
func (k *Kubernetes) object(ref Ref) string {
	return k.collection() + "/" + url.PathEscape(objectName(ref))
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
	suffix := ref.ID
	if i := strings.IndexByte(suffix, '_'); i >= 0 {
		suffix = suffix[i+1:]
	}
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return name + "-" + suffix
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
	RuntimeClassName             *string           `json:"runtimeClassName,omitempty"`
	ServiceAccountName           string            `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken"`
	SecurityContext              *kubePodSecurity  `json:"securityContext,omitempty"`
	Containers                   []kubeContainer   `json:"containers"`
	Volumes                      []kubeVolume      `json:"volumes,omitempty"`
	ImagePullSecrets             []kubeLocalRef    `json:"imagePullSecrets,omitempty"`
	NodeSelector                 map[string]string `json:"nodeSelector,omitempty"`
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
	ReadOnlyRootFilesystem   *bool     `json:"readOnlyRootFilesystem,omitempty"`
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
	Medium    string `json:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type kubeLocalRef struct {
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

/* ------------------------------------------------------------------- create */

// The home volume. It is one volume because everything that should survive a
// suspend - working copy, build cache, shell history - lives under home.
const (
	homeVolume = "home"
	homePath   = "/home/keera"
	// sandboxUID is the user a sandbox runs as. It is fixed rather than taken
	// from the image, so an image upgrade cannot lock a sandbox out of its
	// own home volume.
	sandboxUID = 1000
)

// Create writes one Sandbox object and returns without waiting for it to be
// ready. The panel and the CLI poll the row; a sandbox stuck in Pending shows
// the scheduler's own message.
func (k *Kubernetes) Create(ctx context.Context, spec Spec) (Status, error) {
	runtime, err := k.runtimeFor(spec.Class)
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

// runtimeFor maps a class's isolation tier to this cluster's RuntimeClass.
func (k *Kubernetes) runtimeFor(c policy.SandboxClass) (string, error) {
	name, ok := mappedRuntime(c, k.opts.Runtimes)
	if !ok {
		return "", fmt.Errorf("the sandbox class %q asks for %s isolation and this deployment "+
			"has no RuntimeClass mapped to it; set KEERA_SANDBOX_RUNTIME_%s to the name of "+
			"this cluster's %s RuntimeClass, or move the class to a tier it can deliver",
			c.Name, c.Isolation, strings.ToUpper(string(c.Isolation)), c.Isolation)
	}
	return name, nil
}

// labelsFor is the label set on every object for a sandbox, shared with the
// claim path so the two cannot drift.
func (k *Kubernetes) labelsFor(spec Spec) map[string]string {
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
	if spec.Team != "" {
		labels[labelTeam] = spec.Team
	}
	maps.Copy(labels, k.opts.ExtraLabels)
	return labels
}

func (k *Kubernetes) annotationsFor(spec Spec) map[string]string {
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
	labels := k.labelsFor(spec)

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
		pod.ImagePullSecrets = append(pod.ImagePullSecrets, kubeLocalRef{Name: name})
	}

	obj := &kubeSandbox{
		APIVersion: sandboxAPIGroup + "/" + sandboxAPIVersion,
		Kind:       "Sandbox",
		Metadata: kubeMeta{
			Name:        objectName(spec.Ref),
			Namespace:   k.opts.Namespace,
			Labels:      labels,
			Annotations: k.annotationsFor(spec),
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

// sandboxContainer is the one container in a sandbox pod.
func sandboxContainer(spec Spec) kubeContainer {
	return kubeContainer{
		Name:  "sandbox",
		Image: spec.Class.Image,
		Env:   envList(spec.Env),
		Ports: portList(spec.Ports),
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
			// The root filesystem stays writable: people install packages
			// and builds write to /usr/local. The isolation tier holds the
			// boundary.
		},
		// Ready when sshd answers. The image starts sshd last, after home is
		// set up and the repository checked out, so this also means setup is
		// done.
		ReadinessProbe: &kubeProbe{
			TCPSocket:           &kubeTCPProbe{Port: PortSSH},
			InitialDelaySeconds: 2,
			PeriodSeconds:       3,
			FailureThreshold:    40,
		},
	}
}

// shutdownPolicy decides what happens to the object when its time runs out.
//
// An agent's sandbox is deleted: the task is over, and its result is a
// branch. An engineer's is retained: its volume holds work in progress, the
// row goes to Expired, and resuming it is one click.
func shutdownPolicy(p policy.Purpose) string {
	if p == policy.PurposeAgent {
		return "Delete"
	}
	return "Retain"
}

/* ---------------------------------------------------------------- read back */

// Status reads one sandbox.
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
//
// The checks are in order of precedence. Expired comes first: a sandbox whose
// time is up is not "not ready yet". Finished beats Suspended: a pod that
// failed while suspending is a failure. Ready comes late, as the only good
// news.
func (k *Kubernetes) statusOf(ref Ref, obj *kubeSandbox) Status {
	st := Status{
		Ref:     ref,
		Node:    obj.Status.NodeName,
		Address: firstAddress(obj.Status.PodIPs, obj.Status.ServiceFQDN),
		Expires: parseExpiry(obj.Spec.ShutdownTime),
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
		// The process exited cleanly. Nothing is left to attach to, but the
		// detail says it exited rather than crashed.
		st.State, st.Detail = policy.SandboxFailed, "the sandbox process exited"
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

// firstAddress is where to dial a sandbox: its pod IP, or else its Service.
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

/* -------------------------------------------------------------- the changes */

// Suspend stops the sandbox's pod and keeps its volume. Like Resume it is a
// merge patch of one field, so two gateways doing it at once agree.
func (k *Kubernetes) Suspend(ctx context.Context, ref Ref) error {
	return k.setMode(ctx, ref, "Suspended")
}

// Resume starts a suspended sandbox again.
func (k *Kubernetes) Resume(ctx context.Context, ref Ref) error {
	return k.setMode(ctx, ref, "Running")
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
	return kubeNotFound(k.c.patch(ctx, path, patch, nil))
}

// Extend moves the shutdown time.
//
// For a claim it moves the claim's own lifetime: deleting the claim is what
// returns the sandbox, so the claim must not end before its Sandbox.
func (k *Kubernetes) Extend(ctx context.Context, ref Ref, until time.Time) error {
	path, patch := k.object(ref), map[string]any{
		"spec": map[string]any{"shutdownTime": formatExpiry(until)},
	}
	if ref.Claimed() {
		path = k.claimPath(ref)
		patch = map[string]any{
			"spec": map[string]any{
				"lifecycle": map[string]any{
					"shutdownTime": formatExpiry(until),
				},
			},
		}
	}
	return kubeNotFound(k.c.patch(ctx, path, patch, nil))
}

// Terminate removes the sandbox and, through owner references, its pod,
// Service and volume.
//
// A sandbox already gone is success: a user's terminate and the sweep's
// expiry race by nature, and the loser must not fail.
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

/* ------------------------------------------------------------------ dialing */

// Dial opens a connection to one port of a running sandbox.
//
// It dials the pod IP directly. The port-forward subresource would work from
// anywhere, but costs an upgrade through the API server per connection and
// needs pods/portforward, which reaches any pod in the namespace. Dialing
// directly needs the gateway in the same cluster and the sandbox namespace's
// NetworkPolicy to admit it, as the Helm chart sets up.
func (k *Kubernetes) Dial(ctx context.Context, ref Ref, port int) (net.Conn, error) {
	st, err := k.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	if st.State != policy.SandboxReady || st.Address == "" {
		return nil, fmt.Errorf("%w: it is %s", ErrNotReady, st.State)
	}
	return dialSandbox(ctx, ref, port, dialAddress(st.Address, port))
}

/* ------------------------------------------------------------------ helpers */

func envList(env map[string]string) []kubeEnv {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	// Sorted, so the same spec always gives the same pod template and the
	// controller sees no change.
	sort.Strings(names)
	out := make([]kubeEnv, 0, len(names))
	for _, n := range names {
		out = append(out, kubeEnv{Name: n, Value: env[n]})
	}
	return out
}

func portList(ports []int) []kubePort {
	out := make([]kubePort, 0, len(ports)+1)
	seen := map[int]bool{}
	for _, p := range append([]int{PortSSH}, ports...) {
		if seen[p] {
			continue
		}
		seen[p] = true
		name := fmt.Sprintf("p%d", p)
		if p == PortSSH {
			name = "ssh"
		}
		out = append(out, kubePort{Name: name, ContainerPort: p, Protocol: "TCP"})
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

// boundSandbox is the path of the Sandbox a claim is bound to. Only the mode
// changes need it.
//
// An unbound claim is ErrNotReady, not ErrNotFound: the claim exists and the
// pool has not answered yet, so the answer is "wait", not "gone".
func (k *Kubernetes) boundSandbox(ctx context.Context, ref Ref) (string, error) {
	var c sandboxClaim
	if err := k.c.get(ctx, k.claimPath(ref), &c); err != nil {
		return "", kubeNotFound(err)
	}
	if c.Status.Sandbox.Name == "" {
		return "", fmt.Errorf("%w: the claim has not been bound to a sandbox yet", ErrNotReady)
	}
	return k.collection() + "/" + url.PathEscape(c.Status.Sandbox.Name), nil
}
