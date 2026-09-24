package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// parseFilterReply reads a rewrite filter's answer: the segments rewritten, or
// a refusal.
//
// It is lenient about what surrounds the array (a code fence, some prose) and
// strict about the array: the wrong number of segments means part of the
// request was dropped or merged, and forwarding that would lose it.
//
// sent is what the filter was given. It tells a real refusal apart from a
// request about a refusal that was echoed back.
func parseFilterReply(raw []byte, sent []string) ([]string, error) {
	want := len(sent)
	text, err := completionText(raw)
	if err != nil {
		return nil, err
	}

	// A refusal on its own line, as the protocol asks. A single segment echoed
	// back unchanged is a filter that ignored the format, not a verdict, so it
	// falls through to the error it is.
	if reason, ok := refusalReason(text); ok && (want != 1 || text != sent[0]) {
		return nil, &filterRefusedError{reason: reason}
	}

	// Read from the first bracket to the last, past any fence or prose.
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

// completionText reads the one message a filter's model answered with. Both
// modes read the same envelope; only what is inside it differs.
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

// parseGateReply reads a gate's verdict, and how sure of it the model was. A
// nil error is a request that may go.
//
// The written word decides whenever it is one of the two answers: that is
// what an administrator checked before going live. The first token's
// distribution is only used when the text says neither, for instance when a
// small model put its verdict in bold or opened with a sentence. At
// temperature 0 the most likely first token is what the model would have
// written had it kept to the format. See choice.go.
//
// Anything neither reading can place is still an error, so a gate only lets a
// request through by clearly saying so.
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
		// It refused and explained instead of writing the word, so the
		// explanation becomes the reason, bounded like any other.
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

// gateUnwrittenReason stands in when a gate refused without a sentence. The
// sender is owed some reason.
const gateUnwrittenReason = "the guardrail refused this request without saying what it found"

// gateRefusalTokens and gateAllowTokens are the words a gate's answer may
// begin with: the one the protocol asks for, and a variant small models often
// write.
//
// Gates are read more leniently than rewrite filters, whose answers are the
// sender's own prose: a loose match there could drop a request because of how
// a sentence began. An unmatched gate answer is refused anyway.
var (
	gateRefusalTokens = []string{filterRefusalToken, "REFUSE"}
	gateAllowTokens   = []string{"ALLOW", "ALLOWED"}
)

// anyVerdict reads the first of these words that the answer begins with.
func anyVerdict(s string, words []string) (string, bool) {
	for _, word := range words {
		if reason, ok := verdictReason(s, word); ok {
			return reason, true
		}
	}
	return "", false
}

// refusalReason reads a refusal out of one line of a filter's answer, and
// reports whether that is what the line is.
//
// The word must stand alone: "REFUSEDXYZ" or prose that mentions it later is
// not a verdict. Dropping a request over a coincidence of spelling is the
// mistake to avoid.
func refusalReason(s string) (string, bool) {
	return verdictReason(s, filterRefusalToken)
}

// verdictReason reads a one-word answer, and the reason a model may have put
// after it, and reports whether the word is the one being looked for.
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
	// The reason follows a separator: the colon the protocol asks for, or one
	// a model writes instead.
	for _, sep := range []string{":", ".", "—", "-", " ", "\t", "\n", "\r"} {
		if after, ok := strings.CutPrefix(rest, sep); ok {
			return sanitizeReason(after), true
		}
	}
	return "", false
}

// unanimousRefusal reads a refusal the filter put in every slot of the array
// instead of on its own line, a mistake the protocol invites. Forwarding it
// would replace the whole request with the word REFUSED.
//
// A segment returned exactly as sent is an echo, not a verdict: a user asking
// why their connection was REFUSED is not asking to be refused.
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

// sanitizeReason reduces a filter's sentence to one bounded line of text.
//
// The sentence is a model's prose and ends up in an error body, a usage row
// and a log line. So whitespace runs become one space, control characters are
// dropped, and the length is capped on a rune boundary.
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
		if b.Len()+n > maxRefusalReasonBytes {
			truncated = true
			break
		}
		if gap {
			b.WriteByte(' ')
			gap = false
		}
		b.WriteRune(r)
	}
	if truncated {
		return b.String() + "…"
	}
	return b.String()
}
