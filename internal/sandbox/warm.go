package sandbox

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Warm pools: sandboxes already started before anybody asks for one.
//
// A cold start takes seconds, and an image pull on a fresh node a minute - long
// enough that people stop using the command. So a few sandboxes per class are
// kept idle, and a request binds to one. The controller's extension
// (extensions.agents.x-k8s.io/v1beta1: SandboxTemplate, SandboxWarmPool,
// SandboxClaim) keeps the pool; this file writes those three objects.
//
// It is opt-in per deployment and per class, because every warm sandbox holds
// its class's full CPU and memory all the time.

const (
	warmAPIGroup   = "extensions.agents.x-k8s.io"
	warmAPIVersion = "v1beta1"
	warmAPIPath    = "/apis/" + warmAPIGroup + "/" + warmAPIVersion
)

// Backing says which object a sandbox is made of.
//
// A claim and a Sandbox are addressed differently, and every call needs to
// know which without an extra round trip. It is stored on the row, so turning
// warm pools off does not orphan existing claims.
type Backing string

const (
	// BackingSandbox is a Sandbox object the driver created directly. It is
	// the cold path, and the only one podman has.
	BackingSandbox Backing = "sandbox"
	// BackingClaim is a SandboxClaim bound to a sandbox from a warm pool.
	BackingClaim Backing = "claim"
)

/* ---------------------------------------------------------------- the pool */

// warmTemplate is the SandboxTemplate for one class.
type warmTemplate struct {
	APIVersion string           `json:"apiVersion,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	Metadata   kubeMeta         `json:"metadata"`
	Spec       warmTemplateSpec `json:"spec"`
}

type warmTemplateSpec struct {
	// The blueprint, inlined as the CRD inlines it.
	PodTemplate  kubePodTmpl   `json:"podTemplate"`
	VolumeClaims []kubePVCTmpl `json:"volumeClaimTemplates,omitempty"`
	Service      *bool         `json:"service,omitempty"`
	// EnvVarsInjectionPolicy must be Overrides. A warm sandbox starts before
	// anyone asks for it, so its API key and session arrive on the claim, and
	// this lets the controller apply them.
	EnvVarsInjectionPolicy string `json:"envVarsInjectionPolicy,omitempty"`
	// VolumeClaimTemplatesPolicy is Overrides too: each sandbox's home volume
	// is its own, stated on the claim.
	VolumeClaimTemplatesPolicy string `json:"volumeClaimTemplatesPolicy,omitempty"`
	// NetworkPolicyManagement is Unmanaged: the deployment's own policy over
	// the namespace is the one place egress rules live.
	NetworkPolicyManagement string `json:"networkPolicyManagement,omitempty"`
}

type warmPool struct {
	APIVersion string       `json:"apiVersion,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Metadata   kubeMeta     `json:"metadata"`
	Spec       warmPoolSpec `json:"spec"`
	Status     struct {
		Replicas      int32 `json:"replicas,omitempty"`
		ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	} `json:"status"`
}

type warmPoolSpec struct {
	Replicas    *int32          `json:"replicas,omitempty"`
	TemplateRef warmTemplateRef `json:"sandboxTemplateRef"`
	// UpdateStrategy is OnReplenish. Recreate would empty the pool whenever a
	// class changes. The cost is that some sandboxes run the old image for a
	// while; each row records the image it actually ran.
	UpdateStrategy *warmUpdateStrategy `json:"updateStrategy,omitempty"`
}

type warmTemplateRef struct {
	Name string `json:"name"`
}

type warmUpdateStrategy struct {
	Type string `json:"type,omitempty"`
}

/* --------------------------------------------------------------- the claim */

type sandboxClaim struct {
	APIVersion string           `json:"apiVersion,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	Metadata   kubeMeta         `json:"metadata"`
	Spec       sandboxClaimSpec `json:"spec"`
	Status     claimStatus      `json:"status"`
}

type sandboxClaimSpec struct {
	WarmPoolRef warmPoolRef     `json:"warmPoolRef"`
	Lifecycle   *claimLifecycle `json:"lifecycle,omitempty"`
	// AdditionalPodMetadata gives a pooled sandbox the labels saying whose it
	// is. Without it, all pool members look the same on a node.
	AdditionalPodMetadata kubeMeta      `json:"additionalPodMetadata"`
	Env                   []claimEnv    `json:"env,omitempty"`
	VolumeClaimTemplates  []kubePVCTmpl `json:"volumeClaimTemplates,omitempty"`
}

type warmPoolRef struct {
	Name string `json:"name"`
}

type claimLifecycle struct {
	ShutdownTime   string `json:"shutdownTime,omitempty"`
	ShutdownPolicy string `json:"shutdownPolicy,omitempty"`
}

type claimEnv struct {
	Name          string `json:"name"`
	Value         string `json:"value"`
	ContainerName string `json:"containerName,omitempty"`
}

type claimStatus struct {
	Conditions []kubeCondition `json:"conditions,omitempty"`
	Sandbox    struct {
		Name        string   `json:"name,omitempty"`
		PodIPs      []string `json:"podIPs,omitempty"`
		ServiceFQDN string   `json:"serviceFQDN,omitempty"`
	} `json:"sandbox"`
}

/* ------------------------------------------------------------------- paths */

// warmCollection is the path of one warm-pool resource type in the namespace.
func (k *Kubernetes) warmCollection(resource string) string {
	return warmAPIPath + "/namespaces/" + url.PathEscape(k.opts.Namespace) + "/" + resource
}

func (k *Kubernetes) templatePath(class string) string {
	return k.warmCollection("sandboxtemplates") + "/" + url.PathEscape(warmName(class))
}

func (k *Kubernetes) poolPath(class string) string {
	return k.warmCollection("sandboxwarmpools") + "/" + url.PathEscape(warmName(class))
}

func (k *Kubernetes) claimPath(ref Ref) string {
	return k.warmCollection("sandboxclaims") + "/" + url.PathEscape(objectName(ref))
}

// warmName is the name of a class's template and pool. The prefix avoids
// clashing with other objects in the namespace.
func warmName(class string) string { return "keera-" + class }

// poolMeta is the metadata on a class's template and pool.
func (k *Kubernetes) poolMeta(class string) kubeMeta {
	return kubeMeta{
		Name:      warmName(class),
		Namespace: k.opts.Namespace,
		Labels: map[string]string{
			labelManagedBy: "keera",
			labelClass:     class,
		},
	}
}

/* ----------------------------------------------------------- reconciling it */

// EnsurePool makes one class's template and warm pool match the catalogue.
//
// A class with warm: 0 has its pool deleted rather than scaled to zero, since
// an empty pool looks like a feature in use and holds nothing.
func (k *Kubernetes) EnsurePool(ctx context.Context, class policy.SandboxClass) error {
	if class.Warm <= 0 {
		return k.removePool(ctx, class.Name)
	}
	runtime, err := k.runtimeFor(class)
	if err != nil {
		return err
	}

	// The same builder as a cold sandbox, with no per-sandbox parts (no env,
	// owner or expiry), so warm and cold sandboxes stay the same machine.
	blueprint := k.build(Spec{
		Ref:   Ref{ID: "pool_" + class.Name, Name: class.Name},
		Class: class,
		// A pool member has no purpose until claimed. Engineer is the shape
		// with the volume, which cannot be added later.
		Purpose: policy.PurposeEngineer,
	}, runtime)

	tmpl := &warmTemplate{
		APIVersion: warmAPIGroup + "/" + warmAPIVersion,
		Kind:       "SandboxTemplate",
		Metadata:   k.poolMeta(class.Name),
		Spec: warmTemplateSpec{
			PodTemplate:                blueprint.Spec.PodTemplate,
			VolumeClaims:               blueprint.Spec.VolumeClaims,
			Service:                    blueprint.Spec.Service,
			EnvVarsInjectionPolicy:     "Overrides",
			VolumeClaimTemplatesPolicy: "Overrides",
			NetworkPolicyManagement:    "Unmanaged",
		},
	}
	if err := k.apply(ctx, k.templatePath(class.Name), tmpl); err != nil {
		return fmt.Errorf("sandbox template for class %s: %w", class.Name, err)
	}

	pool := &warmPool{
		APIVersion: warmAPIGroup + "/" + warmAPIVersion,
		Kind:       "SandboxWarmPool",
		Metadata:   k.poolMeta(class.Name),
		Spec: warmPoolSpec{
			Replicas:       new(int32(class.Warm)),
			TemplateRef:    warmTemplateRef{Name: warmName(class.Name)},
			UpdateStrategy: &warmUpdateStrategy{Type: "OnReplenish"},
		},
	}
	if err := k.apply(ctx, k.poolPath(class.Name), pool); err != nil {
		return fmt.Errorf("warm pool for class %s: %w", class.Name, err)
	}
	return nil
}

func (k *Kubernetes) removePool(ctx context.Context, class string) error {
	for _, path := range []string{k.poolPath(class), k.templatePath(class)} {
		if err := k.c.delete(ctx, path); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// PrunePools deletes the template and pool of every class not in keep.
// EnsurePool never visits a class that left the catalogue, and its warm
// sandboxes would hold their CPU and memory for ever.
func (k *Kubernetes) PrunePools(ctx context.Context, keep map[string]bool) error {
	selector := "?labelSelector=" + url.QueryEscape(labelManagedBy+"=keera")
	orphans := map[string]bool{}
	for _, resource := range []string{"sandboxwarmpools", "sandboxtemplates"} {
		var list struct {
			Items []struct {
				Metadata kubeMeta `json:"metadata"`
			} `json:"items"`
		}
		if err := k.c.get(ctx, k.warmCollection(resource)+selector, &list); err != nil {
			return err
		}
		for _, it := range list.Items {
			if class := it.Metadata.Labels[labelClass]; class != "" && !keep[class] {
				orphans[class] = true
			}
		}
	}
	for class := range orphans {
		if err := k.removePool(ctx, class); err != nil {
			return fmt.Errorf("removing the warm pool of class %s: %w", class, err)
		}
	}
	return nil
}

// apply creates an object, or merge-patches it if it already exists.
//
// A merge patch is enough because Keera is the only writer of these objects;
// server-side apply would need field managers and conflict handling.
func (k *Kubernetes) apply(ctx context.Context, path string, obj any) error {
	collection := path[:strings.LastIndexByte(path, '/')]
	err := k.c.post(ctx, collection, obj, nil)
	if isConflict(err) {
		return k.c.patch(ctx, path, obj, nil)
	}
	return err
}

/* --------------------------------------------------------- claiming one out */

// createClaimed asks for a sandbox from a warm pool.
//
// The result is a claim: the controller binds it to a pool member and names
// that member in the claim's status. Later calls address the claim where they
// can and the bound Sandbox where they must.
func (k *Kubernetes) createClaimed(ctx context.Context, spec Spec) (Status, error) {
	labels := k.labelsFor(spec)
	claim := &sandboxClaim{
		APIVersion: warmAPIGroup + "/" + warmAPIVersion,
		Kind:       "SandboxClaim",
		Metadata: kubeMeta{
			Name:        objectName(spec.Ref),
			Namespace:   k.opts.Namespace,
			Labels:      labels,
			Annotations: k.annotationsFor(spec),
		},
		Spec: sandboxClaimSpec{
			WarmPoolRef:           warmPoolRef{Name: warmName(spec.Class.Name)},
			AdditionalPodMetadata: kubeMeta{Labels: labels},
			Env:                   claimEnvList(spec.Env),
		},
	}
	if !spec.Expires.IsZero() {
		claim.Spec.Lifecycle = &claimLifecycle{
			ShutdownTime:   formatExpiry(spec.Expires),
			ShutdownPolicy: shutdownPolicy(spec.Purpose),
		}
	}
	if spec.Class.Disk > 0 {
		claim.Spec.VolumeClaimTemplates = volumeClaimsFor(spec.Class, k.opts.StorageClass, labels)
	}

	var created sandboxClaim
	err := k.c.post(ctx, k.warmCollection("sandboxclaims"), claim, &created)
	if isConflict(err) {
		return k.claimStatus(ctx, spec.Ref)
	}
	if err != nil {
		return Status{}, err
	}
	return statusOfClaim(spec.Ref, &created), nil
}

// claimStatus reads one claim back.
func (k *Kubernetes) claimStatus(ctx context.Context, ref Ref) (Status, error) {
	var c sandboxClaim
	if err := k.c.get(ctx, k.claimPath(ref), &c); err != nil {
		return Status{}, kubeNotFound(err)
	}
	return statusOfClaim(ref, &c), nil
}

// statusOfClaim collapses a claim's conditions like statusOf does a
// Sandbox's.
//
// A claim can also be accepted but not yet bound. Its detail says so, because
// "waiting for a free sandbox in the pool" means the pool is too small, while
// waiting for a node means the cluster is.
func statusOfClaim(ref Ref, c *sandboxClaim) Status {
	st := Status{
		Ref:     ref,
		Address: firstAddress(c.Status.Sandbox.PodIPs, c.Status.Sandbox.ServiceFQDN),
	}
	if c.Spec.Lifecycle != nil {
		st.Expires = parseExpiry(c.Spec.Lifecycle.ShutdownTime)
	}

	ready, hasReady := conditionOf(c.Status.Conditions, "Ready")
	bound, hasBound := conditionOf(c.Status.Conditions, "Bound")

	switch {
	case hasReady && ready.Reason == "ClaimExpired":
		st.State, st.Detail = policy.SandboxExpired, "its lifetime ran out"
	case hasReady && ready.Status == "True":
		st.State = policy.SandboxReady
	case hasBound && bound.Status == "False":
		st.State = policy.SandboxPending
		st.Detail = detailOf(bound, "waiting for a free sandbox in the pool")
	case hasReady:
		st.State, st.Detail = policy.SandboxPending, detailOf(ready, "starting")
	default:
		st.State, st.Detail = policy.SandboxPending, "accepted; waiting for the pool"
	}
	return st
}

func claimEnvList(env map[string]string) []claimEnv {
	kv := envList(env)
	out := make([]claimEnv, 0, len(kv))
	for _, e := range kv {
		// The container is named, so a sidecar added to the template later
		// does not also get this sandbox's API key.
		out = append(out, claimEnv{Name: e.Name, Value: e.Value, ContainerName: "sandbox"})
	}
	return out
}
