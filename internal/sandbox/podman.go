package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The podman driver runs sandboxes on one host, for `make dev` and tests. The
// compose gateway runs in a container and cannot use it. It is the weaker
// driver:
//
//	no warm pool          every start is a cold start
//	no scheduling         one host; it fits or it does not
//	weaker isolation      whatever runtime the host has, usually runc
//	expiry is ours        no controller enforces it, so the gateway's sweep
//	                      does; while the gateway is down, sandboxes outlive
//	                      their lifetime, and the sweep at start-up ends them
//
// See docs/sandboxes.md.

type PodmanOptions struct {
	Binary string
	// Network empty is podman's default. Rootless, the host cannot route into
	// it, which is why Dial uses a published port.
	Network string
	// Runtimes maps a tier to an OCI runtime: "crun" or "runc" for standard,
	// "runsc" for isolated, "krun" for vm. A tier with no entry is refused.
	Runtimes map[policy.Isolation]string
	Log      *slog.Logger
}

type Podman struct {
	opts PodmanOptions
	log  *slog.Logger
}

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

func (p *Podman) Name() string { return "podman" }

func (p *Podman) Capabilities() Capabilities {
	return capabilitiesFor(p.opts.Runtimes, false)
}

// containerName and volumeName depend only on the Ref, so a restarted gateway
// finds what it left running.
func containerName(ref Ref) string { return "keera-sbx-" + objectName(ref) }
func volumeName(ref Ref) string    { return "keera-home-" + objectName(ref) }

func (p *Podman) Create(ctx context.Context, spec Spec) (Status, error) {
	runtime, err := runtimeFor(spec.Class, p.opts.Runtimes, p.Name())
	if err != nil {
		return Status{}, err
	}

	// Already there, so a retry after a dropped connection does not fail.
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

// runArgs is the `podman run` command line, and podman's environment.
func (p *Podman) runArgs(spec Spec, runtime string) (args, env []string) {
	args = []string{"run", "--detach", "--name", containerName(spec.Ref)}
	if runtime != "" {
		args = append(args, "--runtime", runtime)
	}
	if p.opts.Network != "" {
		args = append(args, "--network", p.opts.Network)
	}
	// The only record of a sandbox outside the database.
	labels := labelsFor(spec)
	maps.Copy(labels, annotationsFor(spec))
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", name+"="+labels[name])
	}
	if spec.Class.CPU > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(spec.Class.CPU)/1000, 'f', 2, 64))
	}
	if spec.Class.Memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", spec.Class.Memory))
	}
	// The same hardening as the Kubernetes driver.
	args = append(args,
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--user", strconv.Itoa(sandboxUID),
		"--volume", volumeName(spec.Ref)+":"+homePath,
	)
	// Loopback only, to keep shells off the host's public address.
	args = append(args, "--publish", "127.0.0.1::"+strconv.Itoa(PortSSH))
	// Every user can read a process's arguments, so values go through
	// podman's environment. A name podman uses itself, such as PATH or HOME,
	// stays on the command line.
	for _, kv := range envList(spec.Env) {
		if _, podmans := os.LookupEnv(kv.Name); podmans && !strings.HasPrefix(kv.Name, "KEERA_") {
			args = append(args, "--env", kv.Name+"="+kv.Value)
			continue
		}
		args = append(args, "--env", kv.Name)
		env = append(env, kv.Name+"="+kv.Value)
	}
	return append(args, spec.Class.Image), env
}

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

func (p *Podman) inspect(ctx context.Context, ref Ref) (podmanInspect, error) {
	out, err := p.run(ctx, "inspect", "--type", "container", containerName(ref))
	if err != nil {
		return podmanInspect{}, podmanNotFound(err)
	}
	var items []podmanInspect
	if err := json.Unmarshal(out, &items); err != nil {
		return podmanInspect{}, fmt.Errorf("reading the state of sandbox %s: %w", ref.Name, err)
	}
	// Podman can answer a missing container with an empty list.
	if len(items) == 0 {
		return podmanInspect{}, ErrNotFound
	}
	return items[0], nil
}

func (p *Podman) Status(ctx context.Context, ref Ref) (Status, error) {
	in, err := p.inspect(ctx, ref)
	if err != nil {
		return Status{}, err
	}
	st := Status{Ref: ref}
	if in.State.Running {
		st.Address = "127.0.0.1"
	}
	st.State, st.Detail = podmanState(in)
	st.Exited = in.State.Status == "exited" && in.State.ExitCode == 0
	if st.State == policy.SandboxReady && in.Config.Labels[labelPurpose] != string(policy.PurposeAgent) &&
		!p.sshdListens(ctx, ref) {
		st.State, st.Detail = policy.SandboxPending, "setting up; sshd is not listening yet"
	}
	return st, nil
}

// sshdListens stands in for a readiness probe. It waits for sshd's greeting,
// because rootless podman accepts a connection even when nothing listens yet.
func (p *Podman) sshdListens(ctx context.Context, ref Ref) bool {
	conn, err := p.dialPublished(ctx, ref, PortSSH)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	return sshGreets(conn)
}

// sshGreetingTimeout is short: sshd greets as soon as a connection opens.
const sshGreetingTimeout = 2 * time.Second

func sshGreets(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(sshGreetingTimeout))
	greeting := make([]byte, 4)
	_, err := io.ReadFull(conn, greeting)
	return err == nil && string(greeting) == "SSH-"
}

func podmanState(in podmanInspect) (policy.SandboxState, string) {
	switch {
	case in.State.Running:
		return policy.SandboxReady, ""
	case in.State.Status == "created":
		return policy.SandboxPending, "starting"
	case in.State.Status == "exited" && in.State.ExitCode == 0:
		// A suspend looks like a clean exit. Suspended is the safe guess; the
		// manager tells a finished agent apart.
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

func (p *Podman) Suspend(ctx context.Context, ref Ref) error {
	_, err := p.run(ctx, "stop", "--time", "20", containerName(ref))
	return podmanNotFound(err)
}

func (p *Podman) Resume(ctx context.Context, ref Ref) error {
	_, err := p.run(ctx, "start", containerName(ref))
	return podmanNotFound(err)
}

// Revive replaces the container, since its environment is fixed, and keeps the
// volume.
func (p *Podman) Revive(ctx context.Context, spec Spec) error {
	// Otherwise `podman run` would quietly make a new, empty volume.
	if _, err := p.run(ctx, "volume", "inspect", volumeName(spec.Ref)); err != nil {
		return podmanNotFound(err)
	}
	if err := p.removeContainer(ctx, containerName(spec.Ref)); err != nil {
		return err
	}
	_, err := p.Create(ctx, spec)
	return err
}

// Extend only checks the container exists. The sweep enforces the expiry, since
// podman cannot change a container's labels.
func (p *Podman) Extend(ctx context.Context, ref Ref, _ time.Time) error {
	_, err := p.Status(ctx, ref)
	return err
}

func (p *Podman) Terminate(ctx context.Context, ref Ref) error {
	if err := p.removeContainer(ctx, containerName(ref)); err != nil {
		return err
	}
	// `podman rm --volumes` leaves a named volume behind.
	if _, err := p.run(ctx, "volume", "rm", "--force", volumeName(ref)); err != nil {
		p.log.Warn("sandbox: removing the home volume failed",
			"sandbox", ref.ID, "volume", volumeName(ref), "error", err)
	}
	return nil
}

// removeContainer counts a container already gone as removed.
func (p *Podman) removeContainer(ctx context.Context, name string) error {
	if _, err := p.run(ctx, "rm", "--force", "--time", "20", name); err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}

// Dial connects to the published host port. It refuses a sandbox that is not
// running, for a clearer error.
func (p *Podman) Dial(ctx context.Context, ref Ref, port int) (net.Conn, error) {
	// Only sshd's port is published.
	if port != PortSSH {
		return nil, Refuse("on podman only port %d of a sandbox is reachable, not %d", PortSSH, port)
	}
	in, err := p.inspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state, _ := podmanState(in); state != policy.SandboxReady {
		return nil, fmt.Errorf("%w: it is %s", ErrNotReady, state)
	}
	return p.dialPublished(ctx, ref, port)
}

// dialPublished connects to a published port without looking at the state.
func (p *Podman) dialPublished(ctx context.Context, ref Ref, port int) (net.Conn, error) {
	out, err := p.run(ctx, "port", containerName(ref), strconv.Itoa(port))
	if err != nil {
		return nil, podmanNotFound(err)
	}
	// One "127.0.0.1:43117" line per mapping; a second is the IPv6 one.
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("%w: port %d is not published on sandbox %s",
			ErrNotReady, port, ref.Name)
	}
	return dialSandbox(ctx, ref, port, line)
}

// podmanTimeout is generous because `run` may include an image pull.
const podmanTimeout = 5 * time.Minute

func (p *Podman) run(ctx context.Context, args ...string) ([]byte, error) {
	return p.runEnv(ctx, nil, args...)
}

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
		return nil, &podmanError{Binary: p.opts.Binary, Args: args, Msg: msg, Err: err}
	}
	return stdout.Bytes(), nil
}

// podmanError carries what the command printed.
type podmanError struct {
	// Binary can be set by KEERA_SANDBOX_PODMAN_BINARY.
	Binary string
	Args   []string
	Msg    string
	Err    error
}

func (e *podmanError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Binary, e.Args[0], e.Msg)
}

func (e *podmanError) Unwrap() error { return e.Err }

// isNoSuchContainer matches the message, since podman's exit code 125 is
// shared with every usage error.
func isNoSuchContainer(err error) bool {
	var pe *podmanError
	if !errors.As(err, &pe) {
		return false
	}
	msg := strings.ToLower(pe.Msg)
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such volume") ||
		strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "not found")
}

// podmanNotFound maps a missing container or volume to ErrNotFound.
func podmanNotFound(err error) error {
	if isNoSuchContainer(err) {
		return ErrNotFound
	}
	return err
}
