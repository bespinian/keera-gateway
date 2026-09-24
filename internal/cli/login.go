package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
)

// Signing in at a terminal.
//
// KEERA_OPERATOR_KEY is shared, never expires and can do everything, and the
// audit log cannot say who used it. `keera login` signs a person in through
// the deployment's identity provider instead, and stores a credential that is
// theirs, carries their role and expires. The operator key stays for
// automation and for setting up a new deployment.
//
// The flow is RFC 8252's loopback redirect; cliauth.go on the gateway is the
// other half.

// loginCmd signs this machine in to a gateway.
func loginCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	provider := fs.String("provider", "",
		"which identity provider to sign in through, where a deployment offers several")
	noBrowser := fs.Bool("no-browser", false,
		"print the sign-in URL instead of opening it")
	fs.Usage = func() { _ = printHelp(fs, "login", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "login", want)
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	// Run has already taken the global --url, so the client uses it.
	c := newClient()

	name, err := chooseProvider(ctx, c, *provider)
	if err != nil {
		return err
	}

	// Listen first: the sign-in URL has to name the port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listening for the sign-in to come back: %w", err)
	}
	defer func() { _ = ln.Close() }()

	verifier, err := authn.NewCLIVerifier()
	if err != nil {
		return err
	}
	state, err := authn.NewCLIVerifier()
	if err != nil {
		return err
	}
	redirect := "http://" + ln.Addr().String() + "/callback"

	q := url.Values{
		"cli_redirect":  {redirect},
		"cli_challenge": {authn.CLIChallenge(verifier)},
		"cli_state":     {state},
	}
	if name != "" {
		q.Set("provider", name)
	}
	signInURL := c.base + httpx.ControlPrefix + "/auth/login?" + q.Encode()

	// Always printed, for a machine with no browser or a session over ssh.
	fmt.Fprintf(os.Stderr, "Opening %s\n\n", styleErr.head(c.base))
	fmt.Fprintf(os.Stderr, "If your browser does not open, go to:\n\n  %s\n\n",
		styleErr.cmd(signInURL))
	if !*noBrowser {
		openInBrowser(signInURL)
	}

	handed, err := waitForCallback(ctx, ln, state)
	if err != nil {
		return err
	}

	var token struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.anon(ctx, "POST", "/auth/cli/token", map[string]string{
		"code": handed, "verifier": verifier, "label": machineLabel(),
	}, &token); err != nil {
		return err
	}

	// Store the token before anything else can fail.
	cred := signIn{Token: token.Token, ExpiresAt: token.ExpiresAt}
	if err := saveSignIn(c.base, cred); err != nil {
		return fmt.Errorf("storing the sign-in: %w", err)
	}
	// Ask with the new token, not an operator key the shell may hold.
	c.key, c.signedIn = "", cred

	me, err := whoami(ctx, c)
	if err != nil {
		return err
	}
	// Keep the address too, so whoami can answer without a round trip.
	cred.Email = me.Email
	if err := saveSignIn(c.base, cred); err != nil {
		return fmt.Errorf("storing the sign-in: %w", err)
	}

	fmt.Printf("Signed in to %s as %s.\n", c.base, describe(me))
	// saveSignIn made this the default gateway.
	fmt.Printf("Commands on this machine now talk to it; 'keera whoami' says which.\n")
	fmt.Printf("This sign-in expires on %s. 'keera logout' ends it sooner.\n",
		token.ExpiresAt.Local().Format("2 January 2006"))
	if os.Getenv("KEERA_OPERATOR_KEY") != "" {
		// Otherwise nothing they run next uses the sign-in, and nothing says so.
		fmt.Fprintln(os.Stderr, styleErr.warn("\nNote: KEERA_OPERATOR_KEY is set in this "+
			"shell, and it wins. Unset it to run commands as yourself."))
	}
	return nil
}

// logoutCmd ends this machine's sign-in, and with --all every one this person
// holds.
func logoutCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	all := fs.Bool("all", false,
		"end every sign-in this account holds, on every machine and in every browser")
	fs.Usage = func() { _ = printHelp(fs, "logout", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "logout", want)
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case c.signedIn.Token == "" && c.key == "":
		fmt.Printf("Not signed in to %s.\n", c.base)
		return nil
	case c.signedIn.Token == "":
		// The operator key is not a sign-in, and cannot be ended from here.
		return fmt.Errorf("not signed in to %s: this shell is using "+
			"KEERA_OPERATOR_KEY, which is the deployment's own credential. "+
			"Unset it to stop using it", c.base)
	}

	path := "/auth/logout"
	if *all {
		path += "?all=1"
	}
	// Clear the local file whatever the gateway says, so a token it already
	// forgot does not stay behind.
	err := c.do(ctx, "POST", path, nil, nil)
	if c.signedIn.Token != "" {
		if forget := forgetSignIn(c.base); forget != nil {
			return forget
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "The gateway did not confirm the sign-out (%v), "+
			"but this machine's credential has been deleted.\n", err)
		return nil
	}
	if *all {
		fmt.Printf("Signed out of %s everywhere.\n", c.base)
		return nil
	}
	fmt.Printf("Signed out of %s.\n", c.base)
	return nil
}

// whoamiCmd says who the credential in use belongs to, and which gateway.
func whoamiCmd(ctx context.Context, args []string) error {
	c := newClient()
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print raw JSON")
	fs.Usage = func() { _ = printHelp(fs, "whoami", "") }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "whoami", want)
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	me, err := whoami(ctx, c)
	if err != nil {
		return err
	}
	if *asJSON {
		return out(true, me, nil)
	}
	fmt.Printf("%s at %s\n", describe(me), c.base)
	// A machine can be signed in to several gateways, so say why this one.
	fmt.Printf("That address came from %s.\n", c.baseFrom)
	switch {
	case c.key != "":
		fmt.Println("Signed in with KEERA_OPERATOR_KEY, which is the deployment's own credential.")
	case !c.signedIn.ExpiresAt.IsZero():
		fmt.Printf("This sign-in expires on %s.\n",
			c.signedIn.ExpiresAt.Local().Format("2 January 2006"))
	}
	return nil
}

// identity is what /v1/me says, the same answer the panel gets.
type identity struct {
	Email   string `json:"email"`
	Role    string `json:"role"`
	OrgID   string `json:"org_id"`
	OrgName string `json:"org_name"`
	Via     string `json:"via"`
	// Sandboxes is whether the deployment lends out sandboxes.
	Sandboxes bool `json:"sandboxes"`
}

func whoami(ctx context.Context, c *client) (identity, error) {
	var me identity
	err := c.do(ctx, "GET", "/v1/me", nil, &me)
	return me, err
}

// describe is one line naming the person, their role and their organisation.
func describe(me identity) string {
	who := me.Email
	if who == "" {
		// The operator key is nobody, as the audit log says too.
		who = "the operator key"
	}
	line := who
	if me.Role != "" {
		line += " (" + me.Role + ")"
	}
	switch {
	case me.OrgName != "":
		line += " in " + me.OrgName
	case me.OrgID != "":
		line += " in " + me.OrgID
	}
	return line
}

// chooseProvider settles which identity provider to sign in through before a
// browser is opened. With several and none named, it refuses rather than
// guess: the wrong one can put an account in the wrong tenant.
func chooseProvider(ctx context.Context, c *client, named string) (string, error) {
	var cfg struct {
		SSO       bool `json:"sso"`
		Providers []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
		} `json:"providers"`
		OperatorKey bool `json:"operator_key"`
	}
	if err := c.anon(ctx, "GET", "/auth/config", nil, &cfg); err != nil {
		return "", err
	}
	if !cfg.SSO {
		return "", fmt.Errorf("%s has no identity provider configured, so there is "+
			"nothing to sign in to; use KEERA_OPERATOR_KEY, or configure one - see docs/sso.md",
			c.base)
	}
	names := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		names = append(names, p.Name)
	}
	if named == "" {
		if len(names) == 1 {
			return names[0], nil
		}
		return "", fmt.Errorf("%s has more than one identity provider, so --provider has "+
			"to name one of: %s", c.base, strings.Join(names, ", "))
	}
	for _, n := range names {
		if strings.EqualFold(n, named) {
			return n, nil
		}
	}
	return "", fmt.Errorf("%q is not an identity provider on %s; it offers: %s",
		named, c.base, strings.Join(names, ", "))
}

// waitForCallback serves the loopback listener until the browser arrives with
// a code, and shows the person a page they can close.
func waitForCallback(ctx context.Context, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// The state proves this is the sign-in this process started.
		if q.Get("state") != state {
			finishInBrowser(w, http.StatusBadRequest, "That sign-in was not the one this "+
				"terminal started. Run 'keera login' again.")
			return
		}
		code := q.Get("code")
		if code == "" {
			finishInBrowser(w, http.StatusBadRequest,
				"That sign-in came back without a code. Run 'keera login' again.")
			return
		}
		finishInBrowser(w, http.StatusOK, "You are signed in. You can close this tab "+
			"and go back to the terminal.")
		select {
		case done <- result{code: code}:
		default:
		}
	})
	// Anything else on this port is a stray request, not a sign-in.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		finishInBrowser(w, http.StatusNotFound, "Nothing here. This port is waiting for one "+
			"sign-in and closes afterwards.")
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case done <- result{err: err}:
			default:
			}
		}
	}()
	defer func() {
		// A moment for the browser to get its page before the server stops.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	// Wait as long as the gateway allows the whole sign-in, not just the code:
	// the person may still be at a password or consent screen.
	timeout := time.NewTimer(authn.FlowTTL)
	defer timeout.Stop()

	select {
	case r := <-done:
		return r.code, r.err
	case <-timeout.C:
		return "", errors.New("gave up waiting for the sign-in to come back; " +
			"run 'keera login' again")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// finishInBrowser is the one-sentence page the person is left looking at.
//
// It is always dark, in the panel's dark colours, to avoid a flash of white
// during a terminal session. The colours are inlined because the panel's
// stylesheet lives on the gateway, not on this loopback port.
func finishInBrowser(w http.ResponseWriter, status int, message string) {
	// The panel's --bad for anything that went wrong, its --text otherwise.
	colour := "#ececef"
	if status != http.StatusOK {
		colour = "#ef7b70"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Keera</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: dark }
  body {
    margin: 0; min-height: 100vh; display: grid; place-items: center;
    padding: 2rem; background: #121215; color: #ececef;
    font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Inter, Roboto,
      "Helvetica Neue", Arial, sans-serif;
  }
  main {
    max-width: 28rem; padding: 2rem 2.25rem; border-radius: 10px;
    background: #1a1a1e; border: 1px solid #2b2b32;
    box-shadow: 0 1px 2px rgb(0 0 0 / 40%%), 0 10px 30px -14px rgb(0 0 0 / 70%%);
  }
  h1 {
    margin: 0 0 0.75rem; font-size: 0.8125rem; font-weight: 600;
    letter-spacing: 0.08em; text-transform: uppercase; color: #9c9ba4;
  }
  p { margin: 0; color: %s }
</style></head>
<body><main><h1>Keera</h1><p>%s</p></main></body></html>`, colour, message)
}

// openInBrowser sends somebody to the sign-in. It is a variable so a test can
// stand in for the browser and drive a whole sign-in.
var openInBrowser = openBrowser

// openBrowser asks the desktop to open a URL, and says nothing when it
// cannot: the URL has already been printed.
func openBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return
	}
	// Reaped in the background, because some browsers never return.
	go func() { _ = cmd.Wait() }()
}

// machineLabel names this machine in the sign-in, so an audit entry says
// which terminal it was. It decides nothing.
func machineLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if who := os.Getenv(key); who != "" {
			return who + "@" + host
		}
	}
	return host
}
