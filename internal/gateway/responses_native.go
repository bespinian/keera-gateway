package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// addFileText collects an inline text file's name and contents. A rewrite is
// encoded the way the file came.
func addFileText(d *treeDoc, part map[string]any) {
	d.addField(part, "filename")
	fileData, _ := part["file_data"].(string)
	header, content, ok := inlineText(fileData)
	if !ok {
		return
	}
	d.add(content, func(v string) {
		if strings.HasSuffix(header, ";base64,") {
			v = base64.StdEncoding.EncodeToString([]byte(v))
		}
		part["file_data"] = header + v
	})
}

// responsesDialect implements dialect for `/v1/responses`, forwarded to
// OpenAI as is.
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

// text is the instructions and the text of every message and tool output.
// Call arguments and custom tool input are not text, as on the chat surface.
// An item or part it cannot read is refused.
func (d responsesDialect) text(b *body) (textDoc, error) { return strict(d.read(b)) }

func (responsesDialect) read(b *body) (*treeDoc, error) {
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
		typ, _ := item["type"].(string)
		field := "content"
		switch {
		case typ == "" || typ == "message":
		case outputItems[typ]:
			field = "output"
		case len(builtInResults[typ]) > 0:
			for _, f := range builtInResults[typ] {
				addResultStrings(d, item[f])
				d.addField(item, f)
			}
			continue
		case itemsWithoutText[typ]:
			continue
		default:
			d.cannotRead("an input item", typ)
			continue
		}
		d.addField(item, field)
		for _, part := range d.objects(item[field]) {
			// A shell call's output is one entry per command, with no type.
			if typ == "shell_call_output" {
				d.addField(part, "stdout")
				d.addField(part, "stderr")
				continue
			}
			addResponsesPart(d, part)
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return d, nil
}

// outputItems hold what a tool the client runs gave back, in "output".
var outputItems = map[string]bool{
	"function_call_output": true, "custom_tool_call_output": true,
	"local_shell_call_output": true, "shell_call_output": true,
	"apply_patch_call_output": true,
}

// builtInResults are the fields where OpenAI's own tools put what they found.
// The client sends them back on the next turn, and the model reads them.
var builtInResults = map[string][]string{
	"mcp_call":              {"output", "error"},
	"file_search_call":      {"results"},
	"code_interpreter_call": {"outputs"},
}

// itemsWithoutText carry no text a filter reads: calls the model wrote (as on
// the chat surface), screenshots, encrypted reasoning, and tool lists, whose
// descriptions are not read in the request's tools either.
var itemsWithoutText = map[string]bool{
	"function_call": true, "custom_tool_call": true, "local_shell_call": true,
	"shell_call": true, "apply_patch_call": true, "computer_call": true,
	"computer_call_output": true, "web_search_call": true, "image_generation_call": true,
	"tool_search_call": true, "tool_search_output": true, "mcp_list_tools": true,
	"mcp_approval_request": true, "mcp_approval_response": true, "reasoning": true,
	"item_reference": true, "compaction": true,
}

// addResponsesPart collects the text of one part of a message or a tool's
// output.
func addResponsesPart(d *treeDoc, part map[string]any) {
	typ, _ := part["type"].(string)
	switch typ {
	case "input_text", "output_text", "text":
		d.addField(part, "text")
	case "refusal":
		d.addField(part, "refusal")
	case "input_file":
		addFileText(d, part)
	case "input_image", "input_audio":
	default:
		d.cannotRead("a content part", typ)
	}
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

// needsNative names the field that continues a conversation stored at OpenAI,
// which no other model can read.
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

// onKey keeps a request on the deployment's key to one immediate answer.
// Every organisation shares that OpenAI account, so nothing is stored there
// for another to read by id. A background answer comes after the request
// ends, where the gateway cannot see or bill it.
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

// responsesNativeStream reads a Responses stream on its way through. Events
// carrying the whole response, such as created and completed, hold the model
// name and, at the end, the usage record.
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
