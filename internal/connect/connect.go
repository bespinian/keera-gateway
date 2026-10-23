// Package connect holds the configuration a developer needs to point a coding
// agent at a Keera Gateway.
//
// The control panel's Connect client dialog and `keera connect` both use
// this one catalogue, so they hand out the same configuration. Each block is a
// template, filled in with the gateway address and the models a key may call,
// with their limits: by `keera connect` on the developer's machine, and by the
// control plane for the panel.
package connect

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Client is one coding agent that can be pointed at the gateway.
type Client struct {
	// ID names it on the wire, where a request calls it its client, and as
	// the argument to `keera connect <client>`.
	ID    string `json:"client"`
	Label string `json:"label"`
	// About says in one line what the client is, for the panel's picker.
	About string `json:"about"`
	// Path is the file the configuration belongs in. It is empty for a block
	// that is a command to run rather than a file. A path under ~ is a config
	// that may exist already; any other is a new file in the current directory.
	Path string `json:"path,omitempty"`
	// Lang is how a renderer should highlight the block: json, toml, sh or
	// python.
	Lang string `json:"lang"`
	// Template is the block. It carries {{base}}, {{models}}, {{aliases}}
	// (every alias, comma-separated) and {{alias}} (the first, the default),
	// for Render to fill in. A block that holds the key itself, because its
	// file cannot read the environment, carries {{key}}. Claude Code's
	// carries {{claude_env}} and {{claude_overrides}}; see claude.go.
	Template string `json:"template"`
	// Names is the other ids each Anthropic model goes by, for
	// {{claude_overrides}}. See ClaudeNames.
	Names map[string][]string `json:"names,omitempty"`
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
	// Name is its name for people, or empty to make one from the alias.
	Name string `json:"name,omitempty"`
	// MaxContext is its window, or zero for the default. A router has none.
	MaxContext int `json:"max_context_tokens,omitempty"`
	// BackendModel and Provider say which model it is, for Claude Code. A
	// router has neither.
	BackendModel string `json:"backend_model,omitempty"`
	Provider     string `json:"provider,omitempty"`
}

// The limits a client is given.
//
// Every client that takes a context window gets the one the catalogue
// advertises, so an alias behaves the same in every editor. Codex takes one
// window for every model, so it gets the default model's. Only OpenCode gets
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

// KeyPlaceholder stands in a block for a key nobody has given.
const KeyPlaceholder = "keera_sk_…"

// Render fills in a client's template: base is the gateway as the developer's
// machine reaches it, and models what the key may call, the default first.
func (c Client) Render(base string, models []Model) string {
	return c.RenderKey(base, KeyPlaceholder, models)
}

// RenderKey is Render with key in a block that holds the key itself.
func (c Client) RenderKey(base, key string, models []Model) string {
	entries := make([]string, 0, len(models))
	for _, m := range models {
		entries = append(entries, fill(c.Entry, m))
	}
	return strings.NewReplacer(
		"{{base}}", strings.TrimRight(base, "/"),
		"{{key}}", jsonText(key),
		"{{models}}", strings.Join(entries, ",\n"),
		"{{aliases}}", strings.Join(aliases(models), ", "),
		"{{claude_env}}", claudeEnvLines(models, first(models)),
		"{{claude_overrides}}", claudeOverridesBlock(models, c.Names),
	).Replace(fill(c.Template, first(models)))
}

// HoldsKey says the block holds the key itself, rather than reading it from
// KEERA_API_KEY.
func (c Client) HoldsKey() bool { return strings.Contains(c.Template, "{{key}}") }

// RunFor is Run with the default model and every alias filled in.
func (c Client) RunFor(models []Model) string {
	run := fill(c.Run, first(models))
	return strings.ReplaceAll(run, "{{aliases}}", strings.Join(aliases(models), ", "))
}

// RunText is RunFor for a terminal.
func (c Client) RunText(models []Model) string { return plain(c.RunFor(models)) }

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
		"{{name}}", jsonText(cmp.Or(m.Name, Title(m.Alias))),
		"{{context}}", strconv.Itoa(window),
		"{{output}}", strconv.Itoa(output),
	).Replace(text)
}

// jsonText escapes a name for the JSON string every template puts {{name}} in.
// An administrator types a display name, so it may hold a quote.
func jsonText(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
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
// Keera Speed.
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
		if c.ID == key {
			return c, true
		}
	}
	return Client{}, false
}

// Clients is the catalogue. It is a copy, so a caller cannot change it for
// everyone else.
func Clients() []Client {
	out := slices.Clone(clients)
	for i := range out {
		out[i].Names = maps.Clone(out[i].Names)
	}
	return out
}

// The last entry is a plain request with cURL. Its address and key are all any
// other OpenAI-compatible client needs.
var clients = []Client{
	{
		ID:    "pi",
		Label: "Pi",
		About: "A minimal coding agent for the terminal.",
		Path:  "~/.pi/agent/models.json",
		Lang:  "json",
		Template: `{
  "providers": {
    "keera": {
      "baseUrl": "{{base}}/v1",
      "api": "openai-completions",
      "apiKey": "${KEERA_API_KEY}",
      "headers": { "X-Keera-Client": "pi" },
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
		ID:    "opencode",
		Label: "OpenCode",
		About: "An open-source coding agent for the terminal.",
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
		ID:    "claude-code",
		Label: "Claude Code",
		About: "Anthropic's coding agent, over the Messages API.",
		// The same settings `keera connect claude-code` writes. They hold the
		// key itself, because settings.json does not read the environment,
		// and modelOverrides has no environment variable. The base is the
		// gateway's root: Claude Code adds /v1/messages itself.
		Path: "~/.claude/settings.json",
		Lang: "json",
		Template: `{
  "env": {
    "ANTHROPIC_BASE_URL": "{{base}}",
    "ANTHROPIC_AUTH_TOKEN": "{{key}}",
{{claude_env}},
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }{{claude_overrides}}
}`,
		Names: ClaudeNames(),
		Note: "`keera login` and then `keera connect claude-code` write this for you, with " +
			"a key of this machine's own. Claude Code asks for Opus, Sonnet, Haiku and " +
			"Fable by Anthropic's names, so each is pointed at a model the key may use. " +
			"`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` turns off Claude Code's telemetry " +
			"to Anthropic. Do not put the key in a project's `.claude/settings.json`, " +
			"which is committed. If your Claude Team or Enterprise plan should pay " +
			"instead, run `keera login` and then `keera connect claude-code --subscription`.",
		Run: "Run `claude` in your project. It starts with {{alias}}. `/model <name>` " +
			"switches to another of {{aliases}}, and `/status` shows which gateway and " +
			"key it uses.",
	},
	{
		ID:    "codex",
		Label: "Codex",
		About: "OpenAI's coding agent, over the Responses API.",
		Path:  "~/.codex/config.toml",
		Lang:  "toml",
		Template: `# The key can use: {{aliases}}. Codex starts with this one.
model = "{{alias}}"
model_provider = "keera"
model_context_window = {{context}}

[model_providers.keera]
name = "Keera"
base_url = "{{base}}/v1"
env_key = "KEERA_API_KEY"
wire_api = "responses"`,
		Note: "If the file exists, put the first three settings above its first " +
			"`[section]`. TOML reads anything below a section as part of it.",
		Run: "Run `codex` in your project. It starts with {{alias}}. `codex -m <name>` " +
			"starts with another of the models above.",
	},
	{
		ID:    "python",
		Label: "Python",
		About: "Your own code, with OpenAI's Python library.",
		Path:  "hello.py",
		Lang:  "python",
		Template: `# pip install openai
import os
from openai import OpenAI

# Keera Gateway speaks the OpenAI API at {{base}}/v1.
client = OpenAI(base_url="{{base}}/v1", api_key=os.environ["KEERA_API_KEY"])

# The key can use: {{aliases}}.
reply = client.chat.completions.create(
    model="{{alias}}",
    messages=[{"role": "user", "content": "Hello"}],
)
print(reply.choices[0].message.content)`,
		Note: "Reports list its requests under OpenAI SDK, the library that sends them.",
		Run:  "Run `python hello.py`. The answer is printed.",
	},
	{
		ID:    "curl",
		Label: "cURL",
		About: "A first request from the shell, over the OpenAI API.",
		Lang:  "sh",
		Template: `# Keera Gateway speaks the OpenAI API at {{base}}/v1.
# The key can use: {{aliases}}.
curl {{base}}/v1/chat/completions \
  -H "Authorization: Bearer $KEERA_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"{{alias}}","messages":[{"role":"user","content":"Hello"}]}'`,
		Note: "Any other tool that takes an OpenAI base URL and key works the same way: " +
			"use the address above that ends in `/v1`, the key, and one of the models above.",
		Run: "Run it in a shell where `KEERA_API_KEY` is set. The answer comes back as JSON.",
	},
}
