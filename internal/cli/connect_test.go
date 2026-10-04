package cli

import (
	"context"
	"maps"
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

// noKeys is /v1/access for somebody with no key of their own, such as the
// operator key the tests sign in with.
var noKeys = map[string]any{"anonymous": true, "keys": []any{}}

// connectRoutes is what `keera connect` reads, with extra replacing any of it.
func connectRoutes(gateway string, extra map[string]any) map[string]any {
	routes := map[string]any{
		"GET /v1/orgs":    oneOrg,
		"GET /v1/connect": catalogueOf(gateway),
		"GET /v1/models":  chatAlias,
		"GET /v1/routers": oneRouter,
		"GET /v1/access":  noKeys,
	}
	maps.Copy(routes, extra)
	return routes
}

// The block on stdout is the whole point: it is what gets redirected into the
// file the command names, so nothing else may be on that stream.
func TestConnectPutsOnlyTheBlockOnStdout(t *testing.T) {
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

	got := captured(t, func() error {
		return connectCmd(context.Background(), []string{"opencode"})
	})
	client, _ := connect.Find(connect.Clients(), "opencode")
	want := client.Render("https://keera.example.ch/api",
		[]connect.Model{{Alias: "keera-code"}, {Alias: "auto"}}) + "\n"
	if got != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got, want)
	}
}

// Only chat models that are actually served go in: an embedding model or a
// disabled one would configure an editor that is refused on first use.
func TestConnectConfiguresOnlyEnabledChatModels(t *testing.T) {
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

	got := captured(t, func() error {
		return connectCmd(context.Background(), []string{"pi"})
	})
	for _, want := range []string{`"keera-code"`, `"auto"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the config does not name %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "keera-embed") || strings.Contains(got, "keera-off") {
		t.Errorf("the config names a model no coding agent can use:\n%s", got)
	}
}

// twoKeys is a developer with two keys: the first may call only keera-code, the
// second only the router. A revoked key and a subscription key are skipped.
var twoKeys = map[string]any{"keys": []map[string]any{
	{"id": "key_old", "org_id": "org_1", "name": "old", "state": "revoked",
		"allowed_models": []string{"keera-code", "auto"}},
	{"id": "key_sub", "org_id": "org_1", "name": "plan", "state": "active",
		"kind": "subscription", "allowed_models": []string{"claude-opus"}},
	{"id": "key_1", "org_id": "org_1", "name": "laptop", "state": "active",
		"prefix": "keera_sk_ab12", "allowed_models": []string{"keera-code"}},
	{"id": "key_2", "org_id": "org_1", "name": "ci", "state": "active",
		"allowed_models": []string{"auto"}},
}}

// The config lists what the key may call, so the editor offers nothing that
// answers 404. Without --key it is the caller's first active key.
func TestConnectConfiguresTheKeysModels(t *testing.T) {
	newFakeControl(t, connectRoutes("https://keera.example.ch/api",
		map[string]any{"GET /v1/access": twoKeys}))

	pi, _ := connect.Find(connect.Clients(), "pi")
	tests := []struct {
		args []string
		want []connect.Model
	}{
		{[]string{"pi"}, []connect.Model{{Alias: "keera-code"}}},
		{[]string{"pi", "--key", "ci"}, []connect.Model{{Alias: "auto"}}},
		{[]string{"pi", "--key", "key_1"}, []connect.Model{{Alias: "keera-code"}}},
	}
	for _, tt := range tests {
		got := captured(t, func() error { return connectCmd(context.Background(), tt.args) })
		if want := pi.Render("https://keera.example.ch/api", tt.want) + "\n"; got != want {
			t.Errorf("%v: stdout =\n%s\nwant\n%s", tt.args, got, want)
		}
	}
}

func TestConnectRefusesAKeyThatIsNotTheCallers(t *testing.T) {
	quiet(t)
	newFakeControl(t, connectRoutes("https://keera.example.ch/api",
		map[string]any{"GET /v1/access": twoKeys}))

	for _, name := range []string{"somebody-else", "old", "plan"} {
		err := connectCmd(context.Background(), []string{"pi", "--key", name})
		if err == nil {
			t.Fatalf("connect configured the key %s", name)
		}
		// The names that would work are the useful half.
		if !strings.Contains(err.Error(), "laptop, ci") {
			t.Errorf("the error does not name the caller's keys: %v", err)
		}
	}
}

func TestConnectRefusesAKeyThatMayCallNoChatModel(t *testing.T) {
	quiet(t)
	newFakeControl(t, connectRoutes("https://keera.example.ch/api",
		map[string]any{"GET /v1/access": map[string]any{"keys": []map[string]any{
			{"id": "key_1", "org_id": "org_1", "name": "embed-only", "state": "active",
				"allowed_models": []string{"keera-embed"}},
		}}}))

	err := connectCmd(context.Background(), []string{"pi"})
	if err == nil || !strings.Contains(err.Error(), "guardrails") {
		t.Errorf("error = %v, want it to say the key's guardrails allow no chat model", err)
	}
}

// A configuration lists every model of its key, so there is no model to pick
// outside --subscription.
func TestConnectRefusesAModelOutsideASubscription(t *testing.T) {
	quiet(t)
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

	err := connectCmd(context.Background(), []string{"opencode", "--model", "keera-code"})
	if err == nil || !strings.Contains(err.Error(), "--key") {
		t.Errorf("error = %v, want it to point at --key", err)
	}
}

func TestConnectRefusesAClientItDoesNotKnow(t *testing.T) {
	quiet(t)
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

	err := connectCmd(context.Background(), []string{"emacs"})
	if err == nil || !strings.Contains(err.Error(), "opencode") {
		t.Errorf("error = %v, want it to list the clients it can configure", err)
	}
}

// A client is pointed at the gateway that answered, and nothing passed to the
// command can change that: the wrong address fails as if the key were bad.
func TestConnectUsesTheGatewayThatAnswered(t *testing.T) {
	quiet(t)
	newFakeControl(t, connectRoutes("https://keera.example.ch/api/", nil))

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
	newFakeControl(t, connectRoutes("", nil))

	err := connectCmd(context.Background(), []string{"openai"})
	if err == nil || !strings.Contains(err.Error(), "KEERA_PUBLIC_URL") {
		t.Errorf("error = %v, want it to name the setting that fixes this", err)
	}
}

func TestConnectWithNoClientListsThem(t *testing.T) {
	// The listing reads the organisation's routers too: a client names one
	// where it names a model, so this is where a developer discovers it.
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

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
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", map[string]any{
		"GET /v1/models": map[string]any{"data": []map[string]any{
			{"alias": "keera-embed", "kind": "embedding", "enabled": true},
		}},
		"GET /v1/routers": map[string]any{"data": []any{}},
	}))

	err := connectCmd(context.Background(), []string{"opencode"})
	if err == nil || !strings.Contains(err.Error(), "keera model add") {
		t.Errorf("error = %v, want it to point at adding a model", err)
	}
}

func TestWrapBreaksProseAndIndentsWhatItCarries(t *testing.T) {
	got := wrapAt("one two three four five", 0, 2, 9)
	want := "one two\n  three\n  four\n  five"
	if got != want {
		t.Errorf("wrapAt = %q, want %q", got, want)
	}
}
