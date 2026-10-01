package cli

import (
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
			Enabled: true, Subscription: true},
		{Alias: "claude-haiku", BackendModel: "claude-haiku-4-5", Kind: policy.KindChat,
			Enabled: true, Subscription: true},
	}
	env, main, err := subscriptionEnv(models, "")
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code asks for a Sonnet too, and the organisation has none, so
	// that goes to the main model rather than to a name nothing answers.
	want := map[string]string{
		"ANTHROPIC_MODEL":                "claude-opus",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "claude-opus",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-opus",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "claude-haiku",
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
