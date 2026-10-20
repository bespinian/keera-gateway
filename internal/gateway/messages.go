package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/id"
)

// This file is the Anthropic Messages surface: `POST /v1/messages` in, the
// OpenAI chat shape the inference plane speaks out, and back again. Claude
// Code and several other agents speak only that API.
//
// Fields the OpenAI shape has no place for are dropped on the way in: they
// are hints, and the model answers without them. Everything the client needs
// to read the answer is rebuilt on the way out.

// anthropicShape implements shape for `/v1/messages`.
type anthropicShape struct{}

// ------------------------------------------------------------------- requests

// anthropicRequest is the part of a Messages request the OpenAI shape has a
// place for. Unknown fields are ignored, not rejected, so a client release
// that adds a field does not break against the gateway.
type anthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens *int            `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools         []anthropicTool      `json:"tools"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice"`
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	TopK          *int                 `json:"top_k"`
	StopSequences []string             `json:"stop_sequences"`
	Stream        bool                 `json:"stream"`
}

// anthropicTool is one declared tool. Beta fields such as `strict` or
// `defer_loading` describe how to treat the schema and have no OpenAI
// counterpart, so they are not read.
type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use"`
}

// anthropicBlock is one content block. Block types are told apart by `type`
// on a flat object, so one struct with all their fields reads every kind.
type anthropicBlock struct {
	Type string   `json:"type"`
	Text jsonText `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result, and a search_result's own blocks
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`

	// An object for an image or a document, but the address a search_result
	// came from, so it is read for each type.
	Source json.RawMessage `json:"source"`
	// document and search_result
	Title string `json:"title"`
}

// anthropicSource is where an image's or a document's bytes are.
type anthropicSource struct {
	Type      string          `json:"type"`
	MediaType string          `json:"media_type"`
	Data      jsonText        `json:"data"`
	URL       string          `json:"url"`
	Content   json.RawMessage `json:"content"`
}

func (b anthropicBlock) source() (anthropicSource, bool) {
	var s anthropicSource
	if len(b.Source) == 0 || b.Source[0] != '{' || json.Unmarshal(b.Source, &s) != nil {
		return s, false
	}
	return s, true
}

// decodeBlocks reads a list of content blocks one by one. A block of a type
// the translation does not use is dropped even when its fields do not fit
// this struct, so a new block type cannot make the whole request fail.
func decodeBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	blocks := make([]anthropicBlock, 0, len(items))
	for i, item := range items {
		var b anthropicBlock
		if err := json.Unmarshal(item, &b); err != nil {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(item, &head)
			switch head.Type {
			case "text", "image", "tool_use", "tool_result":
				return nil, fmt.Errorf("content block %d (%s): %w", i, head.Type, err)
			}
			continue
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// blockText is the text a block gives the model: a text block's text, a
// plain-text document, or a search result's title, address and text. A
// local model can read these, so they are kept rather than dropped.
func blockText(b anthropicBlock) jsonText {
	switch b.Type {
	case "text":
		return b.Text
	case "document":
		s, ok := b.source()
		switch {
		case !ok:
			return nil
		case s.Type == "text":
			return joinTexts([]jsonText{quoteText(b.Title), s.Data})
		case s.Type == "content":
			inner, _ := decodeBlocks(s.Content)
			return joinTexts([]jsonText{quoteText(b.Title), joinText(inner)})
		}
	case "search_result":
		var url string
		_ = json.Unmarshal(b.Source, &url)
		inner, _ := decodeBlocks(b.Content)
		return joinTexts([]jsonText{quoteText(b.Title), quoteText(url), joinText(inner)})
	}
	return nil
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON document in a string, not an object: the one way
	// the two APIs' tool calls differ.
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string             `json:"type"`
	Function oaiToolDeclaration `json:"function"`
}

type oaiToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// decodeRequest reads a request body into in. api names the API in the
// error, which the client is shown.
func decodeRequest(raw []byte, in any, api string) error {
	err := json.Unmarshal(raw, in)
	if err == nil {
		return nil
	}
	if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && typeErr.Field != "" {
		return errors.New("the '" + typeErr.Field + "' field has the wrong type")
	}
	return errors.New("request body is not a valid " + api + " request: " + err.Error())
}

func functionTool(name, description string, schema json.RawMessage) oaiTool {
	if len(schema) == 0 || string(schema) == "null" {
		schema = emptySchema
	}
	return oaiTool{
		Type:     "function",
		Function: oaiToolDeclaration{Name: name, Description: description, Parameters: schema},
	}
}

// emptySchema stands in for a tool declared without one. vLLM rejects a
// function without parameters, and tools without arguments are real.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// decodeBody turns a Messages request into a chat completion request. The
// body is built field by field, so it need not be parsed again.
func (sh anthropicShape) decodeBody(raw []byte) (*body, shape, error) {
	// A missing model is left to serve, which refuses it the same way on
	// every API.
	var in anthropicRequest
	if err := decodeRequest(raw, &in, "Messages"); err != nil {
		return nil, nil, err
	}
	if len(in.Messages) == 0 {
		return nil, nil, errors.New("the 'messages' field is required and must not be empty")
	}

	msgs, err := convertMessages(in)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{}
	// max_tokens is required by the Messages API, so it is nearly always
	// here, and it is what the guardrail ceiling clamps in serve.
	addSampling(out, in.MaxTokens, in.Stream, in.Temperature, in.TopP)
	if in.TopK != nil {
		// Not an OpenAI field. vLLM accepts it as an extension, and other
		// backends ignore it.
		out["top_k"] = *in.TopK
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if tools := convertTools(in.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if in.ToolChoice != nil {
		addToolChoice(out, *in.ToolChoice)
	}

	b, err := chatBody(in.Model, msgs, out, len(raw))
	return b, sh, err
}

// chatBody builds a chat completion request from its messages and the other
// fields. size is the request it came from: the messages are most of it, so
// their buffer starts at that size.
func chatBody(model string, msgs []chatMessage, out map[string]any, size int) (*body, error) {
	b := &body{fields: make(map[string]json.RawMessage, len(out)+2)}
	b.setString("model", model)
	b.set("messages", appendMessages(make([]byte, 0, size), msgs))
	for _, k := range slices.Sorted(maps.Keys(out)) {
		v, err := json.Marshal(out[k])
		if err != nil {
			return nil, err
		}
		b.set(k, v)
	}
	return b, nil
}

// convertMessages turns the system prompt and every turn into OpenAI messages.
func convertMessages(in anthropicRequest) ([]chatMessage, error) {
	msgs := make([]chatMessage, 0, len(in.Messages)+1)
	if sys := systemText(in.System); !sys.isEmpty() {
		msgs = append(msgs, chatMessage{role: "system", content: sys})
	}
	for i, m := range in.Messages {
		converted, err := convertMessage(m.Role, m.Content)
		if err != nil {
			return nil, errors.New("messages[" + strconv.Itoa(i) + "]: " + err.Error())
		}
		msgs = append(msgs, converted...)
	}
	return msgs, nil
}

// addSampling copies the output limit and the sampling settings that both
// native APIs share with chat completions.
func addSampling(out map[string]any, maxTokens *int, stream bool, temperature, topP *float64) {
	if maxTokens != nil {
		out["max_tokens"] = *maxTokens
	}
	if stream {
		out["stream"] = true
	}
	if temperature != nil {
		out["temperature"] = *temperature
	}
	if topP != nil {
		out["top_p"] = *topP
	}
}

// convertTools declares the tools as OpenAI functions. Server tools, such as
// web search, run on Anthropic's servers, so no other model has them.
func convertTools(in []anthropicTool) []oaiTool {
	tools := make([]oaiTool, 0, len(in))
	for _, t := range in {
		if t.Name == "" || !keepsTool(t.Type, anthropicClientTools) {
			continue
		}
		tools = append(tools, functionTool(t.Name, t.Description, t.InputSchema))
	}
	return tools
}

// addToolChoice maps the tool choice onto its OpenAI spelling.
func addToolChoice(out map[string]any, tc anthropicToolChoice) {
	switch tc.Type {
	case "auto":
		out["tool_choice"] = "auto"
	case "any":
		out["tool_choice"] = "required"
	case "none":
		out["tool_choice"] = "none"
	case "tool":
		if tc.Name != "" {
			out["tool_choice"] = map[string]any{
				"type": "function", "function": map[string]string{"name": tc.Name},
			}
		}
	}
	if tc.DisableParallelToolUse != nil {
		out["parallel_tool_calls"] = !*tc.DisableParallelToolUse
	}
}

// systemText flattens the system prompt, which the Messages API sends either as
// a string or as an array of text blocks.
func systemText(raw json.RawMessage) jsonText {
	if len(raw) == 0 {
		return nil
	}
	if t, err := textOf(raw); err == nil {
		return t
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return joinText(blocks)
}

// convertMessage turns one Messages turn into one or more OpenAI messages.
//
// Tool results are why it can be more than one. The Messages API puts them
// in a user turn; the OpenAI shape wants each as its own "tool" message, placed
// before any user text so it still follows the assistant turn that called it.
func convertMessage(role string, content json.RawMessage) ([]chatMessage, error) {
	// A system turn between the others is a beta Claude Code sends
	// (mid-conversation-system). It stays a system message in its place, as
	// on the Responses API.
	if role != "user" && role != "assistant" && role != "system" {
		return nil, errors.New("'role' must be 'user', 'assistant' or 'system'")
	}
	if len(content) == 0 {
		return nil, errors.New("'content' is required")
	}

	// The common shape by a wide margin: content is a bare string.
	if text, err := textOf(content); err == nil {
		return []chatMessage{{role: role, content: text}}, nil
	}
	if len(bytes.TrimSpace(content)) == 0 || bytes.TrimSpace(content)[0] != '[' {
		return nil, errors.New("'content' must be a string or an array of content blocks")
	}
	blocks, err := decodeBlocks(content)
	if err != nil {
		return nil, fmt.Errorf("'content' must be a string or an array of content blocks: %w", err)
	}

	switch role {
	case "assistant":
		return []chatMessage{convertAssistant(blocks)}, nil
	case "system":
		text := joinText(blocks)
		if text == nil {
			text = emptyText
		}
		return []chatMessage{{role: "system", content: text}}, nil
	}
	return convertUser(blocks), nil
}

// convertAssistant collapses an assistant turn. Its text becomes the content
// and its tool_use blocks become tool_calls. Thinking blocks have no
// counterpart and are dropped.
func convertAssistant(blocks []anthropicBlock) chatMessage {
	msg := chatMessage{role: "assistant"}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 && string(b.Input) != "null" {
			args = string(b.Input)
		}
		msg.toolCalls = append(msg.toolCalls, oaiToolCall{
			ID: b.ID, Type: "function", Function: oaiFunction{Name: b.Name, Arguments: args},
		})
	}
	msg.content = joinText(blocks)
	// An empty assistant turn gets an empty string. Upstream rejects a
	// message without content, and dropping the turn would break the
	// user/assistant alternation the chat template needs.
	if msg.content == nil && len(msg.toolCalls) == 0 {
		msg.content = emptyText
	}
	return msg
}

// convertUser splits a user turn into the tool results it carries and whatever
// the person actually said.
func convertUser(blocks []anthropicBlock) []chatMessage {
	var (
		out   []chatMessage
		parts []chatPart
	)
	for _, b := range blocks {
		switch b.Type {
		case "text", "document", "search_result":
			if text := blockText(b); !text.isEmpty() {
				parts = append(parts, chatPart{text: text})
			}
		case "image":
			if p, ok := imagePart(b); ok {
				parts = append(parts, p)
			}
		case "tool_result":
			text, images := toolResult(b.Content)
			out = append(out, chatMessage{role: "tool", toolCallID: b.ToolUseID, content: text})
			// The OpenAI tool role carries only text, so images from a tool
			// move to the user turn after the results, where the model reads
			// them.
			parts = append(parts, images...)
		}
		// Other blocks (thinking, redacted_thinking, a PDF document,
		// tool_reference) have no counterpart and are dropped.
	}

	switch {
	case len(parts) == 0:
		// Only tool results, so no empty user turn after them.
	case len(parts) == 1 && parts[0].imageURL == nil:
		out = append(out, chatMessage{role: "user", content: parts[0].text})
	default:
		out = append(out, chatMessage{role: "user", parts: parts})
	}
	if len(out) == 0 {
		out = append(out, chatMessage{role: "user", content: emptyText})
	}
	return out
}

// imagePart converts an image block. The Messages API carries the bytes
// inline; the OpenAI shape carries the same bytes as a data URL.
func imagePart(b anthropicBlock) (chatPart, bool) {
	src, ok := b.source()
	if !ok {
		return chatPart{}, false
	}
	switch src.Type {
	case "url":
		if src.URL == "" {
			return chatPart{}, false
		}
		return chatPart{imageURL: quoteText(src.URL)}, true
	case "base64":
		if src.Data.isEmpty() || src.MediaType == "" {
			return chatPart{}, false
		}
		return chatPart{imageURL: dataURL(src.MediaType, src.Data)}, true
	}
	return chatPart{}, false
}

// dataURL makes the data URL of inline bytes. An image can be megabytes, so
// its base64 is copied as it came rather than decoded.
func dataURL(mediaType string, data jsonText) jsonText {
	media := quoteText(mediaType)
	out := make(jsonText, 0, len(media)+len(data)+16)
	out = append(out, `"data:`...)
	out = append(out, media[1:len(media)-1]...)
	out = append(out, ";base64,"...)
	out = append(out, data[1:len(data)-1]...)
	return append(out, '"')
}

// toolResult flattens a tool result's content, which is a string or an array of
// blocks, and separates out any images it carried.
//
// `is_error` is dropped: the OpenAI shape has no field for it, and a failed
// tool says so in its text anyway.
func toolResult(raw json.RawMessage) (jsonText, []chatPart) {
	if len(raw) == 0 {
		return emptyText, nil
	}
	if text, err := textOf(raw); err == nil {
		return text, nil
	}
	blocks, err := decodeBlocks(raw)
	if err != nil {
		return quoteText(string(raw)), nil
	}
	var images []chatPart
	for _, b := range blocks {
		if b.Type == "image" {
			if p, ok := imagePart(b); ok {
				images = append(images, p)
			}
		}
	}
	return orEmpty(joinText(blocks)), images
}

// joinText joins the text of the blocks in a list with a blank line. Running them
// together would glue the last word of one to the first of the next.
func joinText(blocks []anthropicBlock) jsonText {
	texts := make([]jsonText, 0, len(blocks))
	for _, b := range blocks {
		texts = append(texts, blockText(b))
	}
	return joinTexts(texts)
}

// chatMessage is one message of the chat request a Messages or Responses
// request becomes.
// It is written out by hand, so its text is copied rather than escaped again.
type chatMessage struct {
	role string
	// content is left out when nil: an assistant turn of only tool calls has
	// none.
	content jsonText
	// parts replaces content when the turn has images.
	parts      []chatPart
	toolCalls  []oaiToolCall
	toolCallID string
}

// chatPart is text or, when imageURL is set, an image.
type chatPart struct {
	text     jsonText
	imageURL jsonText
}

// openingOfChat is the first user message in msgs. It writes the content
// exactly as appendMessages does, so a request keeps its session whether or
// not it is translated.
func openingOfChat(msgs []chatMessage) (json.RawMessage, bool) {
	for _, m := range msgs {
		if m.role == "user" {
			return m.appendContent(nil), true
		}
	}
	return nil, false
}

// openingIn is the first user message in an array of a native API's
// messages, each turned into chat messages by convert. One convert cannot read
// is passed over.
func openingIn(raw json.RawMessage, convert func([]byte) ([]chatMessage, error)) (json.RawMessage, bool) {
	var (
		opening json.RawMessage
		found   bool
	)
	eachElement(raw, func(elem []byte) bool {
		msgs, err := convert(elem)
		if err != nil {
			return true
		}
		opening, found = openingOfChat(msgs)
		return !found
	})
	return opening, found
}

// appendContent writes the content: the text, or the parts.
func (m chatMessage) appendContent(dst []byte) []byte {
	if m.parts == nil {
		return append(dst, m.content...)
	}
	dst = append(dst, '[')
	for i, p := range m.parts {
		if i > 0 {
			dst = append(dst, ',')
		}
		if p.imageURL != nil {
			dst = append(dst, `{"type":"image_url","image_url":{"url":`...)
			dst = append(dst, p.imageURL...)
			dst = append(dst, "}}"...)
			continue
		}
		dst = append(dst, `{"type":"text","text":`...)
		dst = append(dst, p.text...)
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

func appendMessages(dst []byte, msgs []chatMessage) []byte {
	dst = append(dst, '[')
	for i, m := range msgs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = m.appendJSON(dst)
	}
	return append(dst, ']')
}

func (m chatMessage) appendJSON(dst []byte) []byte {
	dst = append(dst, `{"role":`...)
	dst = appendQuoted(dst, m.role)
	if m.parts != nil || m.content != nil {
		dst = append(dst, `,"content":`...)
		dst = m.appendContent(dst)
	}
	if len(m.toolCalls) > 0 {
		dst = append(dst, `,"tool_calls":[`...)
		for i, tc := range m.toolCalls {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, `{"id":`...)
			dst = appendQuoted(dst, tc.ID)
			dst = append(dst, `,"type":"function","function":{"name":`...)
			dst = appendQuoted(dst, tc.Function.Name)
			dst = append(dst, `,"arguments":`...)
			dst = appendQuoted(dst, tc.Function.Arguments)
			dst = append(dst, "}}"...)
		}
		dst = append(dst, ']')
	}
	if m.toolCallID != "" {
		dst = append(dst, `,"tool_call_id":`...)
		dst = appendQuoted(dst, m.toolCallID)
	}
	return append(dst, '}')
}

// ------------------------------------------------------------------ responses

// oaiCompletion is the buffered chat completion the inference plane returns.
type oaiCompletion struct {
	Choices []struct {
		Message struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *tokenUsage `json:"usage"`
}

// anthropicMessageOut is a completed Messages response.
type anthropicMessageOut struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []anthropicOut   `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsageIO `json:"usage"`
}

// anthropicOut is one block of a response. `input` is a raw document rather
// than a string, which is the direction the tool-call difference runs.
type anthropicOut struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsageIO struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// encode turns a buffered upstream answer into a Messages response, or an
// upstream error into a Messages error.
func (anthropicShape) encode(raw []byte, alias string, status int) ([]byte, int) {
	if status >= 300 {
		msg := upstreamErrorMessage(raw, status)
		if status == http.StatusBadRequest {
			if overflow := contextOverflow(raw); overflow != "" {
				msg = overflow
			}
		}
		return anthropicErrorBody(status, msg), status
	}
	var in oaiCompletion
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Choices) == 0 {
		// A 2xx that is not a completion. Passing it on would hand the client
		// a malformed response with no explanation.
		return anthropicErrorBody(http.StatusBadGateway, msgUnreadableAnswer), http.StatusBadGateway
	}

	choice := in.Choices[0]
	out := anthropicMessageOut{
		ID: id.New("msg"), Type: "message", Role: "assistant",
		// The alias, not the backend model, so the backend stays swappable.
		Model:      alias,
		Content:    []anthropicOut{},
		StopReason: stopReason(choice.FinishReason),
	}
	if choice.Message.Content != "" {
		out.Content = append(out.Content, anthropicOut{Type: "text", Text: choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Content = append(out.Content, anthropicOut{
			Type: "tool_use", ID: toolUseID(tc.ID), Name: tc.Function.Name,
			Input: toolInput(tc.Function.Arguments),
		})
	}
	if in.Usage != nil {
		out.Usage = anthropicUsageIO{
			InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens,
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return anthropicErrorBody(http.StatusInternalServerError, msgUnencodable),
			http.StatusInternalServerError
	}
	return body, status
}

func (anthropicShape) contentType() string { return "application/json" }

// stopReason maps the OpenAI finish reason onto the Messages one. Clients
// decide whether to continue the turn on it, so an unknown reason reads as a
// finished turn, not a truncated one.
func stopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// toolUseID keeps the upstream's identifier when it has one. Any string works,
// since the client only sends it back, but an empty one would leave the tool
// result with nothing to attach to.
func toolUseID(s string) string { return upstreamID(s, "toolu") }

// upstreamID keeps a tool call's upstream identifier, or makes one with
// prefix when the upstream sent none.
func upstreamID(s, prefix string) string {
	if s == "" {
		return id.New(prefix)
	}
	return s
}

// toolInput turns the arguments string into the document the Messages API
// carries. Models do emit invalid JSON, and an empty object the client can
// report a tool failure against beats a response it cannot parse.
func toolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// --------------------------------------------------------------------- errors

// anthropicErrorType names an error by status, as the Messages API does.
// Clients match on it to decide whether to retry.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

func anthropicErrorBody(status int, msg string) []byte {
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type": anthropicErrorType(status), "message": msg,
		},
	})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"internal error"}}`)
	}
	return body
}

// upstreamErrorMessage pulls the text out of whatever the inference plane
// returned, unchanged. Some clients recover by matching on the wording, so it
// must not be rewritten.
func upstreamErrorMessage(raw []byte, status int) string {
	if msg := upstreamText(raw); msg != "" {
		return msg
	}
	return "the inference plane refused the request with status " +
		strconv.Itoa(status) + " and no message"
}

// upstreamText is the message in an error from the inference plane, whatever
// envelope it came in, or "" when it gave none.
func upstreamText(raw []byte) string {
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		// vLLM answers some refusals with a bare message field, and FastAPI
		// with a detail field.
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		switch {
		case envelope.Error != nil && envelope.Error.Message != "":
			return envelope.Error.Message
		case envelope.Message != "":
			return envelope.Message
		case envelope.Detail != "":
			return envelope.Detail
		}
	}
	if text := strings.TrimSpace(string(raw)); len(text) < 2048 {
		return text
	}
	return ""
}

// A backend's refusal of a request too long for its context, in its own words.
var (
	// vLLM, when the prompt fits but not with max_tokens: "(32000 > 32768 - 18000)".
	vllmOutputTooLong = regexp.MustCompile(`\((\d+) > (\d+) - (\d+)\)`)
	// Older vLLM, either way.
	vllmRequested = regexp.MustCompile(`maximum context length is (\d+) tokens.*` +
		`\((\d+) in the messages, (\d+) in the completion\)`)
	// vLLM, when the prompt alone does not fit.
	vllmPromptTooLong = regexp.MustCompile(`maximum context length is (\d+) tokens\. ` +
		`However, your request has (\d+) input tokens`)
)

// contextOverflow rewrites a backend's refusal of a request too long for its
// context into the Messages API's words, or returns "" for any other error.
//
// Claude Code compacts the conversation when it reads "prompt is too long",
// and lowers max_tokens and tries again when it reads the other sentence.
// vLLM and llama.cpp say neither, and without this the session just stops.
func contextOverflow(raw []byte) string {
	var envelope struct {
		Error struct {
			Type string `json:"type"`
			Code any    `json:"code"`
			// llama.cpp names the numbers.
			PromptTokens int `json:"n_prompt_tokens"`
			Context      int `json:"n_ctx"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	e := envelope.Error
	msg := upstreamText(raw)
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }

	if m := vllmOutputTooLong.FindStringSubmatch(msg); m != nil {
		return overflowMessage(num(m[3]), num(m[1]), num(m[2]))
	}
	if m := vllmRequested.FindStringSubmatch(msg); m != nil {
		return overflowMessage(num(m[2]), num(m[3]), num(m[1]))
	}
	if m := vllmPromptTooLong.FindStringSubmatch(msg); m != nil {
		return overflowMessage(num(m[2]), 0, num(m[1]))
	}
	if e.Type == "exceed_context_size_error" && e.PromptTokens > 0 && e.Context > 0 {
		return overflowMessage(e.PromptTokens, 0, e.Context)
	}
	// Without numbers the words alone still tell the client what to do.
	if e.Type == "exceed_context_size_error" || e.Code == "context_length_exceeded" ||
		strings.Contains(msg, "exceeds the available context size") {
		return "prompt is too long: " + msg
	}
	return ""
}

// overflowMessage says, as the Messages API does, that input tokens and
// maxTokens of output do not fit in window. maxTokens is 0 when the input
// alone is too long.
func overflowMessage(input, maxTokens, window int) string {
	if maxTokens <= 0 || input >= window {
		return fmt.Sprintf("prompt is too long: %d tokens > %d maximum", input, window)
	}
	return fmt.Sprintf("input length and `max_tokens` exceed context limit: %d + %d > %d, "+
		"decrease input length or `max_tokens` and try again", input, maxTokens, window)
}

// writeError renders a refusal the gateway generated itself. The OpenAI type
// and code are dropped: the Messages shape has one type, derived from the
// status, and no field for the others.
func (anthropicShape) writeError(w http.ResponseWriter, status int, _, _, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(anthropicErrorBody(status, msg))
}
