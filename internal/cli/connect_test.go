package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/connect"
)

// catalogueOf is the /v1/connect answer, built from the same package the
// control plane serves it from - so a test asserting on what the CLI printed is
// asserting against the real templates.
func catalogueOf(gateway string) map[string]any {
	return map[string]any{"data": connect.Clients(), "gateway_url": gateway}
}

// oneRouter is the organisation's routers as /v1/routers answers with them. A
// router's alias goes where a model's alias goes, so `keera connect` has to be
// able to write one into an editor's configuration.
var oneRouter = map[string]any{"data": []map[string]any{
	{
		"alias": "auto", "model": "keera-guard",
		"destinations": []string{"keera-code", "keera-frontier"},
		"fallback":     "keera-code",
	},
}}

var chatAlias = map[string]any{"data": []map[string]any{
	{"alias": "keera-code", "kind": "chat", "enabled": true, "backend_model": "qwen"},
	{"alias": "keera-embed", "kind": "embedding", "enabled": true},
	{"alias": "keera-off", "kind": "chat", "enabled": false},
}}

// captured runs a command with stdout collected, which for `keera connect` is the
// configuration block and nothing else.
func captured(t *testing.T, run func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, devNull
	runErr := run()
	_ = w.Close()
	os.Stdout, os.Stderr = stdout, stderr
	_ = devNull.Close()

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = r.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	return b.String()
}

// The block on stdout is the whole point: it is what gets redirected into the
// file the command names, so nothing else may be on that stream.
func TestConnectPutsOnlyTheBlockOnStdout(t *testing.T) {
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api"),
		"GET /v1/models":  chatAlias,
	})

	got := captured(t, func() error {
		return connectCmd(context.Background(), []string{"opencode"})
	})
	client, _ := connect.Lookup("opencode")
	want := client.Render("https://keera.example.ch/api", "keera-code", 0) + "\n"
	if got != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got, want)
	}
}

// The model defaults to a chat one that is actually served: an embedding model
// or a disabled one would configure an editor that is refused on first use.
func TestConnectDefaultsToAnEnabledChatModel(t *testing.T) {
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api"),
		"GET /v1/models": map[string]any{"data": []map[string]any{
			{"alias": "keera-embed", "kind": "embedding", "enabled": true},
			{"alias": "keera-off", "kind": "chat", "enabled": false},
			{"alias": "keera-code", "kind": "chat", "enabled": true},
		}},
	})

	got := captured(t, func() error {
		return connectCmd(context.Background(), []string{"openai"})
	})
	if !strings.Contains(got, `"model":"keera-code"`) {
		t.Errorf("did not pick the enabled chat alias:\n%s", got)
	}
}

func TestConnectRefusesAModelTheDeploymentDoesNotServe(t *testing.T) {
	quiet(t)
	// A name that is not an alias may still be a router, so the routers are
	// read before this is called a name the deployment does not serve.
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api"),
		"GET /v1/models":  chatAlias,
		"GET /v1/orgs":    oneOrg,
		"GET /v1/routers": oneRouter,
	})

	err := connectCmd(context.Background(), []string{"opencode", "--model", "gpt-4"})
	if err == nil {
		t.Fatal("connect configured a model that is not in the catalogue")
	}
	// The list is the useful half: a developer who guessed the name needs the
	// names that exist, not to be told this one does not.
	if !strings.Contains(err.Error(), "keera-code") {
		t.Errorf("the error does not name what is served: %v", err)
	}
	if strings.Contains(err.Error(), "keera-embed") || strings.Contains(err.Error(), "keera-off") {
		t.Errorf("the error offers a model no coding agent can use: %v", err)
	}
}

func TestConnectRefusesAClientItDoesNotKnow(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api"),
		"GET /v1/models":  chatAlias,
	})

	err := connectCmd(context.Background(), []string{"emacs"})
	if err == nil || !strings.Contains(err.Error(), "opencode") {
		t.Errorf("error = %v, want it to list the clients it can configure", err)
	}
}

// A client is pointed at the gateway that answered, and nothing passed to the
// command can change that: the wrong address fails as if the key were bad.
func TestConnectUsesTheGatewayThatAnswered(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api/"),
		"GET /v1/models":  chatAlias,
	})

	got := captured(t, func() error {
		return connectCmd(context.Background(), []string{"openai"})
	})
	if !strings.Contains(got, "export OPENAI_BASE_URL=https://keera.example.ch/api/v1") {
		t.Errorf("the declared gateway was not used, or its trailing slash survived:\n%s", got)
	}
}

// The gateway derives this address from the origin the panel was read on, so
// an empty one means something upstream is misconfigured rather than merely
// undeclared - and guessing would send a developer somewhere nothing answers.
func TestConnectSaysSoWhenNoGatewayAddressIsKnown(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf(""),
		"GET /v1/models":  chatAlias,
	})

	err := connectCmd(context.Background(), []string{"openai"})
	if err == nil || !strings.Contains(err.Error(), "KEERA_PUBLIC_URL") {
		t.Errorf("error = %v, want it to name the setting that fixes this", err)
	}
}

func TestConnectWithNoClientListsThem(t *testing.T) {
	// The listing reads the organisation's routers too: a client names one
	// where it names a model, so this is where a developer discovers it.
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api"),
		"GET /v1/models":  chatAlias,
		"GET /v1/orgs":    oneOrg,
		"GET /v1/routers": oneRouter,
	})

	got := captured(t, func() error {
		return connectCmd(context.Background(), nil)
	})
	for _, want := range []string{"opencode", "claude-code", "openai", "keera-code", "auto"} {
		if !strings.Contains(got, want) {
			t.Errorf("the listing does not mention %s:\n%s", want, got)
		}
	}
	// A listing that offered a disabled alias would send somebody to configure
	// an editor that cannot work.
	if strings.Contains(got, "keera-off") {
		t.Errorf("the listing offers a disabled alias:\n%s", got)
	}
}

func TestConnectRefusesWhenNothingChatIsServed(t *testing.T) {
	quiet(t)
	newFakeControl(t, map[string]any{
		"GET /v1/connect": catalogueOf("https://keera.example.ch/api"),
		"GET /v1/models": map[string]any{"data": []map[string]any{
			{"alias": "keera-embed", "kind": "embedding", "enabled": true},
		}},
	})

	err := connectCmd(context.Background(), []string{"opencode"})
	if err == nil || !strings.Contains(err.Error(), "keera model add") {
		t.Errorf("error = %v, want it to point at adding a model", err)
	}
}

func TestWrapBreaksProseAndIndentsWhatItCarries(t *testing.T) {
	got := wrap("one two three four five", 9, "  ")
	want := "one two\n  three\n  four\n  five"
	if got != want {
		t.Errorf("wrap = %q, want %q", got, want)
	}
}
