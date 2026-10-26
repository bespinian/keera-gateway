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

// Only the owner may read the credentials file.
func TestTheCredentialsFileIsReadableOnlyByItsOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KEERA_CONFIG_DIR", filepath.Join(dir, "keera"))

	if err := saveSignIn("http://127.0.0.1:8080", signIn{
		Token: "keera_cli_secret", ExpiresAt: time.Now().Add(time.Hour),
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

// Signing in to or out of one gateway must not touch the others.
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

// Otherwise a trailing slash would ask somebody to sign in again.
func TestOneGatewayWrittenTwoWaysIsOneSignIn(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())

	if err := saveSignIn("https://Keera.example.ch/", signIn{Token: "keera_cli_one"}); err != nil {
		t.Fatalf("storing the sign-in: %v", err)
	}
	if got := signInFor("https://keera.example.ch").Token; got != "keera_cli_one" {
		t.Errorf("token = %q, want the sign-in that was stored under the same gateway", got)
	}
}

// A missing or deleted file just means "not signed in".
func TestAMachineThatNeverSignedInReadsAsEmpty(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", filepath.Join(t.TempDir(), "never-written"))

	if got := signInFor("http://127.0.0.1:8080"); got.Token != "" {
		t.Errorf("token = %q, want none", got.Token)
	}
}

// Refused here rather than at the gateway, so the message says what to do.
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

// An exported operator key was set on purpose, so it wins.
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

// A deployment with two directories says so in the terminal rather than open
// whichever was configured first.
func TestTheDirectoryToSignInThroughIsSettledFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
		sso       bool
		passkeys  bool
		asked     string
		want      string
		wantErr   string
	}{
		{
			name: "one directory needs no choosing", sso: true,
			providers: []string{"sso"}, want: "sso",
		},
		{
			name: "several, and none named", sso: true,
			providers: []string{"google", "entra"}, wantErr: "google, entra",
		},
		{
			name: "several, one named", sso: true,
			providers: []string{"google", "entra"}, asked: "entra", want: "entra",
		},
		{
			name: "a name this gateway does not have", sso: true,
			providers: []string{"google"}, asked: "okta", wantErr: "google",
		},
		{
			name: "no identity provider at all", sso: false,
			wantErr: "KEERA_OPERATOR_KEY",
		},
		{name: "passkeys only", passkeys: true, want: "passkey"},
		{
			name: "a directory and passkeys", sso: true, passkeys: true,
			providers: []string{"google"}, wantErr: "google, passkey",
		},
		{
			name: "passkey named", sso: true, passkeys: true,
			providers: []string{"google"}, asked: "passkey", want: "passkey",
		},
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
					"sso": tc.sso, "providers": providers, "passkeys": tc.passkeys,
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

// Anything on the machine can reach the loopback port, so a callback without
// this process's state is discarded.
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

// The label only helps a person tell sign-ins apart, so there must be one.
func TestAMachineAlwaysHasSomethingToCallItself(t *testing.T) {
	if got := machineLabel(); got == "" {
		t.Error("machineLabel = empty, want something a person can recognise")
	}
}

func listener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	return ln
}

// A whole sign-in, with a stub gateway and an HTTP client as the browser.
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
			// The identity provider and the callback, trimmed down.
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

	// The browser follows the redirect to the loopback listener.
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

	// The challenge shown at the start must match the verifier that redeemed
	// the code.
	if challenge == "" || verifier == "" || authn.CLIChallenge(verifier) != challenge {
		t.Errorf("challenge %q does not belong to verifier %q", challenge, verifier)
	}

	stored := signInFor(gateway.URL)
	if stored.Token != token {
		t.Errorf("stored token = %q, want the one the gateway issued", stored.Token)
	}
	if stored.ExpiresAt.Before(time.Now()) {
		t.Errorf("stored expiry = %v, want one in the future", stored.ExpiresAt)
	}
}

func signInFor(base string) signIn {
	c, err := loadCredentials()
	if err != nil {
		return signIn{}
	}
	return c.Gateways[gatewayKey(base)]
}
