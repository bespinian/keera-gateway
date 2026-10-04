package cli

import (
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
			"ANTHROPIC_CUSTOM_HEADERS": "X-Team: payments\nx-keera-key: keera_sk_old",
			"ANTHROPIC_AUTH_TOKEN":     "keera_sk_old",
		},
	}
	warnings := setClaudeEnv(settings, map[string]string{"ANTHROPIC_BASE_URL": "https://gw/api"},
		"keera_sk_new")

	env := settings["env"].(map[string]any)
	if settings["theme"] != "dark" || env["ANTHROPIC_BASE_URL"] != "https://gw/api" {
		t.Errorf("settings = %+v, want the theme kept and the address set", settings)
	}
	if got := env["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Team: payments\nX-Keera-Key: keera_sk_new" {
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
