// Package sandbox runs sandboxes: machines the gateway lends out for one task
// or one working day. A sandbox is a container with the toolchain, a
// persistent home, its own API key, and limited network access. An engineer
// attaches over ssh; an agent runs inside one. See docs/sandboxes.md.
//
// Everything is behind Driver. The Kubernetes driver writes Sandbox objects
// for the agent-sandbox controller (sigs.k8s.io/agent-sandbox). The podman
// driver runs local containers for development, with weaker isolation and no
// warm pool.
//
// The Kubernetes driver talks plain HTTPS to the API server instead of using
// client-go, which would grow the module graph a lot to save a few hundred
// lines of JSON.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Errors a caller acts on. Other driver errors are just reported.
var (
	// Terminate treats ErrNotFound as success.
	ErrNotFound = errors.New("sandbox: not found")
	// ErrNotReady means a sandbox was not running although its row said so.
	ErrNotReady = errors.New("sandbox: not ready")
)

// Ref names one sandbox to a driver. Drivers derive their names from it, so a
// restarted gateway finds the sandboxes it left running.
type Ref struct {
	ID string
	// Name is what the developer called it. It shows in kubectl and podman ps.
	Name string
	// Backing is empty for BackingSandbox.
	Backing Backing
}

func (r Ref) Claimed() bool { return r.Backing == BackingClaim }

// Spec is everything a driver needs to make one sandbox. It is fully resolved,
// so drivers make no decisions and behave alike.
type Spec struct {
	Ref
	Class   policy.SandboxClass
	Purpose policy.Purpose
	// Owner, Org and Project are stamped onto the object, so operators see
	// whose it is. Owner is an email address, the only personal data there.
	Owner   string
	Org     string
	Project string
	// Env includes the API key. It sits in the pod spec, not a Secret, as it
	// lives no longer than the sandbox. Anyone who can read pods in the
	// namespace can read it, so that namespace is kept to itself.
	Env map[string]string
	// Expires is enforced by the Kubernetes controller too, so nothing is left
	// if the gateway never comes back. Podman ignores it.
	Expires time.Time
}

// Status is what a driver knows about one sandbox right now.
type Status struct {
	Ref
	State policy.SandboxState
	// Detail says why a state is not Ready, in the driver's own words.
	Detail string
	// Address is a host or IP without a port. Empty while not running.
	Address string
	Node    string
	// Exited says the process ended by itself with success. For an agent,
	// the task is done and the manager terminates it.
	Exited bool
}

// Driver runs sandboxes on one platform. Methods are idempotent where they can
// be, which keeps rows and reality in step after a dropped connection.
type Driver interface {
	// Name is "kubernetes" or "podman".
	Name() string
	// Capabilities lets the control plane refuse clearly instead of failing
	// inside the driver.
	Capabilities() Capabilities
	// Create returns the status, usually Pending, without waiting for
	// readiness.
	Create(ctx context.Context, spec Spec) (Status, error)
	Status(ctx context.Context, ref Ref) (Status, error)
	// Suspend releases the compute and keeps the volume. Resume undoes it.
	Suspend(ctx context.Context, ref Ref) error
	Resume(ctx context.Context, ref Ref) error
	// Revive starts an expired sandbox again on its volume, with a new
	// environment, since its old key was revoked at expiry.
	Revive(ctx context.Context, spec Spec) error
	Extend(ctx context.Context, ref Ref, until time.Time) error
	// Terminate removes the sandbox and its volume. One already gone is not
	// an error.
	Terminate(ctx context.Context, ref Ref) error
	// Dial is the only way in: the control plane proxies authenticated
	// callers to it.
	Dial(ctx context.Context, ref Ref, port int) (net.Conn, error)
}

type Capabilities struct {
	// Isolation is the strongest tier.
	Isolation policy.Isolation
	// Tiers is standard, and each tier with a runtime mapped. A class asking
	// for another is refused, not run with less.
	Tiers []policy.Isolation
	Warm  bool
}

// The ports a sandbox serves.
const (
	// PortSSH is 2222, not 22: a sandbox runs without capabilities and cannot
	// bind a privileged port.
	PortSSH = 2222
	// PortForwardMin and PortForwardMax bound what a developer may forward.
	PortForwardMin = 1024
	PortForwardMax = 65535
)

// ValidPort reports whether a port may be reached through the attach surface.
// Nothing in a sandbox can bind a privileged port anyway.
func ValidPort(p int) bool {
	return p >= PortForwardMin && p <= PortForwardMax
}

// dialTimeout is short: the sandbox is close by and a developer is waiting.
const dialTimeout = 10 * time.Second

func dialSandbox(ctx context.Context, ref Ref, port int, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("reaching sandbox %s on port %d: %w", ref.Name, port, err)
	}
	return conn, nil
}

// capabilitiesFor derives tiers from the runtimes, so a driver cannot claim a
// tier it has no runtime for.
func capabilitiesFor(runtimes map[policy.Isolation]string, warm bool) Capabilities {
	tiers := []policy.Isolation{policy.IsolationStandard}
	for _, tier := range policy.Isolations {
		if tier != policy.IsolationStandard && runtimes[tier] != "" {
			tiers = append(tiers, tier)
		}
	}
	// policy.Isolations is weakest first.
	return Capabilities{Isolation: tiers[len(tiers)-1], Tiers: tiers, Warm: warm}
}

// runtimeFor returns empty for the standard tier: the platform's default.
func runtimeFor(c policy.SandboxClass, runtimes map[policy.Isolation]string, driver string) (string, error) {
	if c.Isolation == policy.IsolationStandard {
		return runtimes[policy.IsolationStandard], nil
	}
	if name := runtimes[c.Isolation]; name != "" {
		return name, nil
	}
	return "", noRuntime(c, driver)
}

// noRuntime refuses a class whose tier has no runtime here, rather than run it
// with weaker isolation.
func noRuntime(c policy.SandboxClass, driver string) error {
	return Refuse("the sandbox class %q asks for %s isolation and the %s driver in this "+
		"deployment has no runtime for it; set KEERA_SANDBOX_RUNTIME_%s (a RuntimeClass on "+
		"Kubernetes, runsc or krun on podman), or move the class to a tier it has",
		c.Name, c.Isolation, driver, strings.ToUpper(string(c.Isolation)))
}

// The labels on every object a driver writes, so operators see whose it is.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelName      = "app.kubernetes.io/name"
	labelSandboxID = "keera.dev/sandbox-id"
	labelOrg       = "keera.dev/org"
	labelProject   = "keera.dev/project"
	labelClass     = "keera.dev/class"
	labelPurpose   = "keera.dev/purpose"
	// annotationOwner is an annotation because a label value cannot hold an @.
	annotationOwner = "keera.dev/owner"
)

// objectName is the developer's name plus the tail of the id: unique, and
// readable in `kubectl get pods`. It depends only on the Ref, so a restarted
// gateway finds what it left running.
func objectName(ref Ref) string {
	name := ref.Name
	if name == "" {
		name = "sandbox"
	}
	return name + "-" + shortID(ref.ID)
}

// shortID is the end of an id, enough to tell objects apart in a short name.
func shortID(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return id
}

// labelsFor is shared by every driver path so the labels cannot drift.
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

// The home volume holds everything that should survive a suspend. The image's
// sshd_config and entrypoint use the same path and user.
const (
	homeVolume = "home"
	homePath   = "/home/keera"
	// sandboxUID is fixed; sandbox/Containerfile says why.
	sandboxUID = 1000
)
