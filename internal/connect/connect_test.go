package connect

import (
	"encoding/json"
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

// A trailing slash on the base URL is what a person correcting the field in the
// panel leaves behind, and it would otherwise reach a config as "…//v1".
func TestRenderTrimsATrailingSlashFromTheBase(t *testing.T) {
	c, _ := Lookup("openai")
	got := c.Render("https://keera.example.ch///", "keera-code", 0)
	if !strings.Contains(got, "export OPENAI_BASE_URL=https://keera.example.ch/v1") {
		t.Errorf("base URL not normalised:\n%s", got)
	}
}

// Every template has to be fully substituted by Render: a placeholder that
// survives is a configuration a developer copies and has to edit, which is the
// one thing this package exists to prevent.
func TestNoTemplateLeavesAPlaceholderBehind(t *testing.T) {
	for _, c := range Clients() {
		for _, text := range map[string]string{
			"config": c.Render("https://keera.example.ch", "keera-code", 0),
			"run":    c.RunText("keera-code"),
			"note":   c.Note,
		} {
			if strings.Contains(text, "{{") {
				t.Errorf("%s left a placeholder in:\n%s", c.Key, text)
			}
		}
	}
}

// Each entry has to carry enough to be rendered by a caller that knows nothing
// about which client it is looking at - which is how both the panel and the CLI
// walk this list.
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
		if c.Key == "" || c.Label == "" || c.Template == "" || c.Run == "" {
			t.Errorf("%q is missing a key, label, template or run", c.Key)
		}
		if c.Lang != "json" && c.Lang != "sh" {
			t.Errorf("%s has lang %q, which no renderer highlights", c.Key, c.Lang)
		}
		// Every block has to name the gateway and the model. One that named
		// neither would be a block that works for nobody in particular.
		for _, want := range []string{"{{base}}", "{{alias}}"} {
			if !strings.Contains(c.Template, want) {
				t.Errorf("%s's template never uses %s", c.Key, want)
			}
		}
	}
	// The OpenAI-compatible entry is the one that answers for a client nobody
	// wrote an entry for, so its absence is a regression rather than a choice.
	if _, found := Lookup("openai"); !found {
		t.Error("the catalogue has no OpenAI-compatible entry")
	}
}

// A JSON template has to still be JSON once it is filled in. A comma dropped
// while editing one of these is not otherwise caught by anything.
func TestJSONTemplatesRenderToValidJSON(t *testing.T) {
	for _, c := range Clients() {
		if c.Lang != "json" {
			continue
		}
		rendered := c.Render("https://keera.example.ch", "keera-code", 0)
		var doc map[string]any
		if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
			t.Errorf("%s does not render valid JSON: %v\n%s", c.Key, err, rendered)
		}
	}
}

// The key is read from the environment rather than baked in. The panel
// substitutes the real one into these at the moment a key is issued, and it
// finds the place to put it by matching these spellings.
func TestConfigsReadTheKeyFromTheEnvironment(t *testing.T) {
	for _, c := range Clients() {
		rendered := c.Render("https://keera.example.ch", "keera-code", 0)
		if !strings.Contains(rendered, "KEERA_API_KEY") {
			t.Errorf("%s's config never mentions KEERA_API_KEY:\n%s", c.Key, rendered)
		}
	}
}

// A client configured by a file says which file, and one configured by the
// environment says nothing rather than guessing at a shell profile.
func TestAFileClientNamesItsFile(t *testing.T) {
	for _, key := range []string{"opencode", "pi"} {
		c, found := Lookup(key)
		if !found {
			t.Fatalf("no client %s", key)
		}
		if c.Path == "" {
			t.Errorf("%s is configured by a file but names none", key)
		}
	}
	for _, key := range []string{"claude-code", "openai"} {
		c, _ := Lookup(key)
		if c.Path != "" {
			t.Errorf("%s names the file %q, but it is configured by environment variables",
				key, c.Path)
		}
	}
}

// Claude Code is the one client whose base URL is the gateway's root rather
// than its /v1: it appends the path itself, and a base carrying /v1 sends it to
// /v1/v1/messages.
func TestClaudeCodeTakesTheGatewayRoot(t *testing.T) {
	c, _ := Lookup("claude-code")
	rendered := c.Render("https://keera.example.ch", "keera-code", 0)
	if !strings.Contains(rendered, "export ANTHROPIC_BASE_URL=https://keera.example.ch\n") {
		t.Errorf("the base URL is not the gateway root:\n%s", rendered)
	}
}

// The clients a developer might use side by side have to describe the same
// model. They used not to: one pinned a temperature and the other an output
// cap, so the same alias behaved differently depending on which editor was
// open, and neither number came from the deployment.
func TestFileClientsStateTheSameWindow(t *testing.T) {
	const maxContext = 200_000
	window, output := Limits(maxContext)

	opencode, _ := Lookup("opencode")
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
	if err := json.Unmarshal([]byte(opencode.Render("https://keera.example.ch", "keera-code", maxContext)), &oc); err != nil {
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

	pi, _ := Lookup("pi")
	var p struct {
		Providers struct {
			Keera struct {
				APIKey string           `json:"apiKey"`
				Models []map[string]any `json:"models"`
			} `json:"keera"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(pi.Render("https://keera.example.ch", "keera-code", maxContext)), &p); err != nil {
		t.Fatalf("pi: %v", err)
	}
	if len(p.Providers.Keera.Models) != 1 {
		t.Fatalf("pi declares %d models, want 1", len(p.Providers.Keera.Models))
	}
	entry := p.Providers.Keera.Models[0]
	if got, _ := entry["contextWindow"].(float64); int(got) != window {
		t.Errorf("pi declares context %v, want %d", entry["contextWindow"], window)
	}
	// The sandbox image depends on this exact spelling: it writes this template
	// to ~/.pi/agent/models.json and Pi expands the variable itself, which is
	// what keeps a sandbox's credential in its environment rather than in a
	// file on a volume that outlives a suspend. A literal key here would be a
	// silent change of that property.
	if got := p.Providers.Keera.APIKey; got != "${KEERA_API_KEY}" {
		t.Errorf("pi apiKey = %q; the sandbox image relies on Pi expanding this "+
			"from the environment", got)
	}

	// And Pi is not, because its schema does not need one: how long an answer
	// may be is not something the catalogue records, so nothing here invents it
	// where it can be left out.
	if _, stated := entry["maxTokens"]; stated {
		t.Errorf("pi states an answer length the catalogue does not know: %v", entry["maxTokens"])
	}
}

// Only the client whose schema demands one is given an answer length. The
// others state the window and leave the length to their own defaults.
func TestOnlyOpenCodeStatesAnAnswerLength(t *testing.T) {
	for _, c := range Clients() {
		rendered := c.Render("https://keera.example.ch", "keera-code", 200_000)
		stated := strings.Contains(rendered, "maxTokens") ||
			strings.Contains(rendered, "MAX_OUTPUT_TOKENS") ||
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

// No entry pins a temperature. It is the one parameter that cannot be
// defaulted for a catalogue an operator fills in themselves: a reasoning model
// refuses a request that sets one, and a config that has to be edited before
// it works is the failure this package exists to prevent.
func TestNoClientPinsATemperature(t *testing.T) {
	for _, c := range Clients() {
		if strings.Contains(c.Render("https://keera.example.ch", "keera-code", 0), "temperature") {
			t.Errorf("%s's config sets a temperature", c.Key)
		}
	}
}
