package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestSubscriptionEnvMapsEachFamilyToAModel(t *testing.T) {
	models := []policy.Model{
		{Alias: "keera-code", Kind: policy.KindChat, Enabled: true},
		{Alias: "claude-opus", BackendModel: "claude-opus-5-5", Kind: policy.KindChat,
			Enabled: true, Subscription: true, MaxContext: 1_000_000},
		{Alias: "claude-haiku", BackendModel: "claude-haiku-4-5", Kind: policy.KindChat,
			Enabled: true, Subscription: true, MaxContext: 200_000},
	}
	env, main, err := subscriptionEnv(models, "")
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code asks for a Sonnet too, and the organisation has none, so
	// that goes to the main model rather than to a name nothing answers. A 1M
	// model carries [1m], or Claude Code would assume 200K.
	want := map[string]string{
		"ANTHROPIC_MODEL":                "claude-opus[1m]",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "claude-opus[1m]",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-opus[1m]",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "claude-haiku",
		"ENABLE_TOOL_SEARCH":             "true",
	}
	if main != "claude-opus" {
		t.Errorf("main model = %s, want the first subscription model", main)
	}
	for name, alias := range want {
		if env[name] != alias {
			t.Errorf("%s = %q, want %q", name, env[name], alias)
		}
	}

	if _, _, err := subscriptionEnv(models, "keera-code"); err == nil {
		t.Error("an organisation model was accepted; a subscription key cannot reach it")
	}
	if _, _, err := subscriptionEnv(models[:1], ""); err == nil {
		t.Error("an organisation with no subscription model was accepted")
	}
}

func TestSetClaudeEnvKeepsTheRestOfTheSettings(t *testing.T) {
	settings := map[string]any{
		"theme": "dark",
		"env": map[string]any{
			"ANTHROPIC_CUSTOM_HEADERS": "X-Project: payments\nx-keera-key: keera_sk_old",
			"ANTHROPIC_AUTH_TOKEN":     "keera_sk_old",
		},
	}
	warnings := setClaudeEnv(settings, map[string]string{"ANTHROPIC_BASE_URL": "https://gw/api"},
		"keera_sk_new")

	env := settings["env"].(map[string]any)
	if settings["theme"] != "dark" || env["ANTHROPIC_BASE_URL"] != "https://gw/api" {
		t.Errorf("settings = %+v, want the theme kept and the address set", settings)
	}
	if got := env["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Project: payments\nX-Keera-Key: keera_sk_new" {
		t.Errorf("headers = %q, want the other header kept and the key replaced", got)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("warnings = %q, want one about ANTHROPIC_AUTH_TOKEN", warnings)
	}
}

func TestAnEmptyValueTakesTheSettingOut(t *testing.T) {
	// An address in the user's settings would stop Claude Code fetching the
	// organisation's, so one from an earlier run goes when they set it.
	settings := map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "https://gw/api"}}
	setClaudeEnv(settings, map[string]string{"ANTHROPIC_BASE_URL": ""}, "keera_sk_new")
	if _, set := settings["env"].(map[string]any)["ANTHROPIC_BASE_URL"]; set {
		t.Errorf("env = %+v, want the address taken out", settings["env"])
	}
}

func TestManagedBaseURLReadsEveryManagedSource(t *testing.T) {
	dir := t.TempDir()
	write := func(path, body string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	env := func(url string) string { return `{"env":{"ANTHROPIC_BASE_URL":"` + url + `"}}` }
	server := filepath.Join(dir, "remote-settings.json")
	system := filepath.Join(dir, "system")

	if got := managedBaseURL(server, system); got != "" {
		t.Errorf("no managed settings gave %q, want none", got)
	}
	write(filepath.Join(system, "managed-settings.json"), env("https://file/api"))
	write(filepath.Join(system, "managed-settings.d", "10-gateway.json"), env("https://drop-in/api"))
	write(filepath.Join(system, "managed-settings.d", ".hidden.json"), env("https://hidden/api"))
	if got := managedBaseURL(server, system); got != "https://drop-in/api" {
		t.Errorf("files gave %q, want the drop-in, which comes after the file", got)
	}
	write(server, env("https://server/api"))
	if got := managedBaseURL(server, system); got != "https://server/api" {
		t.Errorf("got %q, want the admin console's, which wins", got)
	}
	write(server, `{"defaultModel":"opus"}`)
	if got := managedBaseURL(server, system); got != "https://drop-in/api" {
		t.Errorf("got %q, want the files' when the admin console sets no address", got)
	}
}

func TestModelOverridesTellClaudeCodeWhichModelEachAliasIs(t *testing.T) {
	models := []policy.Model{
		{Alias: "keera-code", BackendModel: "qwen", Kind: policy.KindChat, Enabled: true},
		{Alias: "claude-haiku", BackendModel: "claude-haiku-4-5", Kind: policy.KindChat,
			Enabled: true, Subscription: true},
		{Alias: "claude-opus", BackendModel: "claude-opus-5-5", Kind: policy.KindChat,
			Enabled: true, Subscription: true},
		{Alias: "claude-opus-too", BackendModel: "claude-opus-5-5", Kind: policy.KindChat,
			Enabled: true, Subscription: true},
	}
	// Claude Code knows Haiku 4.5 by its dated id only, so both are written.
	want := map[string]string{
		"claude-haiku-4-5":          "claude-haiku",
		"claude-haiku-4-5-20251001": "claude-haiku",
		"claude-opus-5-5":           "claude-opus",
	}
	if got := claudeModelOverrides(models); !maps.Equal(got, want) {
		t.Errorf("overrides = %v, want %v", got, want)
	}

	settings := map[string]any{"modelOverrides": map[string]any{"claude-sonnet-5": "mine"}}
	setModelOverrides(settings, want)
	block := settings["modelOverrides"].(map[string]any)
	if block["claude-sonnet-5"] != "mine" || block["claude-opus-5-5"] != "claude-opus" {
		t.Errorf("modelOverrides = %v, want the person's own entry kept", block)
	}
}

func TestSettingsAreWrittenForTheirOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude", "settings.json")
	settings, err := readSettings(path)
	if err != nil || len(settings) != 0 {
		t.Fatalf("a missing file read as %v, %v; want empty settings", settings, err)
	}
	setClaudeEnv(settings, map[string]string{}, "keera_sk_new")
	if err := writeSettings(path, settings); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600: the file holds a key", info.Mode().Perm())
	}
	back, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := back["env"].(map[string]any); env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Keera-Key: keera_sk_new" {
		t.Errorf("read back %+v", back)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSettings(path); err == nil {
		t.Error("a broken file was read; it would have been overwritten")
	}
}

func TestTheMachineIDStaysTheSame(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	first, err := machineID()
	if err != nil || len(first) != 8 {
		t.Fatalf("machineID = %q, %v; want 8 hex digits", first, err)
	}
	if again, _ := machineID(); again != first {
		t.Errorf("the id changed from %s to %s; the machine's key would not be found again", first, again)
	}
}

// memberKeys is /v1/keys for a member with a key on another machine and a
// subscription key an administrator issued them that no machine holds yet.
var memberKeys = map[string]any{"data": []map[string]any{
	{"id": "key_desktop", "org_id": "org_1", "user_id": "user_1", "kind": "subscription",
		"name": "claude-code on desktop (0a1b2c3d)"},
	{"id": "key_issued", "org_id": "org_1", "user_id": "user_1", "kind": "subscription",
		"name": "ada's Claude Code"},
	{"id": "key_bob", "org_id": "org_1", "user_id": "user_2", "kind": "subscription",
		"name": "bob's Claude Code"},
}}

var member = identity{UserID: "user_1", Email: "ada@example.ch", Role: "member", OrgID: "org_1"}

// Only an administrator issues keys, so a member's first run takes over the
// key one issued them. The key another machine holds is left alone, or that
// machine would stop working.
func TestMachineKeyTakesOverAMembersIssuedKey(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys": memberKeys,
		"POST /v1/keys/key_issued/rotate": map[string]any{
			"id": "key_new", "key": "keera_sk_new", "name": "claude-code on laptop (ffffffff)",
		},
	})

	key, replaced, err := machineKey(context.Background(), newClient(), "org_1", member, "")
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != "key_new" || replaced == nil || replaced.ID != "key_issued" {
		t.Errorf("got %s replacing %+v, want key_new replacing key_issued", key.ID, replaced)
	}
	name, _ := f.request("POST", "/v1/keys/key_issued/rotate").body["name"].(string)
	if !isMachineKeyName(name) {
		t.Errorf("rotated to the name %q, want this machine's", name)
	}
	if f.called("POST", "/v1/keys") {
		t.Error("a member's run tried to issue a key")
	}
}

func TestMachineKeySendsAMemberWithoutAKeyToAnAdministrator(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys": map[string]any{"data": memberKeys["data"].([]map[string]any)[:1]},
	})

	_, _, err := machineKey(context.Background(), newClient(), "org_1", member, "")
	if err == nil || !strings.Contains(err.Error(), "administrator") ||
		!strings.Contains(err.Error(), "--user ada@example.ch") {
		t.Errorf("err = %v, want it to say what to ask an administrator for", err)
	}
	if f.called("POST", "/v1/keys") || f.called("POST", "/v1/keys/key_desktop/rotate") {
		t.Errorf("the CLI changed a key anyway: %v", f.seen)
	}
}

// A key the CLI issues itself lasts as long as one from 'keera key create', so
// it does not live for ever because nobody chose.
func TestMachineKeyIssuedByAnAdministratorExpires(t *testing.T) {
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	f := newFakeControl(t, map[string]any{
		"GET /v1/keys":  map[string]any{"data": []map[string]any{}},
		"POST /v1/keys": map[string]any{"id": "key_1", "key": "keera_sk_new"},
	})
	admin := identity{UserID: "user_1", Email: "ada@example.ch", Role: "admin", OrgID: "org_1"}

	if _, _, err := machineKey(context.Background(), newClient(), "org_1", admin, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.request("POST", "/v1/keys").body["expires_in"]; got != defaultKeyLife {
		t.Errorf("expires_in = %v, want %s", got, defaultKeyLife)
	}
}

func TestIsMachineKeyName(t *testing.T) {
	for name, want := range map[string]bool{
		machineKeyName("laptop", "0a1b2c3d"):          true,
		machineKeyName("my (old) laptop", "0a1b2c3d"): true,
		"ada's Claude Code":                           false,
		"claude-code on laptop":                       false,
		"claude-code on laptop (not-hex!)":            false,
	} {
		if got := isMachineKeyName(name); got != want {
			t.Errorf("isMachineKeyName(%q) = %v, want %v", name, got, want)
		}
	}
}
