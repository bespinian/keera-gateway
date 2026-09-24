package gateway

import (
	"fmt"
	"net/http"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The context window check: a request too long for every model it may go to
// is refused before anything is spent on it, in the words clients recognise.
//
// Counting tokens exactly needs each model's tokeniser, which the gateway does
// not have. So it counts the least a request can be: its text at a generous
// number of bytes per token. Real text is shorter than that per token, so the
// check never refuses a request that fits, and lets through some that do not,
// which the backend then refuses itself.
//
// The words matter. Claude Code compacts its conversation when it reads
// "prompt is too long", and OpenAI clients act on context_length_exceeded. A
// backend's own refusal says neither, and the agent stops.

// bytesPerTokenAtMost is more bytes than a token of real text takes. English
// prose averages about four; code and other languages fewer.
const bytesPerTokenAtMost = 5

// fits refuses a request that cannot fit in the context of any model it may
// go to. A model whose context is not declared can hold anything.
func (s *Server) fits(c *call, b *body) bool {
	window := 0
	for _, m := range c.chain {
		if m.MaxContext <= 0 {
			return true
		}
		window = max(window, m.MaxContext)
	}
	// The whole body is more than its text, so a body that fits needs no
	// closer look. That is nearly every request, and it costs nothing.
	if b.size()/bytesPerTokenAtMost <= window {
		return true
	}
	doc, err := extractText(b, c.surf.kind)
	if err != nil {
		return true // a body that cannot be read is refused where it is read
	}
	tokens := leastTokens(doc.texts(), c.surf.kind)
	if tokens <= window {
		return true
	}
	s.refuse(c, refusal{
		status: http.StatusBadRequest,
		typ:    "invalid_request_error", code: "context_length_exceeded",
		// Claude Code reads the numbers out of this sentence, so it keeps
		// Anthropic's wording.
		msg: fmt.Sprintf("prompt is too long: %d tokens > %d maximum. That is the least this "+
			"request can be, and '%s' holds no more; compact the conversation or start a new one",
			tokens, window, c.alias),
	})
	return false
}

// leastTokens is the fewest tokens the texts can be. A chat's texts are one
// prompt; the texts of a completion or an embedding are separate inputs, each
// of which has to fit on its own.
func leastTokens(texts []string, kind policy.Kind) int {
	total, longest := 0, 0
	for _, t := range texts {
		total += len(t)
		longest = max(longest, len(t))
	}
	if kind != policy.KindChat {
		total = longest
	}
	return total / bytesPerTokenAtMost
}
