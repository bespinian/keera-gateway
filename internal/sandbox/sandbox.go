// Package sandbox runs sandboxes: machines the gateway lends out for one task
// or one working day. A sandbox is a container with the toolchain in it, a
// persistent home, its own API key, and network access only where the
// deployment allows it. An engineer attaches over ssh; an agent is started
// inside one and never leaves it. See docs/sandboxes.md.
//
// # The driver
//
// Everything is behind Driver, with two implementations. The Kubernetes driver
// writes Sandbox objects for the agent-sandbox controller
// (sigs.k8s.io/agent-sandbox). The podman driver runs local containers for the
// single-host compose and NixOS setups; it has weaker isolation and no warm
// pool.
//
// # No client-go
//
// The Kubernetes driver talks plain HTTPS to the API server and decodes only
// the fields it reads. The binary is static, has few dependencies and must
// vendor cleanly for air-gapped sites; client-go would grow the module graph
// enormously to save a few hundred lines of JSON.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Errors a caller acts on. Anything else a driver returns is just reported.
var (
	// ErrNotFound means the sandbox no longer exists where the driver looks.
	// Terminate treats it as success.
	ErrNotFound = errors.New("sandbox: not found")
	// ErrNotReady means the operation needs a running sandbox, most often an
	// attach to a suspended one. The control plane turns it into "resume it
	// first".
	ErrNotReady = errors.New("sandbox: not ready")
	// ErrUnsupported means the driver does not do this at all, so the control
	// plane can say "not in this deployment" instead of "it broke".
	ErrUnsupported = errors.New("sandbox: not supported by this driver")
)

// Ref names one sandbox to a driver.
//
// It carries the gateway's id, not the driver's. Each driver derives its own
// names from it deterministically, so a restarted gateway finds the sandboxes
// it left running.
type Ref struct {
	// ID is the sandbox id, "sbx_...".
	ID string
	// Name is what the developer called it. Drivers use it for the hostname,
	// because that is what shows in the shell prompt.
	Name string
	// Backing says whether the sandbox is a driver-made object or a claim on
	// a warm pool. Empty means BackingSandbox.
	//
	// It is stored on the row rather than derived from configuration, so
	// turning warm pools off still leaves the old claims addressable.
	Backing Backing
}

// Claimed reports whether this sandbox came out of a warm pool.
func (r Ref) Claimed() bool { return r.Backing == BackingClaim }

// Spec is everything a driver needs to make one sandbox.
//
// It is fully resolved: class, policy, lifetime and credentials are decided
// before it gets here, so the drivers make no decisions and behave alike.
type Spec struct {
	Ref
	// Class is the resolved catalogue entry. Drivers read the image, the
	// resources and the isolation tier from it.
	Class policy.SandboxClass
	// Purpose decides the lifecycle. See policy.Purpose.
	Purpose policy.Purpose
	// Owner, Org and Team are stamped onto the object, so a platform team can
	// see whose sandbox is on a node without asking the gateway.
	//
	// Owner is an email address where known. It is the only personal data this
	// package puts into the cluster.
	Owner string
	Org   string
	Team  string
	// Env is the sandbox's environment: the gateway's address, its API key,
	// the session id for an agent, and what the repository setup needs.
	//
	// The key sits in the pod spec rather than a Secret: it has the sandbox's
	// lifetime anyway. So anybody who can read pods in the sandbox namespace
	// can read it, which is why that namespace is kept to itself.
	Env map[string]string
	// Expires is when the driver should tear the sandbox down by itself. It
	// is absolute because the Kubernetes controller enforces it too, so
	// nothing is left behind if the gateway never comes back.
	Expires time.Time
	// Ports are what the sandbox listens on, for drivers that must declare
	// them. PortSSH is always included.
	Ports []int
}

// Status is what a driver knows about one sandbox right now.
type Status struct {
	Ref
	State policy.SandboxState
	// Detail says why, for a state that is not Ready. It reaches the
	// developer, so the driver's own message ("0/6 nodes are available") is
	// passed through as is.
	Detail string
	// Address is where the gateway reaches the sandbox: a host or IP, without
	// a port. Empty while it is not running.
	Address string
	// Node is where it is scheduled.
	Node string
	// Expires is when the driver will tear it down. It is read back, so a
	// failed extension shows up as a mismatch with the row.
	Expires time.Time
}

// Driver is what one deployment shape can do with sandboxes.
//
// Every method is addressed by Ref and idempotent where it can be: creating a
// sandbox that exists returns its status, terminating one that does not is
// success. The control plane does not retry, so this is what keeps rows and
// reality in step after a dropped connection.
type Driver interface {
	// Name is the driver's name for logs and the panel: "kubernetes" or
	// "podman".
	Name() string
	// Capabilities says what this driver implements, so the control plane can
	// refuse clearly instead of failing inside the driver.
	Capabilities() Capabilities
	// Create makes one sandbox and returns its status, usually Pending. It
	// does not wait for readiness; the caller is an HTTP request.
	Create(ctx context.Context, spec Spec) (Status, error)
	// Status reads one sandbox back.
	Status(ctx context.Context, ref Ref) (Status, error)
	// Suspend releases the compute and keeps the volume. Resume undoes it.
	// Both are no-ops on a sandbox already in that state.
	Suspend(ctx context.Context, ref Ref) error
	Resume(ctx context.Context, ref Ref) error
	// Extend moves when the driver will tear the sandbox down.
	Extend(ctx context.Context, ref Ref, until time.Time) error
	// Terminate removes the sandbox and everything it held, including the
	// volume. A sandbox that is already gone is not an error.
	Terminate(ctx context.Context, ref Ref) error
	// Dial opens a connection to one port of a running sandbox. It is the
	// only way in: the control plane proxies authenticated callers to it.
	Dial(ctx context.Context, ref Ref, port int) (net.Conn, error)
}

// Capabilities is what a driver can do.
type Capabilities struct {
	// Suspend is whether a sandbox can be stopped and started again with its
	// volume intact.
	Suspend bool
	// Isolation is the strongest tier this driver can deliver. A class asking
	// for more is refused rather than silently run with less.
	Isolation policy.Isolation
	// Warm is whether this driver keeps a pool of started sandboxes.
	Warm bool
	// Persistence is whether a volume survives a suspend.
	Persistence bool
}

// The ports a sandbox serves. Only PortSSH is opened by the drivers; the range
// bounds what the attach surface lets a caller reach.
const (
	// PortSSH is the shell, and the transport for remote editors (VS Code
	// Remote-SSH, JetBrains Gateway).
	//
	// It is 2222, not 22: a sandbox runs as an ordinary user with every
	// capability dropped and cannot bind a privileged port. Nobody types the
	// number; the gateway dials it.
	PortSSH = 2222
	// PortForwardMin and PortForwardMax bound what a developer may forward:
	// unprivileged ports only.
	PortForwardMin = 1024
	PortForwardMax = 65535
)

// ValidPort reports whether a port may be reached through the attach surface.
// Only unprivileged ports are allowed: a privileged port is the only signal
// that a port belongs to the sandbox itself, and nothing in a sandbox can bind
// one anyway.
func ValidPort(p int) bool {
	return p >= PortForwardMin && p <= PortForwardMax
}

// DialTimeout bounds one connection attempt to a sandbox. It is short because
// the sandbox is close by and a developer is waiting at a terminal.
const DialTimeout = 10 * time.Second

// dialAddress joins a host and a port, handling IPv6.
func dialAddress(host string, port int) string {
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// dialSandbox connects to addr, which serves the given port of the sandbox.
func dialSandbox(ctx context.Context, ref Ref, port int, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("reaching sandbox %s on port %d: %w", ref.Name, port, err)
	}
	return conn, nil
}

// strongestIsolation is the strongest tier with a runtime mapped to it.
// Deriving it from the mapping means a driver cannot claim a tier it has no
// runtime for.
func strongestIsolation(runtimes map[policy.Isolation]string) policy.Isolation {
	best := policy.IsolationStandard
	for _, tier := range policy.Isolations {
		if runtimes[tier] != "" {
			best = tier
		}
	}
	return best
}

// mappedRuntime is the runtime a class runs on. ok is false when the class
// needs a tier that has no runtime mapped; the caller words the refusal. The
// standard tier never needs one: empty means the platform's default.
func mappedRuntime(c policy.SandboxClass, runtimes map[policy.Isolation]string) (name string, ok bool) {
	if c.RuntimeClass != "" {
		return c.RuntimeClass, true
	}
	if c.Isolation == policy.IsolationStandard {
		return runtimes[policy.IsolationStandard], true
	}
	name = runtimes[c.Isolation]
	return name, name != ""
}

// formatExpiry renders an expiry the way both drivers store it.
func formatExpiry(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// parseExpiry reads back what formatExpiry wrote. Anything unreadable is the
// zero time.
func parseExpiry(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
