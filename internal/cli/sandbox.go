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
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/store"
)

// `keera sandbox` - the machines this gateway lends out.
//
// Most verbs are JSON over the control API. Two are not: `proxy` copies bytes
// between the attach surface and stdin/stdout, which is what ssh's
// ProxyCommand wants, and `ssh` runs the developer's own ssh with `proxy` as
// that command. An ssh transport is all VS Code's Remote-SSH and JetBrains
// Gateway need, so `keera sandbox config` is the whole client setup.

// sandboxRun is one 'keera sandbox' invocation.
type sandboxRun struct {
	c   *client
	fs  *flag.FlagSet
	sub string

	org     string
	team    string
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
	yes     bool
	asJSON  bool
}

func sandboxCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("sandbox "+sub, flag.ExitOnError)
	r := &sandboxRun{c: newClient(), fs: fs, sub: sub}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.StringVar(&r.team, "team", "", "team the sandbox belongs to, by name or id; its key is scoped to that "+
		"team, so the budget, the rate limit and the allowed models are the team's")
	fs.StringVar(&r.class, "class", "", "which machine to ask for; 'keera sandbox classes' lists them")
	fs.StringVar(&r.purpose, "purpose", "", "'engineer' - a machine you work in - or 'agent' - one task's")
	fs.DurationVar(&r.ttl, "ttl", 0, "how long it lives; the class's default if not given")
	fs.StringVar(&r.repo, "repo", "", "repository to check out into it")
	fs.StringVar(&r.branch, "branch", "", "branch to check out")
	fs.StringVar(&r.task, "task", "", "what an agent sandbox should carry out; @path reads a file")
	fs.IntVar(&r.port, "port", sandbox.PortSSH, "which port to reach through 'proxy'")
	fs.StringVar(&r.sshKey, "ssh-key", "",
		"public key that may open a shell in it; the keys in ~/.ssh if not given")
	fs.BoolVar(&r.all, "all", false, "include sandboxes that have finished")
	fs.StringVar(&r.by, "by", "class", "group 'usage' by class, team or user")
	fs.DurationVar(&r.since, "since", 7*24*time.Hour, "how far back 'usage' looks")
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "sandbox", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "sandbox", want)
	}
	verb, err := parseVerb(fs, "sandbox", sub, rest)
	if err != nil {
		return err
	}
	r.sub = verb

	switch verb {
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
	team, err := teamID(ctx, r.c, orgID, r.team)
	if err != nil {
		return err
	}
	q := url.Values{"org_id": {orgID}}
	if r.all {
		q.Set("all", "1")
	}
	setIfGiven(q, map[string]string{"team_id": team, "class": r.class, "purpose": r.purpose})
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
		return errors.New("--class is required; `keera sandbox classes` lists what this " +
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
	team, err := teamID(ctx, r.c, orgID, r.team)
	if err != nil {
		return err
	}
	body := map[string]any{
		"org_id": orgID, "team_id": team, "name": r.fs.Arg(0), "class": r.class,
		"purpose": r.purpose, "repo": r.repo, "branch": r.branch, "task": task,
		"authorized_keys": keys,
	}
	if r.ttl > 0 {
		body["ttl"] = r.ttl.String()
	}
	var sb store.Sandbox
	if err := r.c.do(ctx, "POST", "/v1/sandboxes", body, &sb); err != nil {
		return err
	}
	return out(r.asJSON, sb, func(w *table) { printSandboxCreated(w, sb) })
}

// agent is `create` with --purpose agent set, so nobody has to type it.
func (r *sandboxRun) agent(ctx context.Context) error {
	if r.task == "" {
		return errors.New("--task is required for an agent sandbox; it is the instruction " +
			"the agent starts on, and it is passed to the sandbox rather than stored")
	}
	args := append([]string{"up", r.fs.Arg(0), "--purpose", "agent"}, agentFlags(r.fs)...)
	return sandboxCmd(ctx, args)
}

// agentFlags rebuilds the flags `keera sandbox agent` passes on to `up`.
// Going back through the parser means `agent` gains whatever `up` gains.
// Each goes as --name=value, because a bool flag does not take the next
// argument: "--json true" would make "true" a second name.
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
	var res map[string]any
	if err := r.c.do(ctx, "DELETE", "/v1/sandboxes/"+url.PathEscape(sb.ID), nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "terminated\t%s\n", sb.Name)
	})
}

func (r *sandboxRun) extend(ctx context.Context) error {
	sb, err := requireSandbox(ctx, r.c, r.org, r.fs.Arg(0))
	if err != nil {
		return err
	}
	body := map[string]any{}
	if r.ttl > 0 {
		body["ttl"] = r.ttl.String()
	}
	var res struct {
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := r.c.do(ctx, "POST", "/v1/sandboxes/"+url.PathEscape(sb.ID)+"/extend",
		body, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "sandbox\t%s\nexpires\t%s (in %s)\n", res.Name,
			res.ExpiresAt.Local().Format(time.RFC3339),
			time.Until(res.ExpiresAt).Round(time.Minute))
	})
}

func (r *sandboxRun) suspendOrResume(ctx context.Context) error {
	sb, err := requireSandbox(ctx, r.c, r.org, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var res store.Sandbox
	if err := r.c.do(ctx, "POST",
		"/v1/sandboxes/"+url.PathEscape(sb.ID)+"/"+r.sub, nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) { printSandbox(w, res) })
}

func (r *sandboxRun) usage(ctx context.Context) error {
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	q := url.Values{
		"org_id": {orgID}, "group_by": {r.by}, "from": {sinceParam(r.since)},
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
	// One class at a time, as the endpoint takes them. The file is checked
	// first, so a stop halfway is the control plane going away, and a re-run
	// is safe.
	classes := make([]policy.SandboxClass, len(entries))
	for i, e := range entries {
		path := inOrg("/v1/sandbox-classes/"+url.PathEscape(e.Name), orgID)
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

// deleteClass removes one of the organisation's classes. Sandboxes already
// running on it keep their own copy of it, so they are not affected.
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
	path := inOrg("/v1/sandbox-classes/"+url.PathEscape(name), orgID)
	if err := r.c.do(ctx, "DELETE", path, nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted\t%s\n", res.Name)
		if res.LiveSandboxes > 0 {
			_, _ = fmt.Fprintf(w, "\n%s still running on it.\n",
				plural(res.LiveSandboxes, "sandbox", "sandboxes"))
		}
	})
}

func (r *sandboxRun) classes(ctx context.Context) error {
	// The organisation is required: the answer carries the caller's own
	// guardrail, and without it the list would offer classes they may not use.
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

// requireSandbox resolves a name, an id or an ssh host alias into a sandbox.
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

// sshInto runs the developer's own ssh - their keys, agent and config - with
// this command as its ProxyCommand, so the connection goes through the
// gateway's one published port.
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
	args := append([]string{
		"-o", "ProxyCommand=" + proxyCommand(self, c.base, sb.ID, sb.OrgID),
		// No host key is worth pinning: it belongs to the image, so every
		// sandbox from that image shares it. The gateway has already
		// authenticated and encrypted the connection.
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		// Otherwise ssh warns about the above on every connection, which
		// teaches people to ignore ssh warnings.
		"-o", "LogLevel=ERROR",
		sandboxUser + "@" + sb.Name,
	}, rest...)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			// ssh exits with the remote command's status, which a script needs.
			os.Exit(exit.ExitCode())
		}
		return fmt.Errorf("running ssh: %w", err)
	}
	return nil
}

// proxyCommand is the ProxyCommand that reaches one sandbox through the
// gateway at base. It names that gateway, so the proxy cannot end up at
// another one: ssh runs it without this invocation's --url.
func proxyCommand(self, base, target, orgID string) string {
	return fmt.Sprintf("%s --url %s sandbox proxy %s --org %s", self, base, target, orgID)
}

// sandboxUser is who a sandbox is entered as. It matches the uid the drivers
// run the container under.
const sandboxUser = "keera"

// sshHostPrefix starts a sandbox's ssh Host pattern. A prefix, so the pattern
// cannot match a developer's other hosts.
const sshHostPrefix = "sbx-"

// sandboxRef reduces what was typed to a name or an id. ssh hands a
// ProxyCommand %n, the whole hostname typed ("sbx-fix-login"), so the host
// prefix is trimmed here rather than in the developer's ssh config.
func sandboxRef(s string) string { return strings.TrimPrefix(s, sshHostPrefix) }

// proxyTo opens the attach surface and copies bytes between it and the given
// streams. As a ProxyCommand those streams are ssh's own.
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

	// Whichever side closes first ends the process: ssh when the session is
	// over, or sshd when the remote command exits.
	errs := make(chan error, 2)
	go func() { _, err := io.Copy(conn, in); errs <- err }()
	go func() { _, err := io.Copy(outw, conn); errs <- err }()
	if err := <-errs; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// upgradeProtocol is the token the attach surface expects. It is copied from
// the control plane rather than imported, so `keera` does not link the gateway.
const upgradeProtocol = "keera-sandbox/1"

// dialUpgrade performs the HTTP upgrade by hand. http.Client cannot: it reads
// and closes the response body, and what is wanted is the connection under it.
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
	// Bytes sent right after the 101 (on a fast sandbox, the ssh banner) are
	// already in the reader's buffer, so the reader stays in front.
	return &bufferedConn{Conn: conn, r: br}, nil
}

// dialFor opens the transport for one URL, TLS or not.
func dialFor(ctx context.Context, u *url.URL) (net.Conn, error) {
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	if u.Scheme != "https" {
		return d.DialContext(ctx, "tcp", host)
	}
	return tlsDial(ctx, &d, host, u.Hostname())
}

// tlsDial opens a TLS connection for the attach surface, which takes the
// connection over after the 101. Verification uses the system roots as
// everywhere else; a hand-rolled dial is where that gets left off by mistake.
func tlsDial(ctx context.Context, d *net.Dialer, address, serverName string) (net.Conn, error) {
	conn, err := tls.DialWithDialer(d, "tcp", address, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	// DialWithDialer ignores the context, so close on cancellation to make
	// Ctrl-C immediate.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	go func() {
		<-ctx.Done()
		if ctx.Err() != nil {
			_ = conn.SetDeadline(time.Now())
		}
	}()
	return conn, nil
}

// refusal renders what the gateway said into one line for a terminal.
func refusal(resp *http.Response, raw []byte) string {
	if msg := errorMessage(raw); msg != "" {
		return msg
	}
	return resp.Status
}

// bufferedConn is a connection with a reader in front of it, so bytes the
// server sent before the response header was read are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// writeSSHConfig prints the block a developer adds to ~/.ssh/config. It is
// printed rather than written, as `keera connect` does: editing somebody's own
// ssh config would eventually break it. The block is on stdout and the rest on
// stderr, so it can be appended straight to the file.
func writeSSHConfig(base, orgID string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	path := filepath.Join("~", ".ssh", "config")
	fmt.Fprintf(os.Stderr, "Append this to %s. Then `ssh %s<name>` reaches any sandbox you "+
		"own, and so does VS Code's Remote-SSH and JetBrains Gateway - they need an ssh "+
		"transport and nothing else.\n\n", path, sshHostPrefix)
	fmt.Printf("Host %s*\n", sshHostPrefix)
	fmt.Printf("  User %s\n", sandboxUser)
	fmt.Printf("  ProxyCommand %s\n", proxyCommand(self, base, "%n", orgID))
	fmt.Printf("  StrictHostKeyChecking no\n")
	fmt.Printf("  UserKnownHostsFile /dev/null\n")
	fmt.Printf("  LogLevel ERROR\n")
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
		_, _ = fmt.Fprintln(w, "No sandboxes. `keera sandbox create <name> --class <class>` "+
			"makes one.")
		return
	}
	w.header("NAME\tCLASS\tFOR\tSTATE\tISOLATION\tSIZE\tEXPIRES\tRAN\tOWNER")
	for _, sb := range sandboxes {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			sb.Name, sb.Class, sb.Purpose, statusWord(string(sb.State)), sb.Isolation,
			sandboxSize(sb), expiresIn(sb.ExpiresAt), ranFor(sb.RunningSeconds),
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
	show(w, "size", sandboxSize(sb))
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
	// Wall time is what people recognise; core-seconds are what chargeback uses.
	_, _ = fmt.Fprintf(w, "ran for\t%s (%s core-seconds)\n",
		ranFor(sb.RunningSeconds), ranFor(sb.CoreSeconds()))
	if sb.Purpose == policy.PurposeEngineer && sb.State == policy.SandboxReady {
		_, _ = fmt.Fprintf(w, "\nkeera sandbox ssh %s\n", sb.Name)
	}
}

// showIfSet writes a row only when there is something to show.
func showIfSet(w *table, name, v string) {
	if v != "" {
		show(w, name, v)
	}
}

func printSandboxCreated(w *table, sb store.Sandbox) {
	show(w, "sandbox", sb.Name)
	_, _ = fmt.Fprintf(w, "class\t%s (%s, %s)\n", sb.Class, sb.Isolation, sandboxSize(sb))
	show(w, "state", statusWord(string(sb.State)))
	if sb.ExpiresAt != nil {
		show(w, "expires", expiresIn(sb.ExpiresAt))
	}
	// It is still starting, so say how to find out when it is ready.
	_, _ = fmt.Fprintf(w, "\nIt is starting. `keera sandbox show %s` says when it is ready",
		sb.Name)
	if sb.Purpose == policy.PurposeEngineer {
		_, _ = fmt.Fprintf(w, ", and `keera sandbox ssh %s` gets in", sb.Name)
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
		_, _ = fmt.Fprintln(w, "This organisation has no sandbox classes.")
		return
	}
	w.header("CLASS\tISOLATION\tSIZE\tDISK\tDEFAULT\tMAX\tWARM\tFOR\tYOURS\tWHAT IT IS")
	for _, c := range classes {
		yours := "yes"
		if err := limits.Admits(c); err != nil {
			yours = "no"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			c.Name, c.Isolation, sizeOf(c.CPU, c.Memory), mibOf(c.Disk),
			c.DefaultTTL, c.MaxTTL, c.Warm, purposesOf(c), statusWord(yours),
			dash(c.Description))
	}
}

func printSandboxUsage(w *table, by string, since time.Duration,
	rows []store.SandboxUsage,
) {
	_, _ = fmt.Fprintf(w, "Sandbox time by %s, the last %s\n\n", by, since)
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "No sandboxes in this window.")
		return
	}
	w.header(strings.ToUpper(by) + "\tSANDBOXES\tLIVE\tRAN\tCORE-SECONDS")
	for _, r := range rows {
		key := dash(r.Key)
		if r.OrgID != "" {
			key += " (" + r.OrgID + ")"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\n",
			key, r.Count, r.Live, ranFor(r.Running), r.CoreSeconds)
	}
}

func confirmSandboxTerminate(sb store.Sandbox) error {
	return confirm("Terminating the sandbox "+sb.Name+":", []string{
		// Said for every class: a home with no disk behind it is lost too.
		"  its home volume goes with it - anything in /home/" + sandboxUser + " that is not pushed is lost",
		"  its API key is revoked",
		"What it cost and who it belonged to are kept.",
	}, "name", sb.Name, "nothing was terminated")
}

func sandboxSize(sb store.Sandbox) string { return sizeOf(sb.CPU, sb.Memory) }

func sizeOf(cpuMillis, memoryMiB int) string {
	return fmt.Sprintf("%s/%s", coresOf(cpuMillis), mibOf(memoryMiB))
}

func coresOf(millis int) string {
	if millis%1000 == 0 {
		return fmt.Sprintf("%dc", millis/1000)
	}
	return fmt.Sprintf("%.1fc", float64(millis)/1000)
}

// mibOf renders a size in MiB, of memory or of disk.
func mibOf(mib int) string {
	switch {
	case mib == 0:
		return "-"
	case mib%1024 == 0:
		return fmt.Sprintf("%dGi", mib/1024)
	default:
		return fmt.Sprintf("%dMi", mib)
	}
}

// expiresIn is how long a sandbox has left. "in 3h12m" needs no arithmetic,
// unlike a timestamp.
func expiresIn(at *time.Time) string {
	if at == nil {
		return "never"
	}
	d := time.Until(*at)
	if d <= 0 {
		return "expired"
	}
	return "in " + d.Round(time.Minute).String()
}

func ranFor(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
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

// publicKeys is what may open a shell in a new sandbox.
//
// Given a path, it reads only that file, so somebody with several identities
// can pick one. Otherwise it sends every *.pub in ~/.ssh: this command cannot
// know which key ssh will offer, and accepting all of one person's own keys
// is no weaker than accepting one.
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

// splitKeys pulls the key lines out of a file, dropping comments and blanks.
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
