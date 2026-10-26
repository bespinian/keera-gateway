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
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The Kubernetes driver writes one Sandbox object per sandbox, and the
// agent-sandbox controller (sigs.k8s.io/agent-sandbox) runs it, so Keera needs
// no controller of its own:
//
//	spec.shutdownTime    when it tears the sandbox down, even if the gateway is gone
//	spec.shutdownPolicy  whether the object survives that
//	spec.operatingMode   Running or Suspended - the compute, not the volume
//	volumeClaimTemplates the home directory, kept across a suspend
//	status.conditions    Ready, PodScheduled, Suspended, Finished
//
// The controller owns the expiry, so a gateway that dies for good leaves no
// sandboxes running.

const (
	sandboxAPIGroup   = "agents.x-k8s.io"
	sandboxAPIVersion = "v1beta1"
	sandboxAPIPath    = "/apis/" + sandboxAPIGroup + "/" + sandboxAPIVersion
)

type KubernetesOptions struct {
	Kube KubeConfig
	// Namespace must be separate from the gateway's: anybody who can read pods
	// there can read every sandbox's API key.
	Namespace string
	// Runtimes maps isolation tiers to RuntimeClass names. A tier with no
	// entry is refused, never downgraded.
	Runtimes map[policy.Isolation]string
	// StorageClass empty uses the cluster default, which may be slow for
	// builds.
	StorageClass string
	// ServiceAccount empty is the namespace's default, with no token mounted.
	ServiceAccount   string
	ImagePullSecrets []string
	// Warm needs the warm-pool extension (extensions.agents.x-k8s.io/v1beta1),
	// so it is a setting of its own.
	Warm bool
	Log  *slog.Logger
}

type Kubernetes struct {
	c    *kubeClient
	opts KubernetesOptions
}

// NewKubernetes lists sandboxes as a check, which proves the Sandbox CRD is
// installed and the service account may use it.
func NewKubernetes(ctx context.Context, opts KubernetesOptions) (*Kubernetes, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Namespace == "" {
		// Allowed so an install without it still starts, but warned about.
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

func (k *Kubernetes) Capabilities() Capabilities {
	return capabilitiesFor(k.opts.Runtimes, k.opts.Warm)
}

func (k *Kubernetes) collection() string {
	return sandboxAPIPath + "/namespaces/" + url.PathEscape(k.opts.Namespace) + "/sandboxes"
}

func (k *Kubernetes) object(ref Ref) string { return k.sandboxPath(objectName(ref)) }

func (k *Kubernetes) sandboxPath(name string) string {
	return k.collection() + "/" + url.PathEscape(name)
}

// kubeSandbox is the part of the Sandbox resource this driver uses.
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
	// RestartPolicy is Never, so a dead process shows as finished instead of
	// quietly restarting.
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

// Create does not wait for the sandbox to be ready. The panel and CLI poll.
func (k *Kubernetes) Create(ctx context.Context, spec Spec) (Status, error) {
	runtime, err := runtimeFor(spec.Class, k.opts.Runtimes, k.Name())
	if err != nil {
		return Status{}, err
	}
	// The manager set spec.Backing, so the row and the object agree.
	if spec.Claimed() {
		return k.createClaimed(ctx, spec)
	}
	obj := k.build(spec, runtime)

	var created kubeSandbox
	err = k.c.post(ctx, k.collection(), obj, &created)
	if isConflict(err) {
		// Already there, so a retry after a dropped connection is harmless.
		return k.Status(ctx, spec.Ref)
	}
	if err != nil {
		return Status{}, err
	}
	return k.statusOf(spec.Ref, &created), nil
}

// volumeClaimsFor is nil for a class with no disk.
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

// build is used by warm pool templates too, so warm and cold sandboxes match.
func (k *Kubernetes) build(spec Spec, runtimeClass string) *kubeSandbox {
	labels := labelsFor(spec)

	pod := kubePod{
		ServiceAccountName: k.opts.ServiceAccount,
		// The workload runs whatever a model wrote and has no business with
		// the API server.
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
			// A DNS name inside the cluster. The driver dials the pod IP.
			Service:     new(true),
			PodTemplate: kubePodTmpl{Metadata: kubeMeta{Labels: labels}, Spec: pod},
		},
	}

	if !spec.Expires.IsZero() {
		obj.Spec.ShutdownTime = formatExpiry(spec.Expires)
		obj.Spec.ShutdownPolicy = shutdownPolicy(spec.Purpose)
	}

	// A class with no disk gets an emptyDir, which a suspend loses.
	if claims := volumeClaimsFor(spec.Class, k.opts.StorageClass, labels); claims != nil {
		obj.Spec.VolumeClaims = claims
	} else {
		obj.Spec.PodTemplate.Spec.Volumes = []kubeVolume{{
			Name: homeVolume, EmptyDir: &kubeEmptyDir{SizeLimit: "8Gi"},
		}}
	}
	return obj
}

// podContainerName names the one container in a sandbox pod.
const podContainerName = "sandbox"

func sandboxContainer(spec Spec) kubeContainer {
	c := kubeContainer{
		Name:  podContainerName,
		Image: spec.Class.Image,
		Env:   envList(spec.Env),
		// Every attach goes through sshd.
		Ports: []kubePort{{Name: "ssh", ContainerPort: PortSSH, Protocol: "TCP"}},
		Resources: kubeResources{
			// Requests equal limits, so a noisy neighbour cannot slow it.
			Requests: resourceMap(spec.Class),
			Limits:   resourceMap(spec.Class),
		},
		VolumeMounts: []kubeMount{{Name: homeVolume, MountPath: homePath}},
		SecurityContext: &kubeCtrSecurity{
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &kubeCaps{Drop: []string{"ALL"}},
			// The root filesystem stays writable; the isolation tier holds
			// the boundary.
		},
		// The entrypoint starts sshd only once setup is done.
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

// shutdownPolicy deletes an agent's sandbox when its time runs out, and keeps
// an engineer's, whose volume holds work in progress.
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

// statusOf collapses the controller's mode and conditions into one state. The
// cases are in order of precedence.
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
		// The detail says it exited rather than crashed.
		st.State, st.Detail = policy.SandboxFailed, "the sandbox process exited"
		st.Exited = true
	case obj.Spec.OperatingMode == "Suspended":
		st.State, st.Detail = policy.SandboxSuspended, "suspended; its volume is kept"
	case hasReady && ready.Status == "True":
		st.State = policy.SandboxReady
	case hasScheduled && scheduled.Status == "False":
		// The scheduler's message says best why it is still starting.
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

// Suspend, like Resume, patches one field, so two gateways doing it agree.
func (k *Kubernetes) Suspend(ctx context.Context, ref Ref) error {
	return k.setMode(ctx, ref, "Suspended")
}

func (k *Kubernetes) Resume(ctx context.Context, ref Ref) error {
	return k.setMode(ctx, ref, "Running")
}

// Revive sends the container whole, because a merge patch replaces a list.
func (k *Kubernetes) Revive(ctx context.Context, spec Spec) error {
	until := formatExpiry(spec.Expires)
	if spec.Claimed() {
		// A claim carries its own environment.
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
	// A claim has no operatingMode, so patch its bound Sandbox.
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

// Extend moves the shutdown time. For a claim it moves the claim's lifetime,
// which must not end before its Sandbox.
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
// Service and volume. One already gone is success: a user and the sweep may
// race.
func (k *Kubernetes) Terminate(ctx context.Context, ref Ref) error {
	// For a pooled sandbox, delete the claim, not the bound Sandbox.
	path := k.object(ref)
	if ref.Claimed() {
		path = k.claimPath(ref)
	}
	if err := k.c.delete(ctx, path); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// Dial connects to the pod IP directly. Port-forward would need
// pods/portforward, which reaches any pod in the namespace. This needs the
// gateway in the same cluster, admitted by the NetworkPolicy.
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
	// Sorted, so the same spec gives the same pod template.
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

// mibQuantity uses Gi where it divides evenly ("16Gi", not "16384Mi").
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
// claim is ErrNotReady, not ErrNotFound: wait, not gone.
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
