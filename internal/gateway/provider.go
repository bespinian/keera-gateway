package gateway

import (
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// fitProvider fixes the few fields a hosted provider refuses in a request that
// is otherwise right for it.
//
// OpenAI's reasoning models - every model but the gpt-4 family and older -
// refuse max_tokens on chat completions and want max_completion_tokens, and
// refuse any temperature or top_p but their default. The gateway sends
// max_tokens whenever a key has an output ceiling, and its filters, routers and
// model checks send a temperature of 0. So both are fixed here rather than
// refused there. Dropping a sampling setting the model would reject anyway
// changes nothing it could have done.
//
// Anthropic's models since Opus 4.7 refuse temperature, top_p and top_k too, or
// every value but the default, so those are dropped the same way.
//
// Every other model's request is returned untouched.
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
	}
	return payload
}

// fitOpenAI is fitProvider for OpenAI.
func fitOpenAI(m policy.Model, path string, payload []byte) []byte {
	b, err := parseBody(payload)
	if err != nil {
		return payload
	}
	changed := false
	// Every current OpenAI chat model takes max_completion_tokens, so the rename
	// is safe for the older ones too.
	if raw, ok := b.fields["max_tokens"]; ok && path == "/chat/completions" {
		if _, has := b.fields["max_completion_tokens"]; !has {
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

// openAIReasons reports whether an OpenAI model is a reasoning model. It lists
// the older families that are not, so a model newer than this build counts as
// one: every new model OpenAI releases now is.
func openAIReasons(model string) bool {
	for _, older := range []string{"gpt-4", "gpt-3", "chatgpt-"} {
		if strings.HasPrefix(model, older) {
			return false
		}
	}
	return true
}

// anthropicSamples reports whether an Anthropic model still takes sampling
// settings. It lists the older models that do, so a model newer than this build
// counts as one that does not: no new model Anthropic releases does.
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
