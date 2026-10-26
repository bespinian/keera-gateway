package gateway

import (
	"encoding/json"
	"math"
	"strings"
)

// Route steps pick a destination and gates answer ALLOW or REFUSED. Instead of
// parsing the written answer, which models fence, explain or misspell, this
// weighs the answer tokens in the first token's distribution. The winner's
// share says how sure the model was.
//
// This needs logprobs, which vLLM and llama.cpp return but most hosted
// providers do not. Without them the written answer is read instead.

const (
	// choiceLetters label the destinations. A letter is one token in any
	// vocabulary, while aliases like 'keera-a' and 'keera-b' share their first
	// token.
	choiceLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	// topLogprobs is vLLM's default maximum; asking for more is an error.
	topLogprobs = 20
)

// labelled reports whether these destinations fit under letters. Otherwise
// the model is asked for a name.
func labelled(offered []string) bool {
	return len(offered) > 0 && len(offered) <= len(choiceLetters)
}

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

// firstToken maps each candidate first token to its log-probability. It
// reports false when the backend sent none.
func firstToken(raw []byte) (map[string]float64, bool) {
	var envelope logprobsEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}
	if len(envelope.Choices) == 0 || len(envelope.Choices[0].Logprobs.Content) == 0 {
		return nil, false
	}
	first := envelope.Choices[0].Logprobs.Content[0]

	// Servers differ on whether top_logprobs repeats the sampled token, so it
	// is added here and duplicates are collapsed.
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

// pick returns the most likely of n answers and its share of the probability
// among answer tokens. classify maps a token to its answer, or -1 to ignore it.
func pick(seen map[string]float64, n int, classify func(string) int) (int, float64, bool) {
	// Subtract the highest logprob, so very negative ones don't become 0/0.
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

	// The lowest index wins a tie, so the result is stable.
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

// readChoice picks one of n destinations from the first token, with its share
// among the letters. Only letters count, so a model that opens with '<think>'
// still chooses.
func readChoice(raw []byte, n int) (index int, share float64, ok bool) {
	seen, got := firstToken(raw)
	if !got {
		return 0, 0, false
	}
	return pick(seen, n, func(token string) int { return labelIndex(token, n) })
}

const (
	verdictAllow = iota
	verdictRefuse
)

// readVerdict reads a gate's verdict from the first token, with its share
// against the other verdict. ALLOW and REFUSED start with different letters.
func readVerdict(raw []byte) (allow bool, share float64, ok bool) {
	seen, got := firstToken(raw)
	if !got {
		return false, 0, false
	}
	i, share, ok := pick(seen, 2, verdictIndex)
	return i == verdictAllow, share, ok
}

// verdictIndex maps a token to a gate answer, or -1. A token may be a prefix
// ('AL') or longer ('ALLOWED'), so either one starting the other counts.
// 'Absolutely' does not.
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

func begins(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// labelIndex maps a token to one of the first n letters, or -1. Spaces and
// lower case are fine, but 'Absolutely' is not a vote for A.
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
