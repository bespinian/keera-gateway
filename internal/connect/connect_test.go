package connect

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestTitleIsTheLabelAModelPickerShows(t *testing.T) {
	tests := []struct {
		alias string
		want  string
	}{
		{"keera-speed", "Keera Speed"},
		{"keera_code", "Keera Code"},
		{"keera.frontier", "Keera Frontier"},
		{"single", "Single"},
		{"a.b-c_d", "A B C D"},
	}
	for _, tt := range tests {
		if got := Title(tt.alias); got != tt.want {
			t.Errorf("Title(%q) = %q, want %q", tt.alias, got, tt.want)
		}
	}
}

// one is the single model most tests render with.
var one = []Model{{Alias: "keera-code"}}

// A person correcting the base URL in the panel leaves a trailing slash
// behind, which would otherwise reach a config as "…//v1".
func TestRenderTrimsATrailingSlashFromTheBase(t *testing.T) {
	c, _ := Find(Clients(), "curl")
	got := c.Render("https://keera.example.ch///", one)
	if !strings.Contains(got, "curl https://keera.example.ch/v1/chat/completions") {
		t.Errorf("base URL not normalised:\n%s", got)
	}
}

// two is a key that may call a model and a router.
var two = []Model{{Alias: "keera-code", MaxContext: 32_768}, {Alias: "auto"}}

// A placeholder that survives Render is a config the developer has to edit,
// which is what this package exists to prevent.
func TestNoTemplateLeavesAPlaceholderBehind(t *testing.T) {
	for _, c := range Clients() {
		for _, text := range map[string]string{
			"config": c.Render("https://keera.example.ch", two),
			"run":    c.RunText(two),
			"note":   c.NoteText(),
		} {
			if strings.Contains(text, "{{") {
				t.Errorf("%s left a placeholder in:\n%s", c.Key, text)
			}
		}
	}
}

// Each entry must carry enough to be rendered by a caller that does not know
// which client it is, as both the panel and the CLI walk this list.
func TestEveryClientIsCompleteEnoughToRender(t *testing.T) {
	if len(Clients()) == 0 {
		t.Fatal("the catalogue is empty")
	}
	seen := map[string]bool{}
	for _, c := range Clients() {
		if seen[c.Key] {
			t.Errorf("two clients share the key %q", c.Key)
		}
		seen[c.Key] = true
		if c.Key == "" || c.Label == "" || c.About == "" || c.Template == "" || c.Run == "" {
			t.Errorf("%q is missing a key, label, about, template or run", c.Key)
		}
		if !slices.Contains([]string{"json", "toml", "sh", "python"}, c.Lang) {
			t.Errorf("%s has lang %q, which no renderer highlights", c.Key, c.Lang)
		}
		// Every block has to name the gateway and the key's models.
		if !strings.Contains(c.Template, "{{base}}") {
			t.Errorf("%s's template never uses {{base}}", c.Key)
		}
		lists := strings.Contains(c.Template, "{{models}}") && c.Entry != "" ||
			strings.Contains(c.Template, "{{claude_env}}")
		if !lists && !strings.Contains(c.Template, "{{aliases}}") {
			t.Errorf("%s's template names neither {{models}} nor {{aliases}}", c.Key)
		}
	}
	// The cURL entry is the one that answers for a client nobody wrote an
	// entry for, so its absence is a regression rather than a choice.
	if _, found := Find(Clients(), "curl"); !found {
		t.Error("the catalogue has no cURL entry")
	}
}

// A JSON template has to still be JSON once it is filled in. A comma dropped
// while editing one of these is not otherwise caught by anything.
func TestJSONTemplatesRenderToValidJSON(t *testing.T) {
	for _, c := range Clients() {
		if c.Lang != "json" {
			continue
		}
		rendered := c.Render("https://keera.example.ch", two)
		var doc map[string]any
		if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
			t.Errorf("%s does not render valid JSON: %v\n%s", c.Key, err, rendered)
		}
	}
}

// The key is read from the environment rather than baked in, except where the
// file cannot read it. Such a block gets the key it is rendered with, or the
// placeholder.
func TestConfigsReadTheKeyFromTheEnvironment(t *testing.T) {
	for _, c := range Clients() {
		rendered := c.Render("https://keera.example.ch", one)
		if c.HoldsKey() {
			if !strings.Contains(rendered, KeyPlaceholder) ||
				!strings.Contains(c.RenderKey("https://keera.example.ch", "keera_sk_ab…", one),
					`"keera_sk_ab…"`) {
				t.Errorf("%s holds the key but does not show it:\n%s", c.Key, rendered)
			}
			continue
		}
		if !strings.Contains(rendered, "KEERA_API_KEY") {
			t.Errorf("%s's config never mentions KEERA_API_KEY:\n%s", c.Key, rendered)
		}
	}
}

// TOML has no parser in the standard library, so this checks what a hand-edited
// template gets wrong: a line that is neither a section nor a setting, and, for
// Codex, a top-level setting below a section, which TOML reads as part of it.
func TestTOMLTemplatesRenderToTOML(t *testing.T) {
	for _, c := range Clients() {
		if c.Lang != "toml" {
			continue
		}
		for line := range strings.Lines(c.Render("https://keera.example.ch", two)) {
			line = strings.TrimSpace(line)
			switch {
			case line == "" || strings.HasPrefix(line, "#"):
			case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			case strings.Contains(line, " = "):
			default:
				t.Errorf("%s renders a line that is not TOML: %q", c.Key, line)
			}
		}
	}
	codex, _ := Find(Clients(), "codex")
	rendered := codex.Render("https://keera.example.ch", two)
	top, _, _ := strings.Cut(rendered, "\n[")
	for _, want := range []string{`model = "keera-code"`, `model_provider = "keera"`,
		"model_context_window = 32768"} {
		if !strings.Contains(top, want) {
			t.Errorf("codex does not set %s above its first section:\n%s", want, rendered)
		}
	}
	if !strings.Contains(rendered, `base_url = "https://keera.example.ch/v1"`) ||
		!strings.Contains(rendered, `wire_api = "responses"`) {
		t.Errorf("codex is not pointed at the gateway's Responses API:\n%s", rendered)
	}
}

// A client configured by a file says which file, and one run from the shell
// says nothing rather than guessing at a shell profile.
func TestAFileClientNamesItsFile(t *testing.T) {
	for _, key := range []string{"opencode", "pi", "codex", "claude-code"} {
		c, found := Find(Clients(), key)
		if !found {
			t.Fatalf("no client %s", key)
		}
		if c.Path == "" {
			t.Errorf("%s is configured by a file but names none", key)
		}
	}
	if c, _ := Find(Clients(), "curl"); c.Path != "" {
		t.Errorf("curl names the file %q, but it is run from the shell", c.Path)
	}
}

// Claude Code is the one client whose base URL is the gateway's root rather
// than its /v1: it appends the path itself, and a base carrying /v1 sends it to
// /v1/v1/messages.
func TestClaudeCodeTakesTheGatewayRoot(t *testing.T) {
	c, _ := Find(Clients(), "claude-code")
	rendered := c.Render("https://keera.example.ch", one)
	if !strings.Contains(rendered, `"ANTHROPIC_BASE_URL": "https://keera.example.ch",`) {
		t.Errorf("the base URL is not the gateway root:\n%s", rendered)
	}
}

// Clients used side by side must describe the same model. They once did not:
// one pinned a temperature and another an output cap, so an alias behaved
// differently per editor.
func TestFileClientsStateTheSameWindow(t *testing.T) {
	const maxContext = 200_000
	window, output := Limits(maxContext)
	models := []Model{{Alias: "keera-code", MaxContext: maxContext}}

	opencode, _ := Find(Clients(), "opencode")
	var oc struct {
		Provider struct {
			Keera struct {
				Models map[string]struct {
					Limit struct {
						Context int `json:"context"`
						Output  int `json:"output"`
					} `json:"limit"`
				} `json:"models"`
			} `json:"keera"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(opencode.Render("https://keera.example.ch", models)), &oc); err != nil {
		t.Fatalf("opencode: %v", err)
	}
	model, found := oc.Provider.Keera.Models["keera-code"]
	if !found {
		t.Fatal("opencode's block does not declare the model")
	}
	if model.Limit.Context != window {
		t.Errorf("opencode declares context %d, want %d", model.Limit.Context, window)
	}
	// OpenCode is the one client that is given an answer length, because its
	// schema takes context and output together or neither.
	if model.Limit.Output != output {
		t.Errorf("opencode declares output %d, want %d", model.Limit.Output, output)
	}

	pi, _ := Find(Clients(), "pi")
	var p struct {
		Providers struct {
			Keera struct {
				APIKey string           `json:"apiKey"`
				Models []map[string]any `json:"models"`
			} `json:"keera"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(pi.Render("https://keera.example.ch", models)), &p); err != nil {
		t.Fatalf("pi: %v", err)
	}
	if len(p.Providers.Keera.Models) != 1 {
		t.Fatalf("pi declares %d models, want 1", len(p.Providers.Keera.Models))
	}
	entry := p.Providers.Keera.Models[0]
	if got, _ := entry["contextWindow"].(float64); int(got) != window {
		t.Errorf("pi declares context %v, want %d", entry["contextWindow"], window)
	}
	// The sandbox image writes this template to ~/.pi/agent/models.json and Pi
	// expands the variable itself. That keeps a sandbox's credential in its
	// environment, not in a file on a volume that outlives a suspend.
	if got := p.Providers.Keera.APIKey; got != "${KEERA_API_KEY}" {
		t.Errorf("pi apiKey = %q; the sandbox image relies on Pi expanding this "+
			"from the environment", got)
	}

	// Pi's schema needs no answer length, and the catalogue records none, so
	// none is invented.
	if _, stated := entry["maxTokens"]; stated {
		t.Errorf("pi states an answer length the catalogue does not know: %v", entry["maxTokens"])
	}
}

// A key's every model is in the config, the first as the default, so the
// developer can switch between them without editing it.
func TestEveryModelOfTheKeyIsConfigured(t *testing.T) {
	for _, c := range Clients() {
		rendered := c.Render("https://keera.example.ch", two)
		if c.Key == "claude-code" {
			// Claude Code takes one model and switches with /model, so the
			// rest are named in what to run.
			rendered += c.RunText(two)
		}
		for _, want := range []string{"keera-code", "auto"} {
			if !strings.Contains(rendered, want) {
				t.Errorf("%s's config does not name %s:\n%s", c.Key, want, rendered)
			}
		}
	}

	pi, _ := Find(Clients(), "pi")
	var p struct {
		Providers struct {
			Keera struct {
				Models []struct {
					ID            string `json:"id"`
					ContextWindow int    `json:"contextWindow"`
				} `json:"models"`
			} `json:"keera"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(pi.Render("https://keera.example.ch", two)), &p); err != nil {
		t.Fatalf("pi: %v", err)
	}
	got := p.Providers.Keera.Models
	if len(got) != 2 || got[0].ID != "keera-code" || got[1].ID != "auto" {
		t.Fatalf("pi models = %+v, want keera-code then auto", got)
	}
	// Each model gets its own window: a router has none, so it gets the default.
	if got[0].ContextWindow != 32_768 || got[1].ContextWindow != DefaultContext {
		t.Errorf("pi windows = %d, %d", got[0].ContextWindow, got[1].ContextWindow)
	}

	cc, _ := Find(Clients(), "claude-code")
	if !strings.Contains(cc.Render("https://keera.example.ch", two), `"ANTHROPIC_MODEL": "keera-code",`) {
		t.Error("claude code does not start with the first model")
	}
}

// Claude Code's block is the settings `keera connect claude-code` writes:
// every family Claude Code asks for by Anthropic's name goes to a model the
// key may use, a 1M model says so, and each Anthropic model is told apart by
// every id it goes by.
func TestClaudeCodeGetsTheSettingsConnectWrites(t *testing.T) {
	models := []Model{
		{Alias: "claude-opus", BackendModel: "claude-opus-5-5", Provider: "anthropic",
			MaxContext: 1_000_000},
		{Alias: "keera-code", BackendModel: "qwen", MaxContext: 256_000},
		{Alias: "claude-haiku", BackendModel: "claude-haiku-4-5", Provider: "anthropic",
			MaxContext: 200_000},
	}
	cc, _ := Find(Clients(), "claude-code")
	var got struct {
		Env            map[string]string `json:"env"`
		ModelOverrides map[string]string `json:"modelOverrides"`
	}
	rendered := cc.Render("https://keera.example.ch", models)
	if err := json.Unmarshal([]byte(rendered), &got); err != nil {
		t.Fatalf("%v\n%s", err, rendered)
	}
	for name, want := range map[string]string{
		"ANTHROPIC_MODEL":                "claude-opus[1m]",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "claude-opus[1m]",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-opus[1m]",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "claude-haiku",
		"ANTHROPIC_DEFAULT_FABLE_MODEL":  "claude-opus[1m]",
		"ANTHROPIC_AUTH_TOKEN":           KeyPlaceholder,
	} {
		if got.Env[name] != want {
			t.Errorf("%s = %q, want %q", name, got.Env[name], want)
		}
	}
	// Claude Code knows Haiku 4.5 by its dated id only, so both are written.
	want := map[string]string{
		"claude-opus-5-5":           "claude-opus",
		"claude-haiku-4-5":          "claude-haiku",
		"claude-haiku-4-5-20251001": "claude-haiku",
	}
	if !maps.Equal(got.ModelOverrides, want) {
		t.Errorf("modelOverrides = %v, want %v", got.ModelOverrides, want)
	}
}

// Only the client whose schema demands one is given an answer length. The
// others state the window and leave the length to their own defaults.
func TestOnlyOpenCodeStatesAnAnswerLength(t *testing.T) {
	for _, c := range Clients() {
		rendered := c.Render("https://keera.example.ch", []Model{{Alias: "keera-code", MaxContext: 200_000}})
		stated := strings.Contains(rendered, "maxTokens") ||
			strings.Contains(rendered, "MAX_OUTPUT_TOKENS") ||
			strings.Contains(rendered, "max_output_tokens") ||
			strings.Contains(rendered, `"output"`)
		if want := c.Key == "opencode"; stated != want {
			t.Errorf("%s states an answer length: %v, want %v\n%s", c.Key, stated, want, rendered)
		}
	}
}

// The window a config states is the one the deployment gives, because the
// gateway refuses a prompt that exceeds it - and an answer is a fraction of
// that, because the rest of the window is the conversation so far.
func TestLimitsFollowTheModelsWindow(t *testing.T) {
	tests := []struct {
		maxContext int
		window     int
		output     int
	}{
		{0, DefaultContext, MaxOutput}, // a catalogue entry that states none
		{16_384, 16_384, 4_096},        // the window a small local model has
		{200_000, 200_000, MaxOutput},  // capped rather than a quarter of 200k
		{1_000, 1_000, MinOutput},      // floored: 250 tokens is not an edit
	}
	for _, tt := range tests {
		window, output := Limits(tt.maxContext)
		if window != tt.window || output != tt.output {
			t.Errorf("Limits(%d) = %d, %d, want %d, %d",
				tt.maxContext, window, output, tt.window, tt.output)
		}
	}
}

// No entry pins a temperature: a reasoning model refuses a request that sets
// one.
func TestNoClientPinsATemperature(t *testing.T) {
	for _, c := range Clients() {
		if strings.Contains(c.Render("https://keera.example.ch", one), "temperature") {
			t.Errorf("%s's config sets a temperature", c.Key)
		}
	}
}

// A model's display name is what a picker shows, even one with a quote in it.
func TestADisplayNameIsTheModelsName(t *testing.T) {
	named := []Model{{Alias: "fast", Name: `Fast "and" cheap`}}
	for _, c := range Clients() {
		if c.Lang != "json" || !strings.Contains(c.Entry, "{{name}}") {
			continue
		}
		rendered := c.Render("https://keera.example.ch", named)
		if !strings.Contains(rendered, `Fast \"and\" cheap`) {
			t.Errorf("%s does not name the model by its display name:\n%s", c.Key, rendered)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
			t.Errorf("%s does not render valid JSON: %v\n%s", c.Key, err, rendered)
		}
	}
}
