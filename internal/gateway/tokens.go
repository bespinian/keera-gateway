package gateway

import "github.com/bespinian/keera-gateway/internal/policy"

// The gateway has no tokeniser, so it counts tokens from bytes. Each estimate
// errs the way its use can afford: high where a low guess would let a request
// through that should not pass, low where a high guess would refuse one that
// fits.

// bytesPerToken sizes what wants a high guess: a filter's output allowance,
// whether a filter's or router's model can hold the text, a size router's
// choice, the credit a request holds, what a call with no usage record is
// billed, and count_tokens.
const bytesPerToken = 3

// bytesPerTokenAtMost is more bytes than a token of real text takes. English
// prose averages about four; code and other languages fewer. The context check
// uses it, so it never refuses a request that fits.
const bytesPerTokenAtMost = 5

// estimateTokens estimates a request's size from its text. It
// over-estimates, which sends a borderline request to the larger model.
//
// Images are not counted, so the MaxContext tier is a guard, not a guarantee.
func estimateTokens(texts []string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return n / bytesPerToken
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

// bytesPerTokenTypical is the usual rule of thumb for code. It sits between
// the two above because estimateInputTokens counts the whole JSON body, not
// just its text, and the envelope already makes that a slight over-estimate.
const bytesPerTokenTypical = 4

// estimateInputTokens is used when a stream on a key the deployment does not
// pay for ends without its usage record. It errs a little high, which is the
// right way to err for a guardrail.
func estimateInputTokens(payload []byte) int { return len(payload) / bytesPerTokenTypical }
