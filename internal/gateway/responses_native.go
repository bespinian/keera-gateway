package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// responsesDialect implements dialect for `/v1/responses`, forwarded to
// OpenAI as it is.
type responsesDialect struct{}

func (responsesDialect) provider() string { return "openai" }

func (responsesDialect) path() string { return "/responses" }

func (responsesDialect) opening(b *body) (json.RawMessage, bool) {
	raw, _ := b.value("input")
	// Checked before decoding, so an array input is not read in full.
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '"' {
		text, err := textOf(t)
		return json.RawMessage(text), err == nil
	}
	return openingIn(raw, func(elem []byte) ([]chatMessage, error) {
		var it responsesItem
		if err := json.Unmarshal(elem, &it); err != nil {
			return nil, err
		}
		msgs, _, err := addItem(nil, it)
		return msgs, err
	})
}

// text is the instructions, and the text of every message and tool output in
// the input. Function call arguments and a custom tool's input are not text,
// as on the chat surface.
func (responsesDialect) text(b *body) (textDoc, error) {
	d := newTreeDoc()
	instructions, err := d.root(b, "instructions")
	if err != nil {
		return nil, err
	}
	if s, ok := instructions.(string); ok {
		d.add(s, func(v string) { d.roots["instructions"] = v })
	}
	input, err := d.root(b, "input")
	if err != nil {
		return nil, err
	}
	if s, ok := input.(string); ok {
		d.add(s, func(v string) { d.roots["input"] = v })
	}
	for _, item := range d.objects(input) {
		field := "content"
		switch item["type"] {
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output",
			"shell_call_output", "apply_patch_call_output":
			field = "output"
		}
		d.addField(item, field)
		for _, part := range d.objects(item[field]) {
			switch part["type"] {
			case "input_text", "output_text", "text":
				d.addField(part, "text")
			case "refusal":
				d.addField(part, "refusal")
			}
			// A shell call's output is one entry per command, with no type.
			if item["type"] == "shell_call_output" {
				d.addField(part, "stdout")
				d.addField(part, "stderr")
			}
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return d, nil
}

// addSystem puts the system prompt ahead of the client's instructions.
func (responsesDialect) addSystem(b *body, prompt string) error {
	if s, ok := b.str("instructions"); ok && s != "" {
		prompt += "\n\n" + s
	} else if _, present := b.value("instructions"); present {
		return errors.New("the 'instructions' field must be a string; " + promptAdded)
	}
	b.setString("instructions", prompt)
	return nil
}

// needsNative names the fields that continue a conversation OpenAI stored.
// The gateway stores none, so no other model can read it.
func (responsesDialect) needsNative(b *body) string {
	if field := storedConversation(b); field != "" {
		return "the '" + field + "' field continues a conversation OpenAI stored, and only " +
			"OpenAI's own models can read it; send the whole conversation in 'input' instead"
	}
	return ""
}

// storedConversation names the field that continues a conversation stored at
// OpenAI, or is empty when the body carries the whole conversation.
func storedConversation(b *body) string {
	for _, field := range []string{"previous_response_id", "conversation"} {
		if raw, ok := b.value(field); ok && !bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) {
			return field
		}
	}
	return ""
}

// onKey keeps a request on the deployment's key to one answer, given now.
// Every organisation shares that OpenAI account, so nothing is stored there
// for another to read by its id. A background answer is generated after the
// request ends, where the gateway cannot see or bill it.
func (responsesDialect) onKey(b *body) string {
	if on, _ := b.boolean("background"); on {
		return "the 'background' field is not available on this deployment's OpenAI key: " +
			"the answer would be generated where Keera cannot bill it. Leave it out"
	}
	if field := storedConversation(b); field != "" {
		return "the '" + field + "' field is not available on this deployment's OpenAI key, " +
			"which stores no conversations; send the whole conversation in 'input' instead"
	}
	b.set("store", json.RawMessage("false"))
	return ""
}

// auth is the bearer token every OpenAI endpoint takes.
func (responsesDialect) auth(http.Header) func(http.Header, string) { return nil }

func (responsesDialect) usage(raw []byte) *tokenUsage { return usageOf[responsesUsage](raw) }

func (responsesDialect) stream(alias string) nativeStream {
	return &responsesNativeStream{alias: alias}
}

// responsesNativeStream reads a Responses stream on its way through. The
// events that carry the whole response - created, completed and the like -
// carry the model name and, at the end, the usage record.
type responsesNativeStream struct {
	alias  string
	usage  *tokenUsage
	deltas int
	// ended says the response completed, or ended incomplete at a limit.
	// upstreamErr is the message of an error event or a failed response.
	ended       bool
	upstreamErr string
}

func (s *responsesNativeStream) event(payload []byte) []byte {
	typ := typeOf(payload)
	switch {
	case strings.HasSuffix(typ, ".delta"):
		s.deltas++
		return nil
	case typ == "error":
		s.upstreamErr = upstreamText(payload)
		return nil
	case !strings.HasPrefix(typ, "response.") || strings.Count(typ, ".") != 1:
		return nil
	}
	var ev struct {
		Response struct {
			Usage *responsesUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	decoded := json.Unmarshal(payload, &ev) == nil
	if decoded && ev.Response.Usage != nil {
		s.usage = ev.Response.Usage.tokens()
	}
	switch typ {
	case "response.completed", "response.incomplete":
		s.ended = true
	case "response.failed":
		s.upstreamErr = "the response failed"
		if decoded && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			s.upstreamErr = ev.Response.Error.Message
		}
	}
	return renameIn(payload, "response", s.alias)
}

func (s *responsesNativeStream) counts() (*tokenUsage, int, bool) {
	return s.usage, s.deltas, false
}

func (s *responsesNativeStream) failure() error {
	return endedFailure(s.upstreamErr, s.ended)
}
