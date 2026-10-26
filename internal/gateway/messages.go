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

// This file is the Anthropic Messages surface: `POST /v1/messages` is turned
// into an OpenAI chat request and the answer turned back. Claude Code and
// several other agents speak only that API.
//
// Fields with no OpenAI place are dropped, since they are only hints.

type anthropicShape struct{}

// anthropicRequest is the part of a Messages request the OpenAI shape has a
// place for. Unknown fields are ignored, so new client fields do not break.
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
// `defer_loading` have no OpenAI counterpart.
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

// anthropicBlock is one content block of any type.
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

	// An object for an image or a document, but a URL string for a
	// search_result.
	Source json.RawMessage `json:"source"`
	// document and search_result
	Title string `json:"title"`
}

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

// decodeBlocks reads content blocks one by one. An unused block type is
// dropped even if it does not fit the struct, so a new type cannot fail the
// request.
func decodeBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	blocks := make([]anthropicBlock, 0, len(items))
	for i, item := range items {
		var b anthropicBlock
		if err := json.Unmarshal(item, &b); err != nil {
			switch typ := typeOf(item); typ {
			case "text", "image", "tool_use", "tool_result":
				return nil, fmt.Errorf("content block %d (%s): %w", i, typ, err)
			}
			continue
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// blockText is the text a block gives the model: a text block, a plain-text
// document, or a search result's title, URL and text.
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
	// Arguments is JSON in a string, not an object: the one way the two APIs'
	// tool calls differ.
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

// decodeRequest reads a request body into in. api names the API in the error.
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

// emptySchema stands in for a missing tool schema, which vLLM rejects.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// decodeBody turns a Messages request into a chat completion request.
func (sh anthropicShape) decodeBody(raw []byte) (*body, shape, error) {
	// A missing model is left to serve, which refuses it the same on every API.
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
	addSampling(out, in.MaxTokens, in.Stream, in.Temperature, in.TopP)
	if in.TopK != nil {
		// Not an OpenAI field, but vLLM accepts it and others ignore it.
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

// chatBody builds a chat completion request. size is the original request's
// size, used to size the messages buffer.
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

// addSampling copies the settings both native APIs share with chat
// completions.
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
// web search, run on Anthropic's servers, so they are dropped.
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

// systemText flattens the system prompt, a string or an array of text blocks.
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
// Each tool result in a user turn becomes its own "tool" message, placed
// before the user text so it follows the assistant turn that called it.
func convertMessage(role string, content json.RawMessage) ([]chatMessage, error) {
	// Claude Code may send a system turn mid-conversation (a beta). It stays a
	// system message in its place.
	if role != "user" && role != "assistant" && role != "system" {
		return nil, errors.New("'role' must be 'user', 'assistant' or 'system'")
	}
	if len(content) == 0 {
		return nil, errors.New("'content' is required")
	}

	if text, err := textOf(content); err == nil {
		return []chatMessage{{role: role, content: text}}, nil
	}
	if trimmed := bytes.TrimSpace(content); len(trimmed) == 0 || trimmed[0] != '[' {
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

// convertAssistant collapses an assistant turn into content and tool_calls.
// Thinking blocks are dropped.
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
	// Upstream rejects a message without content, and dropping the turn would
	// break the user/assistant alternation.
	if msg.content == nil && len(msg.toolCalls) == 0 {
		msg.content = emptyText
	}
	return msg
}

// convertUser splits a user turn into its tool results and the user's own
// content.
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
			// The OpenAI tool role carries only text, so images move to the
			// user turn after the results.
			parts = append(parts, images...)
		}
		// Other blocks, such as thinking or a PDF document, are dropped.
	}

	switch {
	case len(parts) == 0:
		// Only tool results: no empty user turn after them.
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

// imagePart converts an image block, with inline bytes as a data URL.
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

// dataURL copies the base64 as it came, since an image can be megabytes.
func dataURL(mediaType string, data jsonText) jsonText {
	media := quoteText(mediaType)
	out := make(jsonText, 0, len(media)+len(data)+16)
	out = append(out, `"data:`...)
	out = append(out, media[1:len(media)-1]...)
	out = append(out, ";base64,"...)
	out = append(out, data[1:len(data)-1]...)
	return append(out, '"')
}

// toolResult flattens a tool result's content and separates out its images.
// `is_error` is dropped, since a failed tool says so in its text anyway.
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

// joinText joins the blocks' text with a blank line, so words do not glue.
func joinText(blocks []anthropicBlock) jsonText {
	texts := make([]jsonText, 0, len(blocks))
	for _, b := range blocks {
		texts = append(texts, blockText(b))
	}
	return joinTexts(texts)
}

// chatMessage is one message of a translated chat request. It is written out
// by hand, so its text is copied rather than escaped again.
type chatMessage struct {
	role string
	// content is left out when nil, as for a turn of only tool calls.
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

// openingOfChat is the first user message in msgs, written as appendMessages
// does, so a request keeps its session whether or not it is translated.
func openingOfChat(msgs []chatMessage) (json.RawMessage, bool) {
	for _, m := range msgs {
		if m.role == "user" {
			return m.appendContent(nil), true
		}
	}
	return nil, false
}

// openingIn is the first user message in a native API's messages, converted
// by convert. Messages it cannot read are skipped.
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

type oaiCompletion struct {
	Choices []oaiChoice `json:"choices"`
	Usage   *tokenUsage `json:"usage"`
}

type oaiChoice struct {
	Message struct {
		Content   string        `json:"content"`
		ToolCalls []oaiToolCall `json:"tool_calls"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

// firstChoice reads a chat completion's first choice and usage. It is false
// for a 2xx that is not a completion.
func firstChoice(raw []byte) (oaiChoice, *tokenUsage, bool) {
	var in oaiCompletion
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Choices) == 0 {
		return oaiChoice{}, nil, false
	}
	return in.Choices[0], in.Usage, true
}

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

// encode turns a buffered upstream answer or error into its Messages form.
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
	choice, usage, ok := firstChoice(raw)
	if !ok {
		return anthropicErrorBody(http.StatusBadGateway, msgUnreadableAnswer), http.StatusBadGateway
	}
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
	if usage != nil {
		out.Usage = anthropicUsageIO{
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
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

// stopReason maps the OpenAI finish reason to the Messages one. An unknown
// reason reads as a finished turn, not a truncated one.
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

// toolUseID keeps the upstream's id, or makes one, since an empty id would
// leave the tool result nothing to attach to.
func toolUseID(s string) string { return upstreamID(s, "toolu") }

func upstreamID(s, prefix string) string {
	if s == "" {
		return id.New(prefix)
	}
	return s
}

// toolInput turns the arguments string into a JSON value. Models do emit
// invalid JSON, which becomes an empty object the client can parse.
func toolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// anthropicErrorType names an error by status, as the Messages API does.
// Clients use it to decide whether to retry.
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

// upstreamErrorMessage returns the upstream's error text unchanged, since some
// clients recover by matching on the wording.
func upstreamErrorMessage(raw []byte, status int) string {
	if msg := upstreamText(raw); msg != "" {
		return msg
	}
	return "the inference plane refused the request with status " +
		strconv.Itoa(status) + " and no message"
}

// upstreamText is the message in an upstream error, whatever its envelope, or
// "".
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

// How backends refuse a request too long for their context.
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

// contextOverflow rewrites a backend's context overflow error in the Messages
// API's words, or returns "". Claude Code compacts or lowers max_tokens when
// it reads them; without this, the session just stops.
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
	// Without numbers, the words still tell the client what to do.
	if e.Type == "exceed_context_size_error" || e.Code == "context_length_exceeded" ||
		strings.Contains(msg, "exceeds the available context size") {
		return "prompt is too long: " + msg
	}
	return ""
}

// overflowMessage says, as the Messages API does, that input and maxTokens do
// not fit in window. maxTokens is 0 when the input alone is too long.
func overflowMessage(input, maxTokens, window int) string {
	if maxTokens <= 0 || input >= window {
		return fmt.Sprintf("prompt is too long: %d tokens > %d maximum", input, window)
	}
	return fmt.Sprintf("input length and `max_tokens` exceed context limit: %d + %d > %d, "+
		"decrease input length or `max_tokens` and try again", input, maxTokens, window)
}

// writeError renders the gateway's own refusal. The OpenAI type and code are
// dropped, since the Messages type is derived from the status.
func (anthropicShape) writeError(w http.ResponseWriter, status int, _, _, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(anthropicErrorBody(status, msg))
}
