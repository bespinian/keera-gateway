package gateway

import (
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// fitProvider fixes the few fields a hosted provider refuses in a request that
// is otherwise right for it.
//
// OpenAI's reasoning models - everything after gpt-4 - refuse max_tokens on
// chat completions and want max_completion_tokens, and refuse any temperature
// or top_p but their default. The gateway sends the first whenever a key has an
// output ceiling, and its filters, routers and model checks send a temperature
// of 0. So both are fixed here rather than refused there. Dropping a sampling
// setting the model would reject anyway changes nothing it could have done.
//
// Every other model's request is returned untouched.
func fitProvider(m policy.Model, path string, payload []byte) []byte {
	if m.Provider != "openai" || (path != "/chat/completions" && path != "/responses") {
		return payload
	}
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

// openAIReasons reports whether an OpenAI model is a reasoning model. Written
// as the exception, so a model newer than this build counts as one: every
// model OpenAI has released since gpt-4.1 is.
func openAIReasons(model string) bool {
	for _, older := range []string{"gpt-4", "gpt-3", "chatgpt-"} {
		if strings.HasPrefix(model, older) {
			return false
		}
	}
	return true
}
