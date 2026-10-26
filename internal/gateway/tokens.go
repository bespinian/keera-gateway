package gateway

import "github.com/bespinian/keera-gateway/internal/policy"

// The gateway has no tokeniser, so it estimates tokens from bytes. Each
// estimate errs on the safe side for its use: high where a low guess would
// let through what should not pass, low where a high guess would refuse what
// fits.

// bytesPerToken is for high guesses: filter output allowances, context fits
// for filter and route step models, size steps, credit holds, billing without
// a usage record, and count_tokens.
const bytesPerToken = 3

// bytesPerTokenAtMost is more bytes than a token of real text takes (English
// averages about four). The context check uses it, so it never refuses a
// request that fits.
const bytesPerTokenAtMost = 5

// estimateTokens estimates a request's size from its text. It over-estimates,
// which sends a borderline request to the larger model. Images are not
// counted, so the MaxContext tier is a guard, not a guarantee.
func estimateTokens(texts []string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return n / bytesPerToken
}

// leastTokens is the fewest tokens the texts can be. A chat's texts are one
// prompt; a completion's or embedding's are separate inputs that must each
// fit.
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
// the two above because estimateInputTokens counts the whole JSON body, which
// already over-estimates slightly.
const bytesPerTokenTypical = 4

// estimateInputTokens is used when a stream not on the deployment's key ends
// without its usage record. It errs a little high, as a guardrail should.
func estimateInputTokens(payload []byte) int { return len(payload) / bytesPerTokenTypical }
