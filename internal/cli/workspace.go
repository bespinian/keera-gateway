package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// `keera workspace` works with Keera Agents' workspaces: machines of one's
// own, with a coding agent, that run next to the gateway. Keera Agents is a
// separate app; /v1/me names its address, and it takes the same sign-in.
//
// A workspace takes no connection from outside. `open`, `forward` and
// `ssh-proxy` ask Keera Agents for a stream to a port inside it: an HTTP/1.1
// request answered 101, then raw bytes both ways.

type workspaceRun struct {
	*cmdRun
	terminal, vscode  bool
	model             string
	cpu, memory, disk string
	all               bool
	port              int
	noBrowser         bool
}

func workspaceCmd(ctx context.Context, args []string) error {
	r := &workspaceRun{cmdRun: newCmdRun("workspace", args)}
	fs := r.fs
	fs.BoolVar(&r.terminal, "terminal", false, "pi, the coding agent, in a terminal in the browser")
	fs.BoolVar(&r.vscode, "vscode", false, "VS Code in the browser, with Cline")
	fs.StringVar(&r.model, "model", "", "the model the coding agent starts with; any the key may use can be picked later")
	fs.StringVar(&r.cpu, "cpu", "", "CPUs, such as 2 or 500m")
	fs.StringVar(&r.memory, "memory", "", "memory, such as 4Gi")
	fs.StringVar(&r.disk, "disk", "", "the workspace's disk, such as 20Gi; fixed once it exists")
	fs.BoolVar(&r.all, "all", false, "every workspace in the organisation, for an administrator")
	fs.IntVar(&r.port, "port", 0, "the port on this machine; 0 picks a free one")
	fs.BoolVar(&r.noBrowser, "no-browser", false, "do not open a browser; the address is printed either way")
	if done, err := r.parse(); done {
		return err
	}
	switch r.verb {
	case "create":
		return r.create(ctx)
	case "show":
		return r.show(ctx)
	case "start", "stop":
		return r.setRunning(ctx, r.verb)
	case "delete":
		return r.delete(ctx)
	case "open":
		return r.open(ctx)
	case "forward":
		return r.forward(ctx)
	case "ssh":
		return r.ssh(ctx)
	case "ssh-config":
		return r.sshConfig(ctx)
	case "ssh-proxy":
		return r.sshProxy(ctx)
	default:
		return r.list(ctx)
	}
}

// workspace is a workspace as Keera Agents answers it.
type workspace struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Experience  string `json:"experience"`
	Model       string `json:"model"`
	State       string `json:"state"`
	StateDetail string `json:"state_detail"`
	Enabled     bool   `json:"enabled"`
	OwnerEmail  string `json:"owner_email"`
	Class       struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
		Disk   string `json:"disk"`
	} `json:"class"`
}

// agents is Keera Agents, reached with this machine's sign-in.
type agents struct {
	base  string
	token string
	http  *http.Client
}

// agents finds Keera Agents through the gateway. Only a person's sign-in
// works there: the operator key belongs to nobody, and a workspace is
// somebody's.
func (r *workspaceRun) agents(ctx context.Context) (*agents, error) {
	c := r.c
	if c.key != "" {
		return nil, errors.New("workspaces are a person's: unset KEERA_OPERATOR_KEY and run 'keera login'")
	}
	token, err := c.credential()
	if err != nil {
		return nil, err
	}
	var me struct {
		AgentsURL string `json:"agents_url"`
	}
	if err := c.do(ctx, "GET", "/v1/me", nil, &me); err != nil {
		return nil, err
	}
	if me.AgentsURL == "" {
		return nil, fmt.Errorf("%s has no Keera Agents connected, so it has no workspaces", c.base)
	}
	return &agents{
		base:  strings.TrimRight(me.AgentsURL, "/"),
		token: token,
		// HTTP/1.1 only: a stream takes the connection over, which HTTP/2
		// cannot.
		http: &http.Client{Transport: &http.Transport{
			Proxy:        http.ProxyFromEnvironment,
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		}},
	}, nil
}

func (a *agents) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := jsonRequest(ctx, method, a.base+path, in)
	if err != nil {
		return err
	}
	resp, err := a.send(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return agentsError(resp.StatusCode, raw)
	}
	return decode(raw, nil, out)
}

// send sends req with this machine's sign-in.
func (a *agents) send(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching Keera Agents at %s: %w", a.base, err)
	}
	return resp, nil
}

func agentsError(status int, raw []byte) error {
	if status == http.StatusUnauthorized {
		return errors.New("your sign-in was not accepted by Keera Agents: run 'keera login' again")
	}
	return &apiError{Status: status, Message: errorMessage(raw)}
}

// stream opens a connection to port in the workspace: web, ssh or a number.
// Keera Agents starts a workspace that is not running first, which can take
// a minute or two.
func (a *agents) stream(ctx context.Context, name, port string) (io.ReadWriteCloser, error) {
	u := a.base + workspacePath(name) + "/connect?port=" + url.QueryEscape(port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "keera-stream")
	resp, err := a.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == http.StatusOK {
			return nil, errors.New("a proxy in front of Keera Agents does not pass connections through; " +
				"it must pass Upgrade requests on, as for WebSockets")
		}
		return nil, agentsError(resp.StatusCode, raw)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, errors.New("the answer cannot carry a connection")
	}
	return rwc, nil
}

func workspacePath(name string) string { return "/api/v1/workspaces/" + url.PathEscape(name) }

// printWorkspaces names each one's owner when the list has other people's.
func printWorkspaces(w *table, owners bool, list ...workspace) {
	head := "NAME\tEXPERIENCE\tSTATE\tMODEL\tID"
	if owners {
		head += "\tOWNER"
	}
	w.header(head)
	for _, ws := range list {
		state := ws.State
		if !ws.Enabled {
			state = "stopped"
		}
		row := ws.Name + "\t" + experienceLabel(ws.Experience) + "\t" + state + "\t" + ws.Model + "\t" + ws.ID
		if owners {
			row += "\t" + ws.OwnerEmail
		}
		_, _ = fmt.Fprintln(w, row)
	}
}

func experienceLabel(e string) string {
	if e == "vscode" {
		return "VS Code"
	}
	return "terminal"
}

func (r *workspaceRun) list(ctx context.Context) error {
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	path := "/api/v1/workspaces"
	if r.all {
		path += "?all=true"
	}
	var res struct {
		Data []workspace `json:"data"`
	}
	if err := a.do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res.Data, func(w *table) {
		if len(res.Data) == 0 {
			printNone(w, "workspaces", "keera workspace create <name>")
			return
		}
		printWorkspaces(w, r.all, res.Data...)
	})
}

func (r *workspaceRun) show(ctx context.Context) error {
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	var ws workspace
	if err := a.do(ctx, "GET", workspacePath(r.fs.Arg(0)), nil, &ws); err != nil {
		return err
	}
	return out(r.asJSON, ws, func(w *table) {
		printWorkspaces(w, false, ws)
		if ws.StateDetail != "" {
			_, _ = fmt.Fprintf(w, "\n%s\n", ws.StateDetail)
		}
	})
}

// chooseExperience asks when neither flag says, as a workspace has one or
// the other.
func (r *workspaceRun) chooseExperience() (string, error) {
	switch {
	case r.terminal && r.vscode:
		return "", errors.New("choose one: --terminal or --vscode")
	case r.terminal:
		return "terminal", nil
	case r.vscode:
		return "vscode", nil
	}
	if !isTerminal(os.Stdin) {
		return "", errors.New("say what to open in it: --terminal (pi) or --vscode (VS Code with Cline)")
	}
	return askExperience(os.Stdin, os.Stderr)
}

func askExperience(in io.Reader, w io.Writer) (string, error) {
	_, _ = fmt.Fprintln(w, styleErr.head("What do you want to work in?"))
	_, _ = fmt.Fprintln(w, "  1  Terminal: pi, the coding agent, in a terminal in your browser")
	_, _ = fmt.Fprintln(w, "  2  VS Code: VS Code in your browser, with the Cline coding agent")
	_, _ = fmt.Fprint(w, styleErr.head("Choose 1 or 2: "))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the choice: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "1", "t", "terminal", "pi":
		return "terminal", nil
	case "2", "v", "vscode", "vs code", "code":
		return "vscode", nil
	}
	return "", errors.New("choose 1 for the terminal or 2 for VS Code")
}

func (r *workspaceRun) create(ctx context.Context) error {
	experience, err := r.chooseExperience()
	if err != nil {
		return err
	}
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{"name": r.fs.Arg(0), "experience": experience}
	if r.model != "" {
		body["model"] = r.model
	}
	if r.cpu != "" || r.memory != "" || r.disk != "" {
		body["class"] = map[string]string{"cpu": r.cpu, "memory": r.memory, "disk": r.disk}
	}
	var ws workspace
	if err := a.do(ctx, "POST", "/api/v1/workspaces", body, &ws); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, ws, nil)
	}
	fmt.Printf("Created %s. It is starting; open it with:\n\n  keera workspace open %s\n\n"+
		"or reach it over SSH after 'keera workspace ssh-config' with:\n\n  ssh %s.keera\n",
		ws.Name, ws.Name, ws.Name)
	return nil
}

// setRunning starts or stops a workspace; verb is "start" or "stop".
func (r *workspaceRun) setRunning(ctx context.Context, verb string) error {
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	var ws workspace
	if err := a.do(ctx, "POST", workspacePath(r.fs.Arg(0))+"/"+verb, struct{}{}, &ws); err != nil {
		return err
	}
	return out(r.asJSON, ws, func(w *table) {
		printWorkspaces(w, false, ws)
	})
}

func (r *workspaceRun) delete(ctx context.Context) error {
	name := r.fs.Arg(0)
	if !r.yes {
		if err := confirm("Delete the workspace "+name+"?", []string{
			"Its machine and every file in it are deleted, and its key is revoked.",
		}, "workspace's name", name, "nothing was deleted"); err != nil {
			return err
		}
	}
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	if err := a.do(ctx, "DELETE", workspacePath(name), nil, nil); err != nil {
		return err
	}
	if r.asJSON {
		return out(true, map[string]string{"deleted": name}, nil)
	}
	fmt.Printf("Deleted %s.\n", name)
	return nil
}

// ------------------------------------------------------------ forwarding

// open forwards the workspace's experience to this machine and opens it.
func (r *workspaceRun) open(ctx context.Context) error {
	name := r.fs.Arg(0)
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	// One stream first, so a suspended workspace is up before the browser
	// asks, and a mistake shows here rather than as a blank page.
	fmt.Fprintf(os.Stderr, "Connecting to %s; a workspace that was asleep takes a minute to start...\n", name)
	probe, err := a.stream(ctx, name, "web")
	if err != nil {
		return err
	}
	_ = probe.Close()
	return listen(ctx, a, name, "web", r.port, func(addr string) {
		u := "http://" + addr + "/"
		fmt.Fprintf(os.Stderr, "\n%s is at %s\nKeep this running while you work; Ctrl-C ends it.\n",
			name, styleErr.cmd(u))
		if !r.noBrowser {
			openInBrowser(u)
		}
	})
}

// forward forwards any port in the workspace, as for a dev server.
func (r *workspaceRun) forward(ctx context.Context) error {
	name, remote := r.fs.Arg(0), r.fs.Arg(1)
	if n, err := strconv.Atoi(remote); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q is not a port; give a number, such as 3000", remote)
	}
	local := r.port
	if arg := r.fs.Arg(2); arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 || n > 65535 {
			return fmt.Errorf("%q is not a port on this machine", arg)
		}
		local = n
	}
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	return listen(ctx, a, name, remote, local, func(addr string) {
		fmt.Fprintf(os.Stderr, "Port %s of %s is at %s\nCtrl-C ends it.\n", remote, name, styleErr.cmd(addr))
	})
}

// listen accepts connections on the loopback of this machine, as
// `kubectl port-forward` does, and passes each through to port in the
// workspace.
func listen(ctx context.Context, a *agents, name, port string, local int, ready func(string)) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		return fmt.Errorf("listening on this machine: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	ready(ln.Addr().String())
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			remote, err := a.stream(ctx, name, port)
			if err != nil {
				fmt.Fprintln(os.Stderr, styleErr.bad("keera:"), err)
				return
			}
			join(conn, remote)
		}()
	}
}

// join copies both ways until either side ends, then closes both.
func join(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	pipe := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// ------------------------------------------------------------ ssh

// sshHostSuffix is what ssh names a workspace by: ssh <name>.keera.
const sshHostSuffix = ".keera"

// sshProxy is ssh's ProxyCommand: it joins stdin and stdout to the
// workspace's SSH server.
func (r *workspaceRun) sshProxy(ctx context.Context) error {
	name := strings.TrimSuffix(r.fs.Arg(0), sshHostSuffix)
	a, err := r.agents(ctx)
	if err != nil {
		return err
	}
	conn, err := a.stream(ctx, name, "ssh")
	if err != nil {
		return err
	}
	join(stdio{}, conn)
	return nil
}

// stdio is this process's stdin and stdout as one connection.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error {
	_ = os.Stdin.Close()
	return os.Stdout.Close()
}

// sshPath is a file in ~/.ssh.
func sshPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", name), nil
}

// ensureSSHKey makes this machine's key if there is none, and registers its
// public half with Keera Agents.
func ensureSSHKey(ctx context.Context, a *agents) (string, error) {
	key, err := sshPath("keera_ed25519")
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	if _, err := os.Stat(key); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "keera "+host, "-f", key)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("making an SSH key with ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", fmt.Errorf("reading the public key: %w", err)
	}
	err = a.do(ctx, "POST", "/api/v1/me/ssh-keys", map[string]string{
		"name": host, "public_key": strings.TrimSpace(string(pub)),
	}, nil)
	if apiErr, ok := errors.AsType[*apiError](err); ok && apiErr.Status == http.StatusConflict {
		err = nil // registered already
	}
	return key, err
}

// The block sshConfig writes is marked, so running it again replaces it.
const (
	sshBlockStart = "# keera workspaces: written by 'keera workspace ssh-config'"
	sshBlockEnd   = "# end of keera workspaces"
)

// sshBlock is the ssh configuration for every workspace. --url is written
// in, so it reaches this gateway whatever else this machine signs in to.
func sshBlock(keera, base, key string) string {
	return strings.Join([]string{
		sshBlockStart,
		"Host *" + sshHostSuffix,
		"  ProxyCommand " + proxyCommand(keera, base),
		"  User agent",
		"  IdentityFile " + quoteSSH(key),
		"  IdentitiesOnly yes",
		sshBlockEnd,
	}, "\n") + "\n"
}

// proxyCommand is how ssh reaches a workspace through this gateway.
func proxyCommand(keera, base string) string {
	return quoteSSH(keera) + " --url " + quoteSSH(base) + " workspace ssh-proxy %h"
}

// quoteSSH quotes a value for ssh_config when it holds a space.
func quoteSSH(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// withSSHBlock puts block at the top of an ssh config, replacing an older
// one. At the top, since ssh takes the first value it finds for each
// option, and a 'Host *' further up would otherwise win.
func withSSHBlock(config, block string) string {
	if start := strings.Index(config, sshBlockStart); start >= 0 {
		if end := strings.Index(config[start:], sshBlockEnd); end >= 0 {
			rest := strings.TrimLeft(config[start+end+len(sshBlockEnd):], "\n")
			config = config[:start] + rest
		}
	}
	config = strings.TrimLeft(config, "\n")
	if config == "" {
		return block
	}
	return block + "\n" + config
}

// sshSetup registers this machine's key, and finds this program, which ssh
// runs as its ProxyCommand.
func (r *workspaceRun) sshSetup(ctx context.Context) (key, keera string, err error) {
	a, err := r.agents(ctx)
	if err != nil {
		return "", "", err
	}
	if key, err = ensureSSHKey(ctx, a); err != nil {
		return "", "", err
	}
	keera, err = os.Executable()
	return key, keera, err
}

func (r *workspaceRun) sshConfig(ctx context.Context) error {
	key, keera, err := r.sshSetup(ctx)
	if err != nil {
		return err
	}
	path, err := sshPath("config")
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated := withSSHBlock(string(old), sshBlock(keera, r.c.base, key))
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote the workspaces' entry in %s, and added %s.pub to your keys.\n"+
		"Reach a workspace with:\n\n  ssh <name>%s\n", path, key, sshHostSuffix)
	return nil
}

// ssh signs in to a workspace, without needing ssh-config first.
func (r *workspaceRun) ssh(ctx context.Context) error {
	name := r.fs.Arg(0)
	key, keera, err := r.sshSetup(ctx)
	if err != nil {
		return err
	}
	args := []string{
		"-o", "ProxyCommand=" + proxyCommand(keera, r.c.base),
		"-o", "IdentitiesOnly=yes", "-i", key, "-l", "agent", name + sshHostSuffix,
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
