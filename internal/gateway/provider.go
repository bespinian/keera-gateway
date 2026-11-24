package gateway

import (
	"encoding/json"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// fitProvider fixes the few fields a hosted provider refuses in an otherwise
// valid request. OpenAI's reasoning models (all but gpt-4 and older) want
// max_completion_tokens instead of max_tokens, and refuse a non-default
// temperature or top_p. Anthropic's models since Opus 4.7 refuse temperature,
// top_p and top_k. Mistral reads a temperature of 0 as greedy sampling and
// then wants top_p at 1, which is not its default. The gateway's own filters,
// routers and checks send a temperature of 0, so these are fixed here;
// changing a setting the model would reject changes nothing.
func fitProvider(m policy.Model, path string, payload []byte) []byte {
	switch {
	case m.Provider == "openai" && (path == "/chat/completions" || path == "/responses"):
		return fitOpenAI(m, path, payload)
	case m.Provider == "anthropic" && (path == "/messages" || path == "/chat/completions") &&
		!anthropicSamples(m.BackendModel):
		b, err := parseBody(payload)
		if err != nil {
			return payload
		}
		changed := false
		for _, field := range []string{"temperature", "top_p", "top_k"} {
			changed = b.remove(field) || changed
		}
		if !changed {
			return payload
		}
		return b.encode()
	case m.Provider == "mistral" && path == "/chat/completions":
		return fitMistral(payload)
	}
	return payload
}

// fitMistral sets top_p to 1 under greedy sampling. Greedy sampling ignores
// top_p, so a client's own top_p changes nothing there either.
func fitMistral(payload []byte) []byte {
	b, err := parseBody(payload)
	if err != nil {
		return payload
	}
	raw, ok := b.value("temperature")
	var t float64
	if !ok || json.Unmarshal(raw, &t) != nil || t != 0 {
		return payload
	}
	b.set("top_p", json.RawMessage("1"))
	return b.encode()
}

func fitOpenAI(m policy.Model, path string, payload []byte) []byte {
	b, err := parseBody(payload)
	if err != nil {
		return payload
	}
	changed := false
	// Every current OpenAI chat model takes max_completion_tokens, so the
	// rename is safe for older ones too. A null max_completion_tokens counts
	// as absent, or max_tokens would be lost.
	if raw, ok := b.fields["max_tokens"]; ok && path == "/chat/completions" {
		if _, has := b.value("max_completion_tokens"); !has {
			b.set("max_completion_tokens", raw)
		}
		b.remove("max_tokens")
		changed = true
	}
	if openAIReasons(m.BackendModel) {
		for _, field := range []string{"temperature", "top_p"} {
			changed = b.remove(field) || changed
		}
	}
	if !changed {
		return payload
	}
	return b.encode()
}

// openAIReasons reports whether an OpenAI model is a reasoning model. It
// lists the older families that are not, so new models count as reasoning.
func openAIReasons(model string) bool {
	for _, older := range []string{"gpt-4", "gpt-3", "chatgpt-"} {
		if strings.HasPrefix(model, older) {
			return false
		}
	}
	return true
}

// anthropicSamples reports whether an Anthropic model still takes sampling
// settings. It lists the older models that do, so new models count as not.
func anthropicSamples(model string) bool {
	for _, older := range []string{
		"claude-3", "claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-2",
		"claude-opus-4-5", "claude-opus-4-6", "claude-sonnet-4-0", "claude-sonnet-4-2",
		"claude-sonnet-4-5", "claude-sonnet-4-6", "claude-haiku-4-5",
	} {
		if strings.HasPrefix(model, older) {
			return true
		}
	}
	return false
}
