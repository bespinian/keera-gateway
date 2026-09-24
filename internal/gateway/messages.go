package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
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
	Type string `json:"type"`
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`

	// image
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// oaiMessage is one message in the OpenAI chat shape.
type oaiMessage struct {
	Role string `json:"role"`
	// Content is a string, an array of parts, or absent. The OpenAI shape
	// needs it absent on an assistant turn of only tool calls.
	Content    any           `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"`
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

// emptySchema stands in for a tool declared without one. vLLM rejects a
// function without parameters, and tools without arguments are real.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// decode turns a Messages request into a chat completion request.
func (anthropicShape) decode(raw []byte) ([]byte, error) {
	var in anthropicRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return nil, errors.New("the '" + typeErr.Field + "' field has the wrong type")
		}
		return nil, errors.New("request body is not a valid Messages request: " + err.Error())
	}
	if in.Model == "" {
		return nil, errors.New("the 'model' field is required")
	}
	if len(in.Messages) == 0 {
		return nil, errors.New("the 'messages' field is required and must not be empty")
	}

	msgs, err := convertMessages(in)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"model": in.Model, "messages": msgs}
	addSampling(out, in)
	if tools := convertTools(in.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if in.ToolChoice != nil {
		addToolChoice(out, *in.ToolChoice)
	}
	return json.Marshal(out)
}

// convertMessages turns the system prompt and every turn into OpenAI messages.
func convertMessages(in anthropicRequest) ([]oaiMessage, error) {
	msgs := make([]oaiMessage, 0, len(in.Messages)+1)
	if sys := systemText(in.System); sys != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: sys})
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

// addSampling copies the output limit and the sampling settings.
func addSampling(out map[string]any, in anthropicRequest) {
	// max_tokens is required by the Messages API, so it is nearly always
	// here, and it is what the guardrail ceiling clamps in serve.
	if in.MaxTokens != nil {
		out["max_tokens"] = *in.MaxTokens
	}
	if in.Stream {
		out["stream"] = true
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.TopK != nil {
		// Not an OpenAI field. vLLM accepts it as an extension, and other
		// backends ignore it.
		out["top_k"] = *in.TopK
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
}

// convertTools declares the tools as OpenAI functions.
func convertTools(in []anthropicTool) []oaiTool {
	tools := make([]oaiTool, 0, len(in))
	for _, t := range in {
		if t.Name == "" {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = emptySchema
		}
		tools = append(tools, oaiTool{
			Type: "function",
			Function: oaiToolDeclaration{
				Name: t.Name, Description: t.Description, Parameters: schema,
			},
		})
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
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	return joinText(blocks)
}

// convertMessage turns one Messages turn into one or more OpenAI messages.
//
// Tool results are why it can be more than one. The Messages API puts them
// in a user turn; the OpenAI shape wants each as its own "tool" message, placed
// before any user text so it still follows the assistant turn that called it.
func convertMessage(role string, content json.RawMessage) ([]oaiMessage, error) {
	if role != "user" && role != "assistant" {
		return nil, errors.New("'role' must be 'user' or 'assistant'")
	}
	if len(content) == 0 {
		return nil, errors.New("'content' is required")
	}

	// The common shape by a wide margin: content is a bare string.
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []oaiMessage{{Role: role, Content: text}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, errors.New("'content' must be a string or an array of content blocks")
	}

	if role == "assistant" {
		return convertAssistant(blocks), nil
	}
	return convertUser(blocks), nil
}

// convertAssistant collapses an assistant turn. Its text becomes the content
// and its tool_use blocks become tool_calls. Thinking blocks have no
// counterpart and are dropped.
func convertAssistant(blocks []anthropicBlock) []oaiMessage {
	msg := oaiMessage{Role: "assistant"}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 && string(b.Input) != "null" {
			args = string(b.Input)
		}
		msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{
			ID: b.ID, Type: "function", Function: oaiFunction{Name: b.Name, Arguments: args},
		})
	}
	if text := joinText(blocks); text != "" {
		msg.Content = text
	}
	// An empty assistant turn gets an empty string. Upstream rejects a
	// message without content, and dropping the turn would break the
	// user/assistant alternation the chat template needs.
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		msg.Content = ""
	}
	return []oaiMessage{msg}
}

// convertUser splits a user turn into the tool results it carries and whatever
// the person actually said.
func convertUser(blocks []anthropicBlock) []oaiMessage {
	var (
		out   []oaiMessage
		parts []oaiPart
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, oaiPart{Type: "text", Text: b.Text})
			}
		case "image":
			if p, ok := imagePart(b); ok {
				parts = append(parts, p)
			}
		case "tool_result":
			text, images := toolResult(b.Content)
			out = append(out, oaiMessage{
				Role: "tool", ToolCallID: b.ToolUseID, Content: text,
			})
			// The OpenAI tool role carries only text, so images from a tool
			// move to the user turn after the results, where the model reads
			// them.
			parts = append(parts, images...)
		}
		// Other block types (thinking, redacted_thinking, document,
		// tool_reference) have no counterpart and are dropped.
	}

	switch {
	case len(parts) == 0:
		// Only tool results, so no empty user turn after them.
	case len(parts) == 1 && parts[0].Type == "text":
		out = append(out, oaiMessage{Role: "user", Content: parts[0].Text})
	default:
		out = append(out, oaiMessage{Role: "user", Content: parts})
	}
	if len(out) == 0 {
		out = append(out, oaiMessage{Role: "user", Content: ""})
	}
	return out
}

// imagePart converts an image block. The Messages API carries the bytes
// inline; the OpenAI shape carries the same bytes as a data URL.
func imagePart(b anthropicBlock) (oaiPart, bool) {
	if b.Source == nil {
		return oaiPart{}, false
	}
	switch b.Source.Type {
	case "url":
		if b.Source.URL == "" {
			return oaiPart{}, false
		}
		return oaiPart{Type: "image_url", ImageURL: &oaiImageURL{URL: b.Source.URL}}, true
	case "base64":
		if b.Source.Data == "" || b.Source.MediaType == "" {
			return oaiPart{}, false
		}
		return oaiPart{Type: "image_url", ImageURL: &oaiImageURL{
			URL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
		}}, true
	}
	return oaiPart{}, false
}

// toolResult flattens a tool result's content, which is a string or an array of
// blocks, and separates out any images it carried.
//
// `is_error` is dropped: the OpenAI shape has no field for it, and a failed
// tool says so in its text anyway.
func toolResult(raw json.RawMessage) (string, []oaiPart) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw), nil
	}
	var images []oaiPart
	for _, b := range blocks {
		if b.Type == "image" {
			if p, ok := imagePart(b); ok {
				images = append(images, p)
			}
		}
	}
	return joinText(blocks), images
}

// joinText joins the text blocks in a list with a blank line. Running them
// together would glue the last word of one to the first of the next.
func joinText(blocks []anthropicBlock) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
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
		return anthropicErrorBody(status, upstreamErrorMessage(raw, status)), status
	}
	var in oaiCompletion
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Choices) == 0 {
		// A 2xx that is not a completion. Passing it on would hand the client
		// a malformed response with no explanation.
		return anthropicErrorBody(http.StatusBadGateway,
				"the inference plane returned a response the gateway could not read"),
			http.StatusBadGateway
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
		return anthropicErrorBody(http.StatusInternalServerError,
			"the gateway could not encode the response"), http.StatusInternalServerError
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
func toolUseID(s string) string {
	if s == "" {
		return id.New("toolu")
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
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		// vLLM answers some refusals with a bare message field.
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
	if text := strings.TrimSpace(string(raw)); text != "" && len(text) < 2048 {
		return text
	}
	return "the inference plane refused the request with status " +
		strconv.Itoa(status) + " and no message"
}

// writeError renders a refusal the gateway generated itself. The OpenAI type
// and code are dropped: the Messages shape has one type, derived from the
// status, and no field for the others.
func (anthropicShape) writeError(w http.ResponseWriter, status int, _, _, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(anthropicErrorBody(status, msg))
}
