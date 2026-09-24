package cli

import (
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
)

// The credentials file is a credential, so it is written where only its owner
// can read it. A file mode nobody checks is a file mode that drifts.
func TestTheCredentialsFileIsReadableOnlyByItsOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KEERA_CONFIG_DIR", filepath.Join(dir, "keera"))

	if err := saveSignIn("http://127.0.0.1:8080", signIn{
		Token: "keera_cli_secret", ExpiresAt: time.Now().Add(time.Hour), Email: "alice@example.ch",
	}); err != nil {
		t.Fatalf("storing the sign-in: %v", err)
	}
	path, err := credentialsPath()
	if err != nil {
		t.Fatalf("credentialsPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %v, want 0600", got)
	}
	if got := dirMode(t, filepath.Dir(path)); got != 0o700 {
		t.Errorf("directory mode = %v, want 0700", got)
	}
}

func dirMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.Mode().Perm()
}

// A machine can be signed in to several gateways. Signing in to or out of one
// must not touch the others.
func TestGatewaysAreSignedInToIndependently(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())

	prod := "https://keera.example.ch"
	local := "http://127.0.0.1:8080"
	for base, token := range map[string]string{prod: "keera_cli_prod", local: "keera_cli_local"} {
		if err := saveSignIn(base, signIn{Token: token, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatalf("storing the sign-in to %s: %v", base, err)
		}
	}
	if got := signInFor(prod).Token; got != "keera_cli_prod" {
		t.Errorf("token for %s = %q, want the one it was signed in with", prod, got)
	}

	if err := forgetSignIn(local); err != nil {
		t.Fatalf("signing out of %s: %v", local, err)
	}
	if got := signInFor(local).Token; got != "" {
		t.Errorf("token for %s = %q after signing out, want none", local, got)
	}
	if got := signInFor(prod).Token; got != "keera_cli_prod" {
		t.Errorf("signing out of one gateway took the other with it: %q", got)
	}
}

// The same gateway written two ways is one gateway. Otherwise a second sign-in
// is filed where the first one is not looked for, and somebody is asked to sign
// in again by a trailing slash.
func TestOneGatewayWrittenTwoWaysIsOneSignIn(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())

	if err := saveSignIn("https://Keera.example.ch/", signIn{Token: "keera_cli_one"}); err != nil {
		t.Fatalf("storing the sign-in: %v", err)
	}
	if got := signInFor("https://keera.example.ch").Token; got != "keera_cli_one" {
		t.Errorf("token = %q, want the sign-in that was stored under the same gateway", got)
	}
}

// A machine that has never signed in is not an error, and neither is one whose
// file was deleted. Both are just "not signed in", which every command says for
// itself with something to do about it.
func TestAMachineThatNeverSignedInReadsAsEmpty(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", filepath.Join(t.TempDir(), "never-written"))

	if got := signInFor("http://127.0.0.1:8080"); got.Token != "" {
		t.Errorf("token = %q, want none", got.Token)
	}
}

// An expired sign-in is refused here rather than at the gateway, so that the
// message says what to do about it instead of "sign in".
func TestAnExpiredSignInIsRefusedBeforeItIsSpent(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	t.Setenv("KEERA_OPERATOR_KEY", "")

	base := "http://127.0.0.1:8080"
	t.Setenv("KEERA_CONTROL_URL", base)
	if err := saveSignIn(base, signIn{
		Token: "keera_cli_old", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("storing the sign-in: %v", err)
	}

	err := newClient().do(context.Background(), "GET", "/v1/me", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "keera login") {
		t.Errorf("error = %v, want one that says to sign in again", err)
	}
}

// An operator key exported in the shell was set on purpose, so it wins over a
// stored sign-in.
func TestTheOperatorKeyWinsOverASignIn(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	base := "http://127.0.0.1:8080"
	t.Setenv("KEERA_CONTROL_URL", base)
	if err := saveSignIn(base, signIn{
		Token: "keera_cli_mine", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("storing the sign-in: %v", err)
	}
	t.Setenv("KEERA_OPERATOR_KEY", "the-deployments-own")

	if got := newClient().bearer(); got != "the-deployments-own" {
		t.Errorf("credential = %q, want the operator key from the environment", got)
	}
}

// Which directory to sign in through is settled before a browser is opened, so
// that a deployment with two of them says so in the terminal rather than
// sending somebody to whichever one was configured first.
func TestTheDirectoryToSignInThroughIsSettledFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
		sso       bool
		asked     string
		want      string
		wantErr   string
	}{
		{name: "one directory needs no choosing", sso: true,
			providers: []string{"sso"}, want: "sso"},
		{name: "several, and none named", sso: true,
			providers: []string{"google", "entra"}, wantErr: "google, entra"},
		{name: "several, one named", sso: true,
			providers: []string{"google", "entra"}, asked: "entra", want: "entra"},
		{name: "a name this gateway does not have", sso: true,
			providers: []string{"google"}, asked: "okta", wantErr: "google"},
		{name: "no identity provider at all", sso: false,
			wantErr: "KEERA_OPERATOR_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/control/auth/config" {
					t.Errorf("asked for %s, want the sign-in configuration", r.URL.Path)
				}
				providers := make([]map[string]string, 0, len(tc.providers))
				for _, name := range tc.providers {
					providers = append(providers, map[string]string{"name": name, "label": name})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"sso": tc.sso, "providers": providers, "operator_key": true,
				})
			}))
			defer srv.Close()

			t.Setenv("KEERA_CONTROL_URL", srv.URL)
			t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
			got, err := chooseProvider(context.Background(), newClient(), tc.asked)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("chooseProvider = %v", err)
			}
			if got != tc.want {
				t.Errorf("provider = %q, want %q", got, tc.want)
			}
		})
	}
}

// The loopback port is not private: anything on the machine can reach it. What
// makes the sign-in this process started recognisable is the state it published,
// so a callback carrying anything else is answered and discarded.
func TestALoopbackCallbackIsOnlyAcceptedWithItsOwnState(t *testing.T) {
	ln := listener(t)
	defer func() { _ = ln.Close() }()

	handed := make(chan string, 1)
	go func() {
		code, err := waitForCallback(context.Background(), ln, "the-state")
		if err != nil {
			handed <- "error: " + err.Error()
			return
		}
		handed <- code
	}()

	base := "http://" + ln.Addr().String() + "/callback"
	// A stray request first, which must not complete the sign-in.
	if _, err := http.Get(base + "?state=somebody-elses&code=stolen"); err != nil {
		t.Fatalf("the stray request did not reach the listener: %v", err)
	}
	if _, err := http.Get(base + "?state=the-state&code=the-code"); err != nil {
		t.Fatalf("the sign-in did not reach the listener: %v", err)
	}

	select {
	case got := <-handed:
		if got != "the-code" {
			t.Errorf("code = %q, want the one that came with this sign-in's state", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sign-in never came back")
	}
}

// The label is only ever read by a person deciding which of their sign-ins is
// which, so what matters is that there is always one.
func TestAMachineAlwaysHasSomethingToCallItself(t *testing.T) {
	if got := machineLabel(); got == "" {
		t.Error("machineLabel = empty, want something a person can recognise")
	}
}

// listener is the loopback port a sign-in comes back to.
func listener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	return ln
}

// A whole sign-in, with a stub for the gateway and an HTTP client for the
// browser. It checks the joins: the browser URL carries the loopback address
// and the challenge, the code is redeemed with this process's verifier, and
// the token lands in the credentials file.
func TestAWholeSignInFromTheTerminal(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	t.Setenv("KEERA_OPERATOR_KEY", "")

	const token = "keera_cli_issued"
	var (
		challenge string
		verifier  string
	)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/auth/config":
			_, _ = w.Write([]byte(`{"sso":true,"providers":[{"name":"sso","label":"single sign-on"}]}`))

		case "/control/auth/login":
			// What the identity provider and the callback do, with the part
			// this test is not about left out.
			q := r.URL.Query()
			challenge = q.Get("cli_challenge")
			back, err := url.Parse(q.Get("cli_redirect"))
			if err != nil {
				t.Errorf("the sign-in named an unusable loopback address: %v", err)
				return
			}
			back.RawQuery = url.Values{
				"code": {"the-code"}, "state": {q.Get("cli_state")},
			}.Encode()
			http.Redirect(w, r, back.String(), http.StatusFound)

		case "/control/auth/cli/token":
			var in struct{ Code, Verifier, Label string }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decoding the exchange: %v", err)
				return
			}
			if in.Code != "the-code" {
				t.Errorf("code = %q, want the one the callback handed over", in.Code)
			}
			if in.Label == "" {
				t.Error("the sign-in did not say which machine it was on")
			}
			verifier = in.Verifier
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": token, "expires_at": time.Now().Add(720 * time.Hour),
			})

		case "/control/v1/me":
			if got := r.Header.Get("Authorization"); got != "Bearer "+token {
				t.Errorf("Authorization = %q, want the token that was just issued", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"email": "alice@example.ch", "role": "admin",
				"org_id": "org_1", "org_name": "Example Bank", "via": "cli",
			})

		default:
			t.Errorf("the sign-in asked for %s, which is not part of it", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gateway.Close()
	t.Setenv("KEERA_CONTROL_URL", gateway.URL)

	// Where the desktop would be. The browser follows the redirect to the
	// loopback listener by itself, which is the whole of what a browser does
	// here.
	openInBrowser = func(target string) {
		go func() {
			res, err := http.Get(target)
			if err == nil {
				_ = res.Body.Close()
			}
		}()
	}
	t.Cleanup(func() { openInBrowser = openBrowser })

	if err := loginCmd(context.Background(), nil); err != nil {
		t.Fatalf("keera login: %v", err)
	}

	// The proof key: what the gateway was shown when the sign-in started has to
	// be the challenge for the verifier that redeemed it.
	if challenge == "" || verifier == "" || authn.CLIChallenge(verifier) != challenge {
		t.Errorf("challenge %q does not belong to verifier %q", challenge, verifier)
	}

	stored := signInFor(gateway.URL)
	if stored.Token != token {
		t.Errorf("stored token = %q, want the one the gateway issued", stored.Token)
	}
	if stored.Email != "alice@example.ch" {
		t.Errorf("stored address = %q, want the one the sign-in resolved to", stored.Email)
	}
	if stored.ExpiresAt.Before(time.Now()) {
		t.Errorf("stored expiry = %v, want one in the future", stored.ExpiresAt)
	}
}
