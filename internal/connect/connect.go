// Package connect holds the configuration a developer needs to point a coding
// agent at a Keera Gateway.
//
// The control panel's "Connect a client" screen and `keera connect` both read
// this one catalogue, so they hand out the same configuration. Each block is a
// template; the caller fills in the gateway address and the models a key may
// call, with their limits.
package connect

import (
	"strconv"
	"strings"
	"unicode"
)

// Client is one coding agent that can be pointed at the gateway.
type Client struct {
	// Key names it on the wire and as the argument to `keera connect <key>`.
	Key   string `json:"key"`
	Label string `json:"label"`
	// Path is the file the configuration belongs in. It is empty for a client
	// configured by environment variables.
	Path string `json:"path,omitempty"`
	// Lang is how a renderer should highlight the block: json or sh.
	Lang string `json:"lang"`
	// Template is the block. It carries {{base}}, {{models}}, {{aliases}}
	// (every alias, comma-separated) and {{alias}} (the first, the default),
	// for Render to fill in.
	Template string `json:"template"`
	// Entry is what {{models}} repeats, once per model, joined with commas. It
	// carries {{alias}}, {{name}} and the two limits, {{context}} and
	// {{output}}. It is empty for a client that does not list its models.
	Entry string `json:"entry,omitempty"`
	// Run is what to type once it is configured, and Note a caveat, both in
	// prose. A span in backticks is something to type or a name to find;
	// the panel shows it as code, and RunText and NoteText as quoted.
	Run  string `json:"run"`
	Note string `json:"note,omitempty"`
}

// Model is one model or router a configuration names.
type Model struct {
	Alias string `json:"alias"`
	// MaxContext is its window, or zero for the default. A router has none.
	MaxContext int `json:"max_context,omitempty"`
}

// The limits a client is given.
//
// Every client that takes a context window gets the one the catalogue
// advertises, so an alias behaves the same in every editor. Only OpenCode gets
// an output length, because its schema needs one with the window; the others
// keep their own defaults. No entry sets a temperature, because reasoning
// models reject one.
const (
	// DefaultContext is the window for a model whose entry states none. Pi
	// assumes the same for a model it does not know.
	DefaultContext = 128_000
	// MaxOutput caps the answer length OpenCode is given. It is generous for
	// whole files, but an agent told it has unlimited room writes until the
	// backend cuts it off mid-patch.
	MaxOutput = 16_384
	// MinOutput is the shortest answer that is still a useful edit.
	MinOutput = 1_024
)

// Limits turns a model's context window into the window and output length a
// client config states.
//
// The output is at most a quarter of the window, because an agent spends the
// rest on the conversation so far: a 16k model allowed 16k of answer fails on
// its second turn.
func Limits(maxContext int) (window, output int) {
	window = maxContext
	if window <= 0 {
		window = DefaultContext
	}
	output = max(min(MaxOutput, window/4), MinOutput)
	return window, output
}

// Render fills in a client's template: base is the gateway as the developer's
// machine reaches it, and models what the key may call, the default first.
func (c Client) Render(base string, models []Model) string {
	entries := make([]string, 0, len(models))
	for _, m := range models {
		entries = append(entries, fill(c.Entry, m))
	}
	return strings.NewReplacer(
		"{{base}}", strings.TrimRight(base, "/"),
		"{{models}}", strings.Join(entries, ",\n"),
		"{{aliases}}", strings.Join(aliases(models), ", "),
	).Replace(fill(c.Template, first(models)))
}

// RunText is Run with the default model filled in, for a terminal.
func (c Client) RunText(models []Model) string { return plain(fill(c.Run, first(models))) }

// NoteText is Note for a terminal.
func (c Client) NoteText() string { return plain(c.Note) }

// plain quotes the backtick spans of catalogue prose, which a terminal does
// not render.
func plain(s string) string { return strings.ReplaceAll(s, "`", "'") }

// fill substitutes one model's placeholders.
func fill(text string, m Model) string {
	window, output := Limits(m.MaxContext)
	return strings.NewReplacer(
		"{{alias}}", m.Alias,
		"{{name}}", Title(m.Alias),
		"{{context}}", strconv.Itoa(window),
		"{{output}}", strconv.Itoa(output),
	).Replace(text)
}

func first(models []Model) Model {
	if len(models) == 0 {
		return Model{}
	}
	return models[0]
}

func aliases(models []Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Alias)
	}
	return out
}

// Title turns an alias into the label a model picker shows: keera-speed becomes
// Keera Speed. The panel's own title() has to agree with this one.
func Title(alias string) string {
	words := strings.FieldsFunc(alias, func(r rune) bool {
		switch r {
		case '-', '_', '.', ' ', '\t', '\n':
			return true
		}
		return false
	})
	for i, w := range words {
		runes := []rune(w)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// Find finds a client by key in clients: this catalogue, or the one a
// deployment sent.
func Find(clients []Client, key string) (Client, bool) {
	for _, c := range clients {
		if c.Key == key {
			return c, true
		}
	}
	return Client{}, false
}

// Clients is the catalogue.
func Clients() []Client { return clients }

// The last entry is the base URL and the key alone, which is all any other
// OpenAI-compatible client needs.
var clients = []Client{
	{
		Key:   "pi",
		Label: "Pi",
		Path:  "~/.pi/agent/models.json",
		Lang:  "json",
		Template: `{
  "providers": {
    "keera": {
      "baseUrl": "{{base}}/v1",
      "api": "openai-completions",
      "apiKey": "${KEERA_API_KEY}",
      "models": [
{{models}}
      ]
    }
  }
}`,
		Entry: `        {
          "id": "{{alias}}",
          "name": "{{name}}",
          "contextWindow": {{context}}
        }`,
		Run: "Run `pi` in your project, then `/model`, and pick one of the models above.",
	},
	{
		Key:   "opencode",
		Label: "OpenCode",
		Path:  "~/.config/opencode/opencode.json",
		Lang:  "json",
		Template: `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "keera": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Keera",
      "options": {
        "baseURL": "{{base}}/v1",
        "apiKey": "{env:KEERA_API_KEY}"
      },
      "models": {
{{models}}
      }
    }
  }
}`,
		Entry: `        "{{alias}}": {
          "name": "{{name}}",
          "tool_call": true,
          "limit": {
            "context": {{context}},
            "output": {{output}}
          }
        }`,
		Run: "Run `opencode` in your project, then `/models`, and pick one of the models under Keera.",
	},
	{
		Key:   "claude-code",
		Label: "Claude Code",
		// No file: settings.json takes the key as a literal, and we have no
		// key to put in it. The note explains.
		Lang: "sh",
		Template: `# Claude Code uses the Anthropic Messages API, which the gateway serves at
# /api/v1/messages. Claude Code adds /v1/messages itself.
export ANTHROPIC_BASE_URL={{base}}
export ANTHROPIC_AUTH_TOKEN=$KEERA_API_KEY

# The key can use: {{aliases}}. Claude Code starts with this one.
export ANTHROPIC_MODEL={{alias}}

# Turn off Claude Code's telemetry to Anthropic. It holds no prompts, but it
# should be off here.
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# Test it from the shell first:
curl "$ANTHROPIC_BASE_URL/v1/messages" \
  -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"{{alias}}","max_tokens":16,"messages":[{"role":"user","content":"Hello"}]}'`,
		Note: "To make this permanent, and for background sessions that do not read your " +
			"shell, put the same variables in the `env` block of `~/.claude/settings.json`. " +
			"Use the key itself there, because that file does not expand shell variables. " +
			"To fetch the key from a vault, use the `apiKeyHelper` setting instead. Do not " +
			"put it in a project's `.claude/settings.json`, which is committed. If your " +
			"Claude Team or Enterprise plan should pay instead, run `keera login` and then " +
			"`keera connect claude-code --subscription`.",
		Run: "Run `claude` in your project. It starts with {{alias}}. `/model <name>` " +
			"switches to another of the models above, and `/status` shows which gateway " +
			"and key it uses.",
	},
	{
		Key:   "openai",
		Label: "Anything OpenAI-compatible",
		// No file: where the two variables go depends on the client.
		Lang: "sh",
		Template: `# Keera Gateway speaks the OpenAI API. Most clients need only these two
# settings and a model name. The key can use: {{aliases}}.
export OPENAI_BASE_URL={{base}}/v1
export OPENAI_API_KEY=$KEERA_API_KEY

# Test it from the shell first:
curl "$OPENAI_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"{{alias}}","messages":[{"role":"user","content":"Hello"}]}'`,
		Run: "Start anything that reads `OPENAI_BASE_URL` (the official SDKs, Aider, your " +
			"own scripts) with one of the models above.",
	},
}
