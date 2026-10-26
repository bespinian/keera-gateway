package connect

import (
	"maps"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/catalog"
)

// Claude Code's model settings, shared by `keera connect claude-code` and the
// Claude Code block.

// claudeFamilies are the models Claude Code asks for by Anthropic's own names,
// for background work and in its /model picker, whatever its main model is.
var claudeFamilies = []string{"opus", "sonnet", "haiku", "fable"}

// ClaudeName is the name Claude Code is given for a model. Claude Code assumes
// a 200K context for a model it does not know; the [1m] suffix says 1M, and is
// taken off before the request is sent.
func ClaudeName(m Model) string {
	if m.MaxContext >= 1_000_000 {
		return m.Alias + "[1m]"
	}
	return m.Alias
}

// ClaudeEnv points Claude Code at main, and each family at the first of models
// that is one, or else at main. The gateway knows none of Anthropic's names,
// so every family must be set.
func ClaudeEnv(models []Model, main Model) map[string]string {
	env := map[string]string{"ANTHROPIC_MODEL": ClaudeName(main)}
	for _, family := range claudeFamilies {
		name := ClaudeName(main)
		for _, m := range models {
			if strings.Contains(m.BackendModel, family) {
				name = ClaudeName(m)
				break
			}
		}
		env["ANTHROPIC_DEFAULT_"+strings.ToUpper(family)+"_MODEL"] = name
	}
	return env
}

// ClaudeOverrides tells Claude Code which Claude model each alias is.
// Otherwise it guesses what they support, and sends Haiku adaptive thinking
// it refuses. names gives the other ids a model goes by, such as its dated
// one. The first model by alias wins an id two share.
func ClaudeOverrides(models []Model, names map[string][]string) map[string]string {
	overrides := map[string]string{}
	for _, m := range models {
		if m.Provider != "anthropic" {
			continue
		}
		for _, id := range append([]string{m.BackendModel}, names[m.BackendModel]...) {
			if _, taken := overrides[id]; !taken && id != "" {
				overrides[id] = m.Alias
			}
		}
	}
	return overrides
}

// ClaudeNames is the other ids each of Anthropic's models goes by: the dated
// id for the plain one, and the other way round.
func ClaudeNames() map[string][]string {
	anthropic, _ := catalog.ProviderByName("anthropic")
	names := map[string][]string{}
	for _, m := range anthropic.Models {
		if m.Snapshot != "" {
			names[m.ID] = append(names[m.ID], m.Snapshot)
			names[m.Snapshot] = append(names[m.Snapshot], m.ID)
		}
	}
	return names
}

// claudeEnvLines renders ClaudeEnv as lines of a settings.json "env" block,
// in a fixed order.
func claudeEnvLines(models []Model, main Model) string {
	env := ClaudeEnv(models, main)
	keys := []string{"ANTHROPIC_MODEL"}
	for _, family := range claudeFamilies {
		keys = append(keys, "ANTHROPIC_DEFAULT_"+strings.ToUpper(family)+"_MODEL")
	}
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, `    "`+k+`": "`+jsonText(env[k])+`"`)
	}
	return strings.Join(lines, ",\n")
}

// claudeOverridesBlock renders ClaudeOverrides as a "modelOverrides" member
// that follows another, or as nothing when there are none.
func claudeOverridesBlock(models []Model, names map[string][]string) string {
	overrides := ClaudeOverrides(models, names)
	if len(overrides) == 0 {
		return ""
	}
	lines := make([]string, 0, len(overrides))
	for _, id := range slices.Sorted(maps.Keys(overrides)) {
		lines = append(lines, `    "`+jsonText(id)+`": "`+jsonText(overrides[id])+`"`)
	}
	return ",\n  \"modelOverrides\": {\n" + strings.Join(lines, ",\n") + "\n  }"
}
