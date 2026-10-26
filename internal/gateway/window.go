package gateway

import (
	"fmt"
	"net/http"
)

// The context window check refuses a request too long for every model it may
// go to, before the key's filters run (a router's filter steps run before
// it). It counts the fewest tokens the text can be, so it never refuses a
// request that fits but may let through some the backend refuses.
//
// The wording matters: Claude Code compacts when it reads "prompt is too
// long", and OpenAI clients act on context_length_exceeded. A backend's own
// refusal says neither, and the agent stops.

// fits refuses a request that cannot fit in the context of any model it may
// go to. A model with no declared context can hold anything.
func (s *Server) fits(c *call, b *body) bool {
	window, largest := 0, c.alias
	for _, m := range c.chain {
		if m.MaxContext <= 0 {
			return true
		}
		if m.MaxContext > window {
			window, largest = m.MaxContext, m.Alias
		}
	}
	// Without a translation, every destination gets the client's own body.
	sent, extract := b, func(b *body) (textDoc, error) { return extractText(b, c.surf.kind) }
	if b == nil {
		sent, extract = c.native, c.surf.dialect.text
	}
	// The whole body is more than its text, so if it fits, the text does.
	// That covers nearly every request at no cost.
	if sent.size()/bytesPerTokenAtMost <= window {
		return true
	}
	doc, err := extract(sent)
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
			tokens, window, largest),
	})
	return false
}
