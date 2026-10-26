package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

// `keera sandbox`: most verbs are JSON over the control API. `proxy` copies
// bytes between the attach surface and stdin/stdout for ssh's ProxyCommand,
// and `ssh` runs the developer's own ssh with it. VS Code and JetBrains only
// need ssh, so `keera sandbox config` is the whole client setup.

type sandboxRun struct {
	*cmdRun

	project string
	class   string
	purpose string
	ttl     time.Duration
	repo    string
	branch  string
	task    string
	port    int
	sshKey  string
	all     bool
	by      string
	since   time.Duration
}

func sandboxCmd(ctx context.Context, args []string) error {
	r := &sandboxRun{cmdRun: newCmdRun("sandbox", args)}
	fs := r.fs
	fs.StringVar(&r.project, "project", "", "project the sandbox belongs to, by name or id; without it, "+
		"the organisation's oldest. Its key is in that project, so the budget, the rate limit "+
		"and the allowed models are the project's")
	fs.StringVar(&r.class, "class", "", "which machine to ask for (required); 'keera sandbox classes' lists them")
	fs.StringVar(&r.purpose, "purpose", "", "'engineer' - a machine you work in - or 'agent' - one task's")
	r.reword("list", "project", "only the sandboxes of one project, by name or id")
	r.reword("list", "class", "only the sandboxes of one class")
	r.reword("list", "purpose", "only the sandboxes for one purpose: engineer or agent")
	durationVar(fs, &r.ttl, "ttl", 0, "how long it lives; the class's default if not given")
	fs.StringVar(&r.repo, "repo", "", "repository to check out into it")
	fs.StringVar(&r.branch, "branch", "", "branch to check out")
	fs.StringVar(&r.task, "task", "", "what an agent sandbox should carry out; @path reads a file")
	r.reword("agent", "task", "what the agent should carry out (required); @path reads a file")
	fs.IntVar(&r.port, "port", sandbox.PortSSH, "which port to reach through 'proxy'")
	fs.StringVar(&r.sshKey, "ssh-key", "",
		"a file holding the public key that may open a shell in it, such as "+
			"~/.ssh/id_ed25519.pub (default: every *.pub file in ~/.ssh)")
	fs.BoolVar(&r.all, "all", false, "include sandboxes that have finished")
	fs.StringVar(&r.by, "by", "class", "group by: class, project or user")
	durationVar(fs, &r.since, "since", reportWindow, sinceUsage)
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
	case "classes":
		return r.classes(ctx)
	case "apply":
		return r.apply(ctx)
	case "delete-class":
		return r.deleteClass(ctx)
	case "create":
		return r.create(ctx)
	case "agent":
		return r.agent(ctx)
	case "show":
		return r.show(ctx)
	case "terminate":
		return r.terminate(ctx)
	case "extend":
		return r.extend(ctx)
	case "suspend", "resume":
		return r.suspendOrResume(ctx)
	case "ssh":
		return sshInto(ctx, r.c, r.org, fs.Arg(0), fs.Args()[1:])
	case "proxy":
		return proxyTo(ctx, r.c, r.org, fs.Arg(0), r.port, os.Stdin, os.Stdout)
	case "config":
		orgID, err := resolveOrg(ctx, r.c, r.org)
		if err != nil {
			return err
		}
		return writeSSHConfig(r.c.base, orgID)
	case "usage":
		return r.usage(ctx)
	default:
		return r.list(ctx)
	}
}

func (r *sandboxRun) list(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	project, err := projectID(ctx, r.c, orgID, r.project)
	if err != nil {
		return err
	}
	q := url.Values{"org_id": {orgID}}
	if r.all {
		q.Set("all", "1")
	}
	setIfGiven(q, map[string]string{"project_id": project, "class": r.class, "purpose": r.purpose})
	sandboxes, err := list[store.Sandbox](ctx, r.c, "/v1/sandboxes?"+q.Encode())
	if err != nil {
		return err
	}
	return out(r.asJSON, sandboxes, func(w *table) { printSandboxes(w, sandboxes) })
}

func (r *sandboxRun) create(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	if r.class == "" {
		return errors.New("--class is required; 'keera sandbox classes' lists what this " +
			"organisation offers")
	}
	task, err := textOrFile(r.task)
	if err != nil {
		return err
	}
	keys, err := publicKeys(r.sshKey)
	if err != nil {
		return err
	}
	project, err := projectID(ctx, r.c, orgID, r.project)
	if err != nil {
		return err
	}
	body := map[string]any{
		"org_id": orgID, "project_id": project, "name": r.fs.Arg(0), "class": r.class,
		"purpose": r.purpose, "repo": r.repo, "branch": r.branch, "task": task,
		"authorized_keys": keys,
	}
	if r.ttl > 0 {
		body["ttl_seconds"] = int64(r.ttl / time.Second)
	}
	return doOut(ctx, r.c, "POST", "/v1/sandboxes", body, r.asJSON, printSandboxCreated)
}

// agent is `create` with --purpose agent.
func (r *sandboxRun) agent(ctx context.Context) error {
	if r.task == "" {
		return errors.New("--task is required for an agent sandbox; it is the instruction " +
			"the agent starts on, and it is passed to the sandbox rather than stored")
	}
	args := append([]string{"up", r.fs.Arg(0), "--purpose", "agent"}, agentFlags(r.fs)...)
	return sandboxCmd(ctx, args)
}

// agentFlags rebuilds the flags `agent` passes on to `up`, so `agent` gains
// whatever `up` gains. Each goes as --name=value, because "--json true" would
// make "true" a second name.
func agentFlags(fs *flag.FlagSet) []string {
	var out []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "purpose" {
			return
		}
		out = append(out, "--"+f.Name+"="+f.Value.String())
	})
	return out
}

func (r *sandboxRun) show(ctx context.Context) error {
	sb, err := requireSandbox(ctx, r.c, r.org, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return out(r.asJSON, sb, func(w *table) { printSandbox(w, sb) })
}

func (r *sandboxRun) terminate(ctx context.Context) error {
	sb, err := requireSandbox(ctx, r.c, r.org, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !r.yes {
		if err := confirmSandboxTerminate(sb); err != nil {
			return err
		}
	}
	return doOut(ctx, r.c, "DELETE", "/v1/sandboxes/"+url.PathEscape(sb.ID), nil, r.asJSON,
		func(w *table, _ map[string]any) { _, _ = fmt.Fprintf(w, "terminated\t%s\n", sb.Name) })
}

func (r *sandboxRun) extend(ctx context.Context) error {
	sb, err := requireSandbox(ctx, r.c, r.org, r.fs.Arg(0))
	if err != nil {
		return err
	}
	body := map[string]any{}
	if r.ttl > 0 {
		body["ttl_seconds"] = int64(r.ttl / time.Second)
	}
	var res struct {
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	raw, err := keepRaw(ctx, r.c, "POST", "/v1/sandboxes/"+url.PathEscape(sb.ID)+"/extend", body, &res)
	if err != nil {
		return err
	}
	return out(r.asJSON, raw, func(w *table) {
		_, _ = fmt.Fprintf(w, "sandbox\t%s\nexpires\t%s (%s)\n", res.Name,
			res.ExpiresAt.Local().Format(time.RFC3339), expiresIn(&res.ExpiresAt))
	})
}

func (r *sandboxRun) suspendOrResume(ctx context.Context) error {
	sb, err := requireSandbox(ctx, r.c, r.org, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return doOut(ctx, r.c, "POST", "/v1/sandboxes/"+url.PathEscape(sb.ID)+"/"+r.verb, nil, r.asJSON,
		printSandbox)
}

// usage spans every organisation unless one is chosen, as 'keera usage' does.
func (r *sandboxRun) usage(ctx context.Context) error {
	q, err := reportQuery(ctx, r.c, r.org, nil, r.since, map[string]string{"group_by": r.by})
	if err != nil {
		return err
	}
	var res struct {
		Data    []store.SandboxUsage `json:"data"`
		GroupBy string               `json:"group_by"`
	}
	if err := r.c.do(ctx, "GET", "/v1/sandbox-usage?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		printSandboxUsage(w, res.GroupBy, r.since, res.Data)
	})
}

func (r *sandboxRun) apply(ctx context.Context) error {
	entries, err := catalog.LoadSandboxes(r.fs.Arg(0))
	if err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	// One at a time, as the endpoint takes them. The file was checked, so a
	// re-run after a stop halfway is safe.
	classes := make([]policy.SandboxClass, len(entries))
	for i, e := range entries {
		path := aliasPath("sandbox-classes", orgID, e.Name, "")
		if err := r.c.do(ctx, "PUT", path, e, &classes[i]); err != nil {
			return fmt.Errorf("applying %s: %w", e.Name, err)
		}
	}
	return out(r.asJSON, classes, func(w *table) {
		for _, c := range classes {
			_, _ = fmt.Fprintf(w, "applied\t%s\n", c.Name)
		}
		_, _ = fmt.Fprintln(w, "\nA class the file does not name is left alone.")
	})
}

// deleteClass does not affect running sandboxes, which keep their own copy.
func (r *sandboxRun) deleteClass(ctx context.Context) error {
	name := r.fs.Arg(0)
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	if !r.yes {
		if err := confirm("Deleting the sandbox class "+name+":", []string{
			"  nobody can start a new sandbox of this class",
			"  sandboxes already running on it keep working",
		}, "class name", name, "nothing was deleted"); err != nil {
			return err
		}
	}
	var res struct {
		Name          string `json:"name"`
		LiveSandboxes int    `json:"live_sandboxes"`
	}
	raw, err := keepRaw(ctx, r.c, "DELETE", aliasPath("sandbox-classes", orgID, name, ""), nil, &res)
	if err != nil {
		return err
	}
	return out(r.asJSON, raw, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted\t%s\n", res.Name)
		if res.LiveSandboxes > 0 {
			_, _ = fmt.Fprintf(w, "\n%s still running on it.\n",
				plural(res.LiveSandboxes, "sandbox", "sandboxes"))
		}
	})
}

func (r *sandboxRun) classes(ctx context.Context) error {
	// Required, or the list would offer classes the guardrail refuses.
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	var res struct {
		Data   []policy.SandboxClass  `json:"data"`
		Driver map[string]any         `json:"driver"`
		Limits policy.ResolvedSandbox `json:"limits"`
	}
	if err := r.c.do(ctx, "GET", inOrg("/v1/sandbox-classes", orgID), nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		printSandboxClasses(w, res.Data, res.Limits)
		if res.Driver != nil {
			_, _ = fmt.Fprintf(w, "\ndriver\t%v, strongest isolation %v\n",
				res.Driver["name"], res.Driver["isolation"])
		} else {
			_, _ = fmt.Fprintln(w, "\nThis deployment runs no sandbox driver, so none of "+
				"these can be started.")
		}
	})
}

// requireSandbox takes a name, an id or an ssh host alias.
func requireSandbox(ctx context.Context, c *client, org, ref string) (store.Sandbox, error) {
	ref = sandboxRef(ref)
	var sb store.Sandbox
	if id.HasPrefix(ref, "sbx") {
		err := c.do(ctx, "GET", "/v1/sandboxes/"+url.PathEscape(ref), nil, &sb)
		return sb, err
	}
	orgID, err := resolveOrg(ctx, c, org)
	if err != nil {
		return sb, err
	}
	err = c.do(ctx, "GET", inOrg("/v1/sandboxes/"+url.PathEscape(ref), orgID), nil, &sb)
	return sb, err
}

// sshInto runs the developer's own ssh with this command as its ProxyCommand,
// through the gateway's one published port.
func sshInto(ctx context.Context, c *client, org, name string, rest []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this command's own path, which ssh needs for its "+
			"ProxyCommand: %w", err)
	}
	sb, err := requireSandbox(ctx, c, org, name)
	if err != nil {
		return err
	}
	var args []string
	for _, o := range sshOptions(proxyCommand(self, c.base, sb.ID, sb.OrgID)) {
		args = append(args, "-o", o[0]+"="+o[1])
	}
	args = append(append(args, sandboxUser+"@"+sb.Name), rest...)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			// ssh exits with the remote command's status.
			code := exit.ExitCode()
			if code < 0 {
				// Ended by a signal, usually Ctrl-C: 128 plus SIGINT.
				code = 130
			}
			return ExitCode(code)
		}
		return fmt.Errorf("running ssh: %w", err)
	}
	return nil
}

// sshOptions are passed with -o by `sandbox ssh` and printed by `sandbox
// config`.
func sshOptions(proxy string) [][2]string {
	return [][2]string{
		{"ProxyCommand", proxy},
		// Every sandbox from an image shares its host key, and the gateway
		// already authenticates and encrypts the connection.
		{"StrictHostKeyChecking", "no"},
		{"UserKnownHostsFile", "/dev/null"},
		// Otherwise ssh warns about the above every time, which teaches people
		// to ignore its warnings.
		{"LogLevel", "ERROR"},
		// The image refuses it too, but an older one would not.
		{"ForwardAgent", "no"},
	}
}

// proxyCommand names the gateway, because ssh runs it without this
// invocation's --url.
func proxyCommand(self, base, target, orgID string) string {
	return fmt.Sprintf("%s --url %s sandbox proxy %s --org %s",
		proxyWord(self), proxyWord(base), target, orgID)
}

// proxyWord quotes s when needed, as for a path with a space. ssh runs the
// command through a shell, except on Windows, and reads % as a token, so a
// literal one is doubled.
func proxyWord(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	safe := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/_.:=@+,-%"
	if runtime.GOOS == "windows" {
		safe += `\`
	}
	switch {
	case s != "" && strings.Trim(s, safe) == "":
		return s
	case runtime.GOOS == "windows":
		return `"` + s + `"`
	default:
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
}

// sandboxUser matches the uid the drivers run the container under.
const sandboxUser = "keera"

// sshHostPrefix keeps the ssh Host pattern from matching a developer's other
// hosts.
const sshHostPrefix = "sbx-"

// sandboxRef trims the host prefix, because ssh hands a ProxyCommand the whole
// hostname ("sbx-fix-login").
func sandboxRef(s string) string { return strings.TrimPrefix(s, sshHostPrefix) }

// proxyTo copies bytes between the attach surface and the given streams.
func proxyTo(ctx context.Context, c *client, org, ref string, port int,
	in io.Reader, outw io.Writer,
) error {
	ref = sandboxRef(ref)
	endpoint := fmt.Sprintf("%s%s/v1/%s/tcp/%d", c.base, httpx.SandboxPrefix, url.PathEscape(ref), port)
	if !id.HasPrefix(ref, "sbx") {
		orgID, err := resolveOrg(ctx, c, org)
		if err != nil {
			return err
		}
		endpoint = inOrg(endpoint, orgID)
	}

	conn, err := dialUpgrade(ctx, c, endpoint)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// Whichever side closes first ends the process.
	errs := make(chan error, 2)
	go func() { _, err := io.Copy(conn, in); errs <- err }()
	go func() { _, err := io.Copy(outw, conn); errs <- err }()
	if err := <-errs; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// upgradeProtocol must match the one in internal/control/attach.go.
const upgradeProtocol = "keera-sandbox/1"

// dialUpgrade upgrades by hand, because http.Client closes the response body
// and the connection under it is what is wanted.
func dialUpgrade(ctx context.Context, c *client, endpoint string) (net.Conn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	cred, err := c.credential()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", upgradeProtocol)

	conn, err := dialFor(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("reaching the gateway at %s: %w", c.base, err)
	}
	// A dialled connection ignores the context; closing it makes Ctrl-C
	// immediate.
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = conn.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("the gateway refused the connection: %s", refusal(resp, raw))
	}
	// Bytes sent right after the 101, such as the ssh banner, may already be
	// in the reader's buffer.
	return &bufferedConn{Conn: conn, r: br}, nil
}

// dialFor verifies TLS with the system roots; a hand-rolled dial is where that
// gets left off by mistake.
func dialFor(ctx context.Context, u *url.URL) (net.Conn, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	if u.Scheme != "https" {
		return d.DialContext(ctx, "tcp", dialAddress(u))
	}
	td := tls.Dialer{NetDialer: &d, Config: &tls.Config{
		ServerName: u.Hostname(),
		MinVersion: tls.VersionTLS12,
	}}
	return td.DialContext(ctx, "tcp", dialAddress(u))
}

// dialAddress uses the scheme's port when the URL names none.
func dialAddress(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

func refusal(resp *http.Response, raw []byte) string {
	if msg := errorMessage(raw); msg != "" {
		return msg
	}
	return resp.Status
}

// bufferedConn keeps bytes that arrived with the response header.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// writeSSHConfig prints rather than edits ~/.ssh/config, which editing would
// eventually break. Only the block goes to stdout, so it can be appended.
func writeSSHConfig(base, orgID string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	path := filepath.Join("~", ".ssh", "config")
	fmt.Fprintf(os.Stderr, "Append this to %s. Then 'ssh %s<name>' reaches any sandbox you "+
		"own in this organisation, and so does VS Code's Remote-SSH and JetBrains Gateway - they need an ssh "+
		"transport and nothing else.\n\n", path, sshHostPrefix)
	fmt.Printf("Host %s*\n", sshHostPrefix)
	fmt.Printf("  User %s\n", sandboxUser)
	for _, o := range sshOptions(proxyCommand(self, base, "%n", orgID)) {
		fmt.Printf("  %s %s\n", o[0], o[1])
	}
	fmt.Fprintf(os.Stderr, "\nThe ProxyCommand carries the connection over this gateway's own "+
		"port, so there is no second address to publish and no jump host. It signs in "+
		"the way every other keera command does.\n")
	fmt.Fprintf(os.Stderr, "\nHost-key checking is off because there is no host identity "+
		"worth pinning: a sandbox's host key belongs to its image, so every sandbox built "+
		"from the same one presents it. What proves who you are talking to is the gateway, "+
		"which has already authenticated the connection end to end before ssh sees it.\n")
	return nil
}

func printSandboxes(w *table, sandboxes []store.Sandbox) {
	if len(sandboxes) == 0 {
		printNone(w, "sandboxes", "keera sandbox create <name> --class <class>")
		return
	}
	w.header("NAME\tCLASS\tFOR\tSTATE\tISOLATION\tSIZE\tEXPIRES\tRAN\tOWNER")
	for _, sb := range sandboxes {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			sb.Name, sb.Class, sb.Purpose, statusWord(string(sb.State)), sb.Isolation,
			sizeOf(sb.CPU, sb.Memory), expiresIn(sb.ExpiresAt), ranFor(sb.RunningSeconds),
			dash(sb.Owner))
	}
}

func printSandbox(w *table, sb store.Sandbox) {
	show(w, "name", sb.Name)
	show(w, "id", sb.ID)
	show(w, "class", sb.Class)
	show(w, "for", sb.Purpose)
	show(w, "state", statusWord(string(sb.State)))
	showIfSet(w, "detail", sb.Detail)
	show(w, "isolation", sb.Isolation)
	show(w, "size", sizeOf(sb.CPU, sb.Memory))
	show(w, "image", sb.Image)
	showIfSet(w, "repo", sb.Repo)
	showIfSet(w, "branch", sb.Branch)
	showIfSet(w, "node", sb.Node)
	showIfSet(w, "owner", sb.Owner)
	show(w, "created", sb.CreatedAt.Local().Format(time.RFC3339))
	if sb.ExpiresAt != nil {
		_, _ = fmt.Fprintf(w, "expires\t%s (%s)\n",
			sb.ExpiresAt.Local().Format(time.RFC3339), expiresIn(sb.ExpiresAt))
	}
	// Wall time for people, core-seconds for chargeback.
	_, _ = fmt.Fprintf(w, "ran for\t%s (%s core-seconds)\n",
		ranFor(sb.RunningSeconds), ranFor(sb.CoreSeconds()))
	if sb.Purpose == policy.PurposeEngineer && sb.State == policy.SandboxReady {
		_, _ = fmt.Fprintf(w, "\nkeera sandbox ssh %s\n", sb.Name)
	}
}

func printSandboxCreated(w *table, sb store.Sandbox) {
	show(w, "sandbox", sb.Name)
	_, _ = fmt.Fprintf(w, "class\t%s (%s, %s)\n", sb.Class, sb.Isolation, sizeOf(sb.CPU, sb.Memory))
	show(w, "state", statusWord(string(sb.State)))
	if sb.ExpiresAt != nil {
		show(w, "expires", expiresIn(sb.ExpiresAt))
	}
	_, _ = fmt.Fprintf(w, "\nIt is starting. 'keera sandbox show %s' says when it is ready",
		sb.Name)
	if sb.Purpose == policy.PurposeEngineer {
		_, _ = fmt.Fprintf(w, ", and 'keera sandbox ssh %s' gets in", sb.Name)
	}
	_, _ = fmt.Fprintln(w, ".")
	if sb.Purpose == policy.PurposeAgent {
		_, _ = fmt.Fprintln(w, "Nothing attaches to an agent sandbox: what comes out of it "+
			"is a branch.")
	}
}

func printSandboxClasses(w *table, classes []policy.SandboxClass,
	limits policy.ResolvedSandbox,
) {
	if len(classes) == 0 {
		printNone(w, "sandbox classes", "keera sandbox apply <file>")
		return
	}
	w.header("CLASS\tISOLATION\tSIZE\tDISK\tDEFAULT\tMAX\tWARM\tFOR\tYOURS\tWHAT IT IS")
	for _, c := range classes {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			c.Name, c.Isolation, sizeOf(c.CPU, c.Memory), mibOf(c.Disk),
			shortDuration(c.DefaultTTL()), shortDuration(c.MaxTTL()), c.Warm, purposesOf(c),
			yesNo(limits.Admits(c) == nil), dash(c.Description))
	}
}

func printSandboxUsage(w *table, by string, since time.Duration,
	rows []store.SandboxUsage,
) {
	_, _ = fmt.Fprintf(w, "Sandbox time by %s, the last %s\n\n", by, shortDuration(since))
	if len(rows) == 0 {
		printNone(w, "sandboxes in this window", "")
		return
	}
	w.header(strings.ToUpper(by) + "\tSANDBOXES\tLIVE\tRAN\tCORE-SECONDS")
	for _, r := range rows {
		key := dash(r.Group)
		if r.OrgID != "" {
			key += " (" + r.OrgID + ")"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\n",
			key, r.Count, r.Live, ranFor(r.Running), r.CoreSeconds)
	}
}

func confirmSandboxTerminate(sb store.Sandbox) error {
	return confirm("Terminating the sandbox "+sb.Name+":", []string{
		// For every class: a home with no disk behind it is lost too.
		"  its home volume goes with it - anything in /home/" + sandboxUser + " that is not pushed is lost",
		"  its API key is revoked",
		"What it cost and who it belonged to are kept.",
	}, "name", sb.Name, "nothing was terminated")
}

func sizeOf(cpuMillis, memoryMiB int) string {
	return fmt.Sprintf("%s/%s", coresOf(cpuMillis), mibOf(memoryMiB))
}

func coresOf(millis int) string { return policy.FormatCores(millis) + "c" }

func mibOf(mib int) string {
	if mib == 0 {
		return "-"
	}
	return policy.FormatMiB(mib)
}

// expiresIn reads "in 3h12m", which needs no arithmetic, unlike a timestamp.
func expiresIn(at *time.Time) string {
	if at == nil {
		return "never"
	}
	d := time.Until(*at)
	if d <= 0 {
		return "expired"
	}
	return "in " + shortDuration(d.Round(time.Minute))
}

func ranFor(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	return shortDuration(time.Duration(seconds) * time.Second)
}

func purposesOf(c policy.SandboxClass) string {
	if len(c.Purposes) == 0 {
		return "both"
	}
	names := make([]string, 0, len(c.Purposes))
	for _, p := range c.Purposes {
		names = append(names, string(p))
	}
	return strings.Join(names, ",")
}

// publicKeys reads only path when given. Otherwise it sends every *.pub in
// ~/.ssh: it cannot know which key ssh will offer, and accepting all of one
// person's keys is no weaker than accepting one.
func publicKeys(path string) ([]string, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		return splitKeys(string(raw)), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	if err != nil || len(matches) == 0 {
		return nil, nil
	}
	var keys []string
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		keys = append(keys, splitKeys(string(raw))...)
	}
	return keys, nil
}

func splitKeys(raw string) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
