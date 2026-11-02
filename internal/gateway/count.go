package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A self-hosted model with a gateway fee is billed on the gateway's own count
// of its tokens, since its backend's usage record says whatever the backend
// likes. The count is what the gateway passed on: the whole request body, and
// the text the model wrote.

// countedTokens is the gateway's count for one buffered call.
func countedTokens(payload, answer []byte) policy.Tokens {
	return policy.Tokens{
		Input:  estimateInputTokens(payload),
		Output: writtenBytes(answer) / bytesPerTokenTypical,
	}
}

// writtenBytes is how much text a chat or text completion answer, or one
// chunk of its stream, carries: the content, the reasoning and the tool calls.
// An embedding or a rerank writes none.
func writtenBytes(payload []byte) int {
	var answer struct {
		Choices []struct {
			Text    string      `json:"text"`
			Message writtenText `json:"message"`
			Delta   writtenText `json:"delta"`
		} `json:"choices"`
	}
	// A field of an unexpected type fails the decode but the rest is still
	// read, so the error is ignored.
	_ = json.Unmarshal(payload, &answer)
	n := 0
	for _, c := range answer.Choices {
		n += len(c.Text) + c.Message.bytes() + c.Delta.bytes()
	}
	return n
}

type writtenText struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ToolCalls        []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (t writtenText) bytes() int {
	n := len(t.Content) + len(t.ReasoningContent) + len(t.Reasoning)
	for _, tc := range t.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n
}

// textCounter counts the text of a chat completion stream as the gateway
// reads it. It sits beside the pipe rather than in it, so a stream that is not
// counted decodes no more chunks than before.
type textCounter struct {
	line []byte
	// limit caps one line, as the pipe does. The pipe fails a longer one.
	limit int64
	n     int
}

func (c *textCounter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.add(p)
			break
		}
		c.add(p[:i])
		c.take()
		p = p[i+1:]
	}
	return n, nil
}

func (c *textCounter) add(p []byte) {
	if int64(len(c.line)+len(p)) <= c.limit {
		c.line = append(c.line, p...)
	}
}

func (c *textCounter) take() {
	line := bytes.TrimSpace(c.line)
	c.line = c.line[:0]
	if payload, ok := bytes.CutPrefix(line, dataPrefix); ok {
		c.n += writtenBytes(bytes.TrimSpace(payload))
	}
}

// tokens is the output counted so far, a last line without its newline
// included.
func (c *textCounter) tokens() int {
	c.take()
	return c.n / bytesPerTokenTypical
}
