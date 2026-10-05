package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The podman driver runs sandboxes on a single host, for a gateway that runs as
// a process there (`make dev`, a NixOS service) and has podman but no cluster.
// The compose gateway runs in a container and cannot use it.
//
// It is the weaker driver, on purpose and openly:
//
//	no warm pool          every start is a cold start
//	no scheduling         one host; it fits or it does not
//	weaker isolation      whatever runtime the host has, usually runc
//	expiry is ours        no controller enforces it, so the gateway's sweep
//	                      does; while the gateway is down, sandboxes outlive
//	                      their lifetime, and the sweep at start-up ends them
//
// The last one is the real difference from Kubernetes, and docs/sandboxes.md
// says so too.

// PodmanOptions configures the driver.
type PodmanOptions struct {
	// Binary is the podman command, "podman" by default.
	Binary string
	// Network is the container network sandboxes join. Empty is podman's
	// default, which on rootless podman the host cannot route into; that is
	// why Dial uses a published port.
	Network string
	// Runtimes maps an isolation tier to an OCI runtime on this host: "crun"
	// or "runc" for standard, "runsc" for isolated, "krun" for vm. A tier with
	// no entry is refused, never downgraded.
	Runtimes map[policy.Isolation]string
	Log      *slog.Logger
}

// Podman is the driver.
type Podman struct {
	opts PodmanOptions
	log  *slog.Logger
}

// NewPodman builds the driver and checks that the binary runs.
func NewPodman(ctx context.Context, opts PodmanOptions) (*Podman, error) {
	if opts.Binary == "" {
		opts.Binary = "podman"
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	p := &Podman{opts: opts, log: opts.Log}
	if _, err := p.run(ctx, "version", "--format", "{{.Client.Version}}"); err != nil {
		return nil, fmt.Errorf("running %s: %w; the podman sandbox driver needs it on PATH",
			opts.Binary, err)
	}
	return p, nil
}

// Name returns "podman".
func (p *Podman) Name() string { return "podman" }

// Capabilities reports what this driver does: no warm pool, and the
// strongest isolation tier with a runtime mapped.
func (p *Podman) Capabilities() Capabilities {
	return Capabilities{
		Isolation: strongestIsolation(p.opts.Runtimes), Tiers: mappedTiers(p.opts.Runtimes),
	}
}

// containerName and volumeName depend only on the Ref, so a restarted gateway
// finds what it left running.
func containerName(ref Ref) string { return "keera-sbx-" + objectName(ref) }
func volumeName(ref Ref) string    { return "keera-home-" + objectName(ref) }

// The labels on every container, for an operator reading `podman ps`. They
// are the only record of a sandbox outside the database, so they say whose it
// is as the Kubernetes labels do.
const (
	podmanLabelID      = "keera.sandbox.id"
	podmanLabelName    = "keera.sandbox.name"
	podmanLabelOwner   = "keera.sandbox.owner"
	podmanLabelOrg     = "keera.sandbox.org"
	podmanLabelProject = "keera.sandbox.project"
	podmanLabelClass   = "keera.sandbox.class"
	podmanLabelPurpose = "keera.sandbox.purpose"
)

// Create starts one container.
func (p *Podman) Create(ctx context.Context, spec Spec) (Status, error) {
	runtime, err := p.runtimeFor(spec.Class)
	if err != nil {
		return Status{}, err
	}

	// Already there is what was asked for, so a retry after a dropped
	// connection does not fail.
	if st, err := p.Status(ctx, spec.Ref); err == nil {
		return st, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Status{}, err
	}

	args, env := p.runArgs(spec, runtime)
	if _, err := p.runEnv(ctx, env, args...); err != nil {
		return Status{}, err
	}
	return p.Status(ctx, spec.Ref)
}

// privateEnv are the values kept off podman's command line, where every user
// of the host can read them. podman takes them from its own environment
// instead, which only the gateway's user can read.
var privateEnv = map[string]bool{
	"KEERA_API_KEY": true, "KEERA_GIT_TOKEN": true, "KEERA_TASK": true,
}

// runArgs is the `podman run` command line for a spec, and the environment
// podman needs to run it with.
func (p *Podman) runArgs(spec Spec, runtime string) (args, env []string) {
	args = []string{"run", "--detach", "--name", containerName(spec.Ref)}
	if runtime != "" {
		args = append(args, "--runtime", runtime)
	}
	if p.opts.Network != "" {
		args = append(args, "--network", p.opts.Network)
	}
	args = append(args,
		"--label", podmanLabelID+"="+spec.ID,
		"--label", podmanLabelName+"="+spec.Name,
		"--label", podmanLabelClass+"="+spec.Class.Name,
		"--label", podmanLabelPurpose+"="+string(spec.Purpose),
	)
	for _, l := range [][2]string{
		{podmanLabelOwner, spec.Owner}, {podmanLabelOrg, spec.Org}, {podmanLabelProject, spec.Project},
	} {
		if l[1] != "" {
			args = append(args, "--label", l[0]+"="+l[1])
		}
	}
	if spec.Class.CPU > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(spec.Class.CPU)/1000, 'f', 2, 64))
	}
	if spec.Class.Memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", spec.Class.Memory))
	}
	// The same hardening as the Kubernetes driver. The root filesystem stays
	// writable there too.
	args = append(args,
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--user", strconv.Itoa(sandboxUID),
		"--volume", volumeName(spec.Ref)+":"+homePath,
	)
	// sshd is published on a host-chosen port on loopback. Rootless podman's
	// network is not routable from the host, and loopback keeps developers'
	// shells off the host's public address.
	args = append(args, "--publish", "127.0.0.1::"+strconv.Itoa(PortSSH))
	for _, kv := range envList(spec.Env) {
		if privateEnv[kv.Name] {
			args = append(args, "--env", kv.Name)
			env = append(env, kv.Name+"="+kv.Value)
			continue
		}
		args = append(args, "--env", kv.Name+"="+kv.Value)
	}
	return append(args, spec.Class.Image), env
}

func (p *Podman) runtimeFor(c policy.SandboxClass) (string, error) {
	name, ok := mappedRuntime(c, p.opts.Runtimes)
	if !ok {
		return "", fmt.Errorf("the sandbox class %q asks for %s isolation and this host has no "+
			"OCI runtime mapped to it; set KEERA_SANDBOX_RUNTIME_%s (runsc for isolated, krun "+
			"for vm), or move the class to a tier this host can deliver",
			c.Name, c.Isolation, strings.ToUpper(string(c.Isolation)))
	}
	return name, nil
}

// podmanInspect is the part of `podman inspect` this driver reads.
type podmanInspect struct {
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		Error    string `json:"Error"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Status reads one container back.
func (p *Podman) Status(ctx context.Context, ref Ref) (Status, error) {
	out, err := p.run(ctx, "inspect", "--type", "container", containerName(ref))
	if err != nil {
		return Status{}, podmanNotFound(err)
	}
	var items []podmanInspect
	if err := json.Unmarshal(out, &items); err != nil {
		return Status{}, fmt.Errorf("reading the state of sandbox %s: %w", ref.Name, err)
	}
	// An empty list means podman found no such container, the same as its
	// "no such container" error.
	if len(items) == 0 {
		return Status{}, ErrNotFound
	}
	in := items[0]

	st := Status{Ref: ref}
	if in.State.Running {
		st.Address = "127.0.0.1"
	}
	st.State, st.Detail = podmanState(in)
	st.Exited = in.State.Status == "exited" && in.State.ExitCode == 0
	if st.State == policy.SandboxReady && in.Config.Labels[podmanLabelPurpose] != string(policy.PurposeAgent) &&
		!p.sshdListens(ctx, ref) {
		st.State, st.Detail = policy.SandboxPending, "setting up; sshd is not listening yet"
	}
	return st, nil
}

// sshdListens stands in for the Kubernetes readiness probe. The entrypoint
// starts sshd last, so a running container is only ready once sshd answers:
// before that, the repository may still be checking out.
//
// It dials from the host, so the image needs no shell for it. A connection
// alone proves nothing, since rootless podman's port forwarder accepts one
// even when nothing listens behind it yet, so it waits for sshd's greeting.
func (p *Podman) sshdListens(ctx context.Context, ref Ref) bool {
	conn, err := p.dialPublished(ctx, ref, PortSSH)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	return sshGreets(conn)
}

// sshGreetingTimeout bounds the wait for sshd's greeting, which it sends as
// soon as a connection is open.
const sshGreetingTimeout = 2 * time.Second

// sshGreets reports whether conn opens with an SSH version line.
func sshGreets(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(sshGreetingTimeout))
	greeting := make([]byte, 4)
	_, err := io.ReadFull(conn, greeting)
	return err == nil && string(greeting) == "SSH-"
}

// podmanState collapses a container's state into a sandbox state and detail.
func podmanState(in podmanInspect) (policy.SandboxState, string) {
	switch {
	case in.State.Running:
		return policy.SandboxReady, ""
	case in.State.Status == "created":
		return policy.SandboxPending, "starting"
	case in.State.Status == "exited" && in.State.ExitCode == 0:
		// A clean stop is what a suspend looks like here. A process that
		// exited zero by itself looks the same; reading it as suspended is the
		// safe guess, since a resume that exits again is visible. An agent
		// that finished is told apart by the manager, which knows it was
		// not suspended.
		return policy.SandboxSuspended, "stopped; its volume is kept"
	case in.State.Status == "exited":
		detail := fmt.Sprintf("the sandbox process exited with status %d", in.State.ExitCode)
		if in.State.Error != "" {
			detail += ": " + in.State.Error
		}
		return policy.SandboxFailed, detail
	default:
		return policy.SandboxPending, in.State.Status
	}
}

// Suspend stops the container and keeps the named volume.
func (p *Podman) Suspend(ctx context.Context, ref Ref) error {
	_, err := p.run(ctx, "stop", "--time", "20", containerName(ref))
	return podmanNotFound(err)
}

// Resume starts a stopped container again.
func (p *Podman) Resume(ctx context.Context, ref Ref) error {
	_, err := p.run(ctx, "start", containerName(ref))
	return podmanNotFound(err)
}

// Revive replaces the container and keeps the named volume. A container's
// environment is fixed when it is made, so a new key needs a new container.
// Only the home directory carries over. A suspend keeps more, since it only
// stops the container.
func (p *Podman) Revive(ctx context.Context, spec Spec) error {
	if err := p.removeContainer(ctx, containerName(spec.Ref)); err != nil && !isNoSuchContainer(err) {
		return err
	}
	_, err := p.Create(ctx, spec)
	return err
}

// Extend only checks that the container still exists.
//
// There is no controller here: the database row holds the expiry, and the
// gateway's sweep enforces it. That is also why no label carries it: podman
// cannot change a container's labels, so one would go stale.
func (p *Podman) Extend(ctx context.Context, ref Ref, _ time.Time) error {
	_, err := p.Status(ctx, ref)
	return err
}

// Terminate removes the container and its volume.
func (p *Podman) Terminate(ctx context.Context, ref Ref) error {
	if err := p.removeContainer(ctx, containerName(ref)); err != nil && !isNoSuchContainer(err) {
		return err
	}
	// The volume is named, so `podman rm --volumes` would leave it. Without
	// this, every terminated sandbox would leave a volume behind.
	if _, err := p.run(ctx, "volume", "rm", "--force", volumeName(ref)); err != nil {
		p.log.Warn("sandbox: removing the home volume failed",
			"sandbox", ref.ID, "volume", volumeName(ref), "error", err)
	}
	return nil
}

func (p *Podman) removeContainer(ctx context.Context, name string) error {
	_, err := p.run(ctx, "rm", "--force", "--time", "20", name)
	return err
}

// Dial connects to the host port the container's port is published on. Like
// the Kubernetes driver, it refuses a sandbox that is not ready, so a
// suspended one says so rather than that its port is not published.
func (p *Podman) Dial(ctx context.Context, ref Ref, port int) (net.Conn, error) {
	st, err := p.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	if st.State != policy.SandboxReady {
		return nil, fmt.Errorf("%w: it is %s", ErrNotReady, st.State)
	}
	return p.dialPublished(ctx, ref, port)
}

// dialPublished connects to a published port without looking at the state.
func (p *Podman) dialPublished(ctx context.Context, ref Ref, port int) (net.Conn, error) {
	out, err := p.run(ctx, "port", containerName(ref), strconv.Itoa(port))
	if err != nil {
		return nil, podmanNotFound(err)
	}
	// One "127.0.0.1:43117" line per mapping. Take the first; a second line
	// would be the IPv6 mapping of the same port.
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return nil, fmt.Errorf("%w: port %d is not published on sandbox %s",
			ErrNotReady, port, ref.Name)
	}
	return dialSandbox(ctx, ref, port, line)
}

/* ------------------------------------------------------------------ running */

// podmanTimeout bounds one command. It is generous because `run` on a cold
// image includes the pull.
const podmanTimeout = 5 * time.Minute

func (p *Podman) run(ctx context.Context, args ...string) ([]byte, error) {
	return p.runEnv(ctx, nil, args...)
}

// runEnv is run with env added to podman's own environment.
func (p *Podman) runEnv(ctx context.Context, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, podmanTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.opts.Binary, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, &podmanError{Args: args, Msg: msg, Err: err}
	}
	return stdout.Bytes(), nil
}

// podmanError carries what the command printed, which is the useful part.
type podmanError struct {
	Args []string
	Msg  string
	Err  error
}

func (e *podmanError) Error() string {
	return fmt.Sprintf("podman %s: %s", e.Args[0], e.Msg)
}

func (e *podmanError) Unwrap() error { return e.Err }

// isNoSuchContainer recognises a missing container. It matches the message,
// because podman's exit code for it (125) is shared with every usage error.
func isNoSuchContainer(err error) bool {
	var pe *podmanError
	if !errors.As(err, &pe) {
		return false
	}
	msg := strings.ToLower(pe.Msg)
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "not found")
}

// podmanNotFound turns a missing container into ErrNotFound and passes
// anything else through.
func podmanNotFound(err error) error {
	if isNoSuchContainer(err) {
		return ErrNotFound
	}
	return err
}
