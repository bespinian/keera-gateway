package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeCodeRoutes is a member with a key an administrator left for them to
// activate, and the routes `keera connect claude-code` reads.
func claudeCodeRoutes(extra map[string]any) map[string]any {
	routes := connectRoutes("https://keera.example.ch/api", map[string]any{
		"GET /v1/me": map[string]any{"user_id": "user_1", "email": "ada@example.ch",
			"role": "member", "org_id": "org_1"},
		"GET /v1/keys": map[string]any{"data": []map[string]any{
			{"id": "key_pending", "org_id": "org_1", "user_id": "user_1", "kind": "standard",
				"name": "Ada's laptop", "pending": true},
		}},
		"POST /v1/keys/key_pending/activate": map[string]any{
			"id": "key_pending", "org_id": "org_1", "user_id": "user_1", "kind": "standard",
			"name": "claude-code on laptop (0a1b2c3d)", "prefix": "keera_sk_ne",
			"key": "keera_sk_new",
		},
		"GET /v1/access": map[string]any{"keys": []map[string]any{
			{"id": "key_pending", "org_id": "org_1", "name": "claude-code on laptop (0a1b2c3d)",
				"kind": "standard", "state": "active", "allowed_models": []string{"keera-code"}},
		}},
	})
	maps.Copy(routes, extra)
	return routes
}

// The whole developer set-up is one command: the key left for them is
// activated for this machine and written where Claude Code reads it.
func TestConnectClaudeCodeActivatesAPendingKeyAndWritesTheSettings(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	// What --subscription wrote earlier must not win over the new key.
	if err := os.WriteFile(path, []byte(`{"theme":"dark","env":{
		"ANTHROPIC_CUSTOM_HEADERS":"X-Keera-Key: keera_sk_old","ENABLE_TOOL_SEARCH":"true"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeControl(t, claudeCodeRoutes(nil))

	if err := connectCmd(context.Background(), []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	name, _ := f.request("POST", "/v1/keys/key_pending/activate").body["name"].(string)
	if !isMachineKeyName(name) {
		t.Errorf("activated with the name %q, want this machine's", name)
	}
	if f.called("POST", "/v1/keys") {
		t.Error("a member's run tried to issue a key")
	}
	settings, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := settings["env"].(map[string]any)
	want := map[string]any{
		"ANTHROPIC_BASE_URL":                       "https://keera.example.ch/api",
		"ANTHROPIC_AUTH_TOKEN":                     "keera_sk_new",
		"ANTHROPIC_MODEL":                          "keera-code",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %v, want %v", k, env[k], v)
		}
	}
	for _, gone := range []string{"ANTHROPIC_CUSTOM_HEADERS", "ENABLE_TOOL_SEARCH"} {
		if _, set := env[gone]; set {
			t.Errorf("%s is still set: %v", gone, env[gone])
		}
	}
	if settings["theme"] != "dark" {
		t.Errorf("the rest of the settings were lost: %v", settings)
	}
}

func TestConnectClaudeCodeSendsAMemberWithoutAKeyToAnAdministrator(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, claudeCodeRoutes(map[string]any{
		"GET /v1/keys": map[string]any{"data": []any{}},
	}))

	err := connectCmd(context.Background(), []string{"claude-code"})
	if err == nil || !strings.Contains(err.Error(), "--pending --user ada@example.ch") {
		t.Errorf("err = %v, want it to say what to ask an administrator for", err)
	}
	if f.called("POST", "/v1/keys") {
		t.Error("a member's run tried to issue a key")
	}
}

// --print is the old behaviour: the settings, and no key issued.
func TestConnectClaudeCodePrintIssuesNoKey(t *testing.T) {
	f := newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

	got := captured(t, func() error {
		return connectCmd(context.Background(), []string{"claude-code", "--print"})
	})
	if !strings.Contains(got, `"ANTHROPIC_BASE_URL": "https://keera.example.ch/api"`) {
		t.Errorf("stdout =\n%s\nwant the settings", got)
	}
	if f.called("POST", "/v1/keys") {
		t.Error("--print issued a key")
	}
}

// The operator key is nobody, so it cannot hold this machine's key.
func TestConnectClaudeCodeRefusesTheOperatorKey(t *testing.T) {
	quiet(t)
	newFakeControl(t, connectRoutes("https://keera.example.ch/api", nil))

	err := connectCmd(context.Background(), []string{"claude-code"})
	if err == nil || !strings.Contains(err.Error(), "--print") {
		t.Errorf("err = %v, want it to point at --print", err)
	}
}

// thisMachinesKey is a key `connect claude-code` issued this machine before.
func thisMachinesKey(t *testing.T, kind string) map[string]any {
	t.Helper()
	machine, err := machineID()
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "this machine"
	}
	return map[string]any{"id": "key_mine", "org_id": "org_1", "user_id": "user_1",
		"kind": kind, "name": machineKeyName(host, machine)}
}

// Replacing the machine's key revokes the one Claude Code holds, so a model the
// key may not use is caught before that, not after.
func TestConnectClaudeCodeKeepsTheKeyWhenItMayNotUseTheModel(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, claudeCodeRoutes(map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{thisMachinesKey(t, "standard")}},
		"GET /v1/access": map[string]any{"keys": []map[string]any{
			{"id": "key_mine", "org_id": "org_1", "kind": "standard", "state": "active",
				"allowed_models": []string{"keera-code"}},
		}},
	}))

	err := connectCmd(context.Background(), []string{"claude-code", "--model", "auto"})
	if err == nil || !strings.Contains(err.Error(), "may not use auto") {
		t.Errorf("err = %v, want it to say the key may not use the model", err)
	}
	if f.called("POST", "/v1/keys/key_mine/rotate") {
		t.Error("the key was replaced although nothing was written")
	}
}

// An expired key is when people run this again, so it is replaced, not
// refused for not being active.
func TestConnectClaudeCodeReplacesAnExpiredKey(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, claudeCodeRoutes(map[string]any{
		"GET /v1/keys": map[string]any{"data": []map[string]any{thisMachinesKey(t, "standard")}},
		"GET /v1/access": map[string]any{"keys": []map[string]any{
			{"id": "key_mine", "org_id": "org_1", "kind": "standard", "state": "expired",
				"allowed_models": []string{"keera-code"}},
		}},
		"POST /v1/keys/key_mine/rotate": map[string]any{
			"id": "key_new", "key": "keera_sk_new", "kind": "standard",
		},
	}))

	if err := connectCmd(context.Background(), []string{"claude-code"}); err != nil {
		t.Fatal(err)
	}
	if !f.called("POST", "/v1/keys/key_mine/rotate") {
		t.Error("the expired key was not replaced")
	}
}

// The Keera key a run without --subscription wrote would be sent instead of
// the Claude sign-in, so --subscription takes it out.
func TestConnectClaudeCodeSubscriptionRemovesTheKeeraKey(t *testing.T) {
	quiet(t)
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"keera_sk_old"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	newFakeControl(t, claudeCodeRoutes(map[string]any{
		"GET /v1/me": map[string]any{"user_id": "user_1", "email": "ada@example.ch",
			"role": "member", "org_id": "org_1", "claude_subscriptions": true},
		"GET /v1/models": map[string]any{"data": []map[string]any{
			{"alias": "claude-opus", "kind": "chat", "enabled": true, "provider": "anthropic",
				"backend_model": "claude-opus-5-5", "subscription": true},
		}},
		"GET /v1/keys": map[string]any{"data": []map[string]any{thisMachinesKey(t, "subscription")}},
		"POST /v1/keys/key_mine/rotate": map[string]any{
			"id": "key_new", "key": "keera_sk_new", "kind": "subscription",
		},
	}))

	if err := connectCmd(context.Background(), []string{"claude-code", "--subscription"}); err != nil {
		t.Fatal(err)
	}
	settings, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := settings["env"].(map[string]any)
	if _, set := env["ANTHROPIC_AUTH_TOKEN"]; set {
		t.Errorf("ANTHROPIC_AUTH_TOKEN is still set: %v", env)
	}
	if h, _ := env["ANTHROPIC_CUSTOM_HEADERS"].(string); !strings.Contains(h, "keera_sk_new") {
		t.Errorf("ANTHROPIC_CUSTOM_HEADERS = %q, want the subscription key", h)
	}
}
