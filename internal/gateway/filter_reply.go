package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// parseFilterReply reads a rewrite filter's answer: the rewritten segments, or
// a refusal. It is lenient about what surrounds the array, such as a code
// fence, but strict about the segment count, since a wrong count means part of
// the request was lost. sent tells a real refusal from an echoed one.
func parseFilterReply(raw []byte, sent []string) ([]string, error) {
	want := len(sent)
	text, err := completionText(raw)
	if err != nil {
		return nil, err
	}

	// A single segment echoed back unchanged is not a verdict: the filter
	// ignored the format.
	if reason, ok := refusalReason(text); ok && (want != 1 || text != sent[0]) {
		return nil, &filterRefusedError{reason: reason}
	}

	start, end := strings.IndexByte(text, '['), strings.LastIndexByte(text, ']')
	if start < 0 || end < start {
		return nil, errors.New("it answered with prose rather than a JSON array; " +
			"this filter's model may be too small to follow the format")
	}

	var out []string
	if err = json.Unmarshal([]byte(text[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("its answer was not a JSON array of strings: %w", err)
	}
	if reason, ok := unanimousRefusal(out, sent); ok {
		return nil, &filterRefusedError{reason: reason}
	}
	if len(out) != want {
		return nil, fmt.Errorf("it returned %d segments where the request had %d, so part "+
			"of the request would have been lost", len(out), want)
	}
	return out, nil
}

// completionText reads the message a filter's model answered with.
func completionText(raw []byte) (string, error) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("its answer was not a completion: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return "", errors.New("its answer carried no completion")
	}
	text := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if text == "" {
		return "", errors.New("it answered with nothing")
	}
	return text, nil
}

// parseGateReply reads a gate's verdict and how sure the model was. A nil
// error means the request may go.
//
// The written word decides when it is one of the two answers, since that is
// what an administrator checked. The first token's distribution is used only
// when the text says neither. Anything else is an error, so a gate only allows
// a request by clearly saying so.
func parseGateReply(raw []byte) (float64, error) {
	allow, share, read := readVerdict(raw)

	text, err := completionText(raw)
	if err != nil {
		if !read {
			return 0, err
		}
		// It wrote nothing, so only the distribution is left.
		if allow {
			return share, nil
		}
		return share, &filterRefusedError{reason: gateUnwrittenReason}
	}

	if reason, ok := anyVerdict(text, gateRefusalTokens); ok {
		return share, &filterRefusedError{reason: reason}
	}
	if _, ok := anyVerdict(text, gateAllowTokens); ok {
		return share, nil
	}
	if strings.HasPrefix(text, "[") {
		return share, errors.New("it answered with a rewrite rather than a verdict; a gate is " +
			"asked whether a request may go, so its instruction has to say what to refuse " +
			"rather than what to replace")
	}
	if read {
		if allow {
			return share, nil
		}
		// It explained instead of writing the word, so that is the reason.
		reason := sanitizeReason(text)
		if reason == "" {
			reason = gateUnwrittenReason
		}
		return share, &filterRefusedError{reason: reason}
	}
	return 0, errors.New("it answered neither ALLOW nor REFUSED; this filter's model may be too " +
		"small to hold the format, or its instruction may be asking for something other " +
		"than a verdict")
}

// gateUnwrittenReason stands in when a gate refused without a sentence.
const gateUnwrittenReason = "the guardrail refused this request without saying what it found"

// gateRefusalTokens and gateAllowTokens are the words a gate's answer may
// begin with, including variants small models often write. Rewrite filters
// are read more strictly, since their answers are the sender's own prose.
var (
	gateRefusalTokens = []string{filterRefusalToken, "REFUSE"}
	gateAllowTokens   = []string{"ALLOW", "ALLOWED"}
)

// anyVerdict reads the first of words that s begins with.
func anyVerdict(s string, words []string) (string, bool) {
	for _, word := range words {
		if reason, ok := verdictReason(s, word); ok {
			return reason, true
		}
	}
	return "", false
}

// refusalReason reads a refusal from one line of a filter's answer. The word
// must stand alone at the start: "REFUSEDXYZ" or a later mention is not a
// verdict.
func refusalReason(s string) (string, bool) {
	return verdictReason(s, filterRefusalToken)
}

// verdictReason reports whether s starts with word, and returns any reason
// after it.
func verdictReason(s, word string) (string, bool) {
	// Emphasis or a fence around the word is still the word.
	s = strings.Trim(s, "`*#>_ \t\n\r")
	if len(s) < len(word) || !strings.EqualFold(s[:len(word)], word) {
		return "", false
	}
	rest := strings.TrimLeft(s[len(word):], "`*_")
	if rest == "" {
		return "", true
	}
	// The protocol asks for a colon, but models use others too.
	for _, sep := range []string{":", ".", "—", "-", " ", "\t", "\n", "\r"} {
		if after, ok := strings.CutPrefix(rest, sep); ok {
			return sanitizeReason(after), true
		}
	}
	return "", false
}

// unanimousRefusal reads a refusal the filter put in every slot of the array
// instead of on its own line. A segment returned as sent is an echo, not a
// verdict: a user asking why their connection was REFUSED is not refused.
func unanimousRefusal(out, sent []string) (string, bool) {
	if len(out) == 0 {
		return "", false
	}
	var reason string
	for i, s := range out {
		r, ok := refusalReason(s)
		if !ok {
			return "", false
		}
		if i < len(sent) && s == sent[i] {
			return "", false
		}
		if reason == "" {
			reason = r
		}
	}
	return reason, true
}

// sanitizeReason reduces a model's sentence to one bounded line, since it ends
// up in an error body, a usage row and a log line.
func sanitizeReason(s string) string {
	var (
		b         strings.Builder
		gap       bool
		truncated bool
	)
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsSpace(r):
			gap = b.Len() > 0
			continue
		case unicode.IsControl(r), r == utf8.RuneError:
			continue
		}
		n := utf8.RuneLen(r)
		if gap {
			n++
		}
		if b.Len()+n > policy.MaxRefusalReasonBytes {
			truncated = true
			break
		}
		if gap {
			b.WriteByte(' ')
			gap = false
		}
		b.WriteRune(r)
	}
	if !truncated {
		return b.String()
	}
	// Make room for the ellipsis, so the cap holds.
	out := b.String()
	for len(out)+len(ellipsis) > policy.MaxRefusalReasonBytes {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return strings.TrimRight(out, " ") + ellipsis
}

const ellipsis = "…"

// shorten cuts s to n bytes, between characters, and adds an ellipsis.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + ellipsis
}
