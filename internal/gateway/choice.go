package gateway

import (
	"encoding/json"
	"math"
	"strings"
)

// Routers and gates ask a model to choose: a router picks one destination, a
// gate answers ALLOW or REFUSED. Both choices are read here.
//
// Reading the written answer (chosenDestination, anyVerdict) has to cope with
// models that fence, explain or misspell their answer. Instead, this reads the
// distribution over the first token and keeps only the tokens that are
// answers, weighed against each other. That gives two things:
//
//   - The result is always one of the answers, so a well-formed backend no
//     longer produces a reply that means nothing.
//   - It comes with a number: the winner's share says how sure the model was.
//
// For a router it is also faster: the destinations are lettered, so the
// answer is one token.
//
// It needs a backend that returns logprobs. vLLM and llama.cpp do; most hosted
// providers do not, so the written answer is still read where there is no
// distribution. See decide and parseGateReply.

const (
	// routerLabels are the letters destinations are offered under. A letter
	// is one token in any vocabulary, while two aliases starting with 'keera-'
	// share their first token and could not be told apart.
	routerLabels = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	// routerTopLogprobs is how much of the first token's distribution to ask
	// for, leaving room for a model that puts something before the letter.
	// 20 is vLLM's default maximum, and asking for more is an error.
	routerTopLogprobs = 20
)

// labelled reports whether these destinations can be offered under letters. A
// router with more destinations than letters is asked for a name instead;
// nobody has one that large.
func labelled(offered []string) bool {
	return len(offered) > 0 && len(offered) <= len(routerLabels)
}

// logprobsEnvelope is the part of a completion this file reads: the
// distribution over the first token the model would have written.
type logprobsEnvelope struct {
	Choices []struct {
		Logprobs struct {
			Content []struct {
				Token       string  `json:"token"`
				Logprob     float64 `json:"logprob"`
				TopLogprobs []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
}

// firstToken reads the distribution over a completion's first token, as token
// to log-probability. It reports false when the backend served none.
func firstToken(raw []byte) (map[string]float64, bool) {
	var envelope logprobsEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}
	if len(envelope.Choices) == 0 || len(envelope.Choices[0].Logprobs.Content) == 0 {
		return nil, false
	}
	first := envelope.Choices[0].Logprobs.Content[0]

	// Servers differ on whether the sampled token is repeated in its own
	// top_logprobs, so it is added here and duplicates are collapsed.
	seen := make(map[string]float64, len(first.TopLogprobs)+1)
	keep := func(token string, logprob float64) {
		if was, ok := seen[token]; !ok || logprob > was {
			seen[token] = logprob
		}
	}
	keep(first.Token, first.Logprob)
	for _, alt := range first.TopLogprobs {
		keep(alt.Token, alt.Logprob)
	}
	return seen, true
}

// pick weighs the tokens that mean one of n answers against each other, and
// returns the answer with the most probability and its share.
//
// classify maps a token to its answer, or -1 for tokens that are none of
// them. Those take no part: what else the model might have written says
// nothing about this question.
func pick(seen map[string]float64, n int, classify func(string) int) (int, float64, bool) {
	// Log-sum-exp, so very negative logprobs still normalise instead of
	// becoming zero over zero.
	high := math.Inf(-1)
	for token, logprob := range seen {
		if classify(token) >= 0 && logprob > high {
			high = logprob
		}
	}
	if math.IsInf(high, -1) {
		return 0, 0, false
	}
	weights := make([]float64, n)
	var total float64
	for token, logprob := range seen {
		i := classify(token)
		if i < 0 || i >= n {
			continue
		}
		w := math.Exp(logprob - high)
		weights[i] += w
		total += w
	}

	// The lowest index wins a tie, so the same input always decides the same
	// way.
	best := 0
	for i, w := range weights {
		if w > weights[best] {
			best = i
		}
	}
	if weights[best] == 0 {
		return 0, 0, false
	}
	return best, weights[best] / total, true
}

// readChoice picks one of n destinations out of a completion's first token,
// and reports what share of the probability among the letters it took.
//
// A model that opens with '<think>' still chooses here, since only letters
// are counted. The small models routers run on answer directly anyway.
//
// It reports false when there is no distribution, or no letter in it.
func readChoice(raw []byte, n int) (index int, share float64, ok bool) {
	seen, got := firstToken(raw)
	if !got {
		return 0, 0, false
	}
	return pick(seen, n, func(token string) int { return labelIndex(token, n) })
}

// The two things a gate's first token can mean.
const (
	verdictAllow = iota
	verdictRefuse
)

// readVerdict reads a gate's verdict out of its first token, and reports the
// share of probability it took against the other verdict. ALLOW and REFUSED
// start with different letters, so no special protocol is needed.
func readVerdict(raw []byte) (allow bool, share float64, ok bool) {
	seen, got := firstToken(raw)
	if !got {
		return false, 0, false
	}
	i, share, ok := pick(seen, 2, verdictIndex)
	return i == verdictAllow, share, ok
}

// verdictIndex reads a token as the start of one of a gate's two answers, and
// returns -1 if it is neither.
//
// A tokeniser may return a prefix of the word ('A', 'AL') or more than it
// ('ALLOWED'), so either one starting the other counts. 'Absolutely' does
// not: a guardrail must not decide by spelling.
func verdictIndex(token string) int {
	t := strings.ToUpper(strings.TrimSpace(token))
	if t == "" {
		return -1
	}
	switch {
	case begins(t, "ALLOW"):
		return verdictAllow
	case begins(t, filterRefusalToken):
		return verdictRefuse
	}
	return -1
}

// begins reports whether either of these words starts the other.
func begins(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// labelIndex reads a token as one of the first n letters, and returns -1 if it
// is not one of them.
//
// A leading space and lower case are fine. Anything longer than one letter is
// not: 'Absolutely' is prose, not a vote for A.
func labelIndex(token string, n int) int {
	t := strings.TrimSpace(token)
	if len(t) != 1 {
		return -1
	}
	c := t[0]
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	i := int(c - 'A')
	if i < 0 || i >= n {
		return -1
	}
	return i
}
