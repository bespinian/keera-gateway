package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
)

// This file is the OpenAI Responses surface: `POST /v1/responses` in, the chat
// shape the inference plane speaks out, and back again. Codex and the newer
// OpenAI SDKs speak it. A request to one of OpenAI's own models is forwarded
// as it is instead; see native.go.
//
// As on the Messages surface, fields with no place in the chat shape are
// dropped on the way in: reasoning items, built-in tools such as web search,
// and settings that only OpenAI's servers act on.

// responsesShape implements shape for `/v1/responses`.
type responsesShape struct{}

// ------------------------------------------------------------------- requests

// responsesRequest is the part of a Responses request the chat shape has a
// place for.
type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      *string         `json:"instructions"`
	MaxOutputTokens   *int            `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Tools             []responsesTool `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Stream            bool            `json:"stream"`
	Text              *struct {
		Format *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict"`
		} `json:"format"`
	} `json:"text"`
}

// responsesTool is one declared tool. Unlike the chat shape, a function's
// fields sit on the tool itself.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesItem is one item of the input. Items are told apart by `type`, and
// a message may leave it out.
type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// function_call_output
	Output json.RawMessage `json:"output"`
}

// responsesPart is one part of a message's content.
type responsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
}

// decode turns a Responses request into a chat completion request.
func (responsesShape) decode(raw []byte) ([]byte, error) {
	var in responsesRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return nil, errors.New("the '" + typeErr.Field + "' field has the wrong type")
		}
		return nil, errors.New("request body is not a valid Responses request: " + err.Error())
	}
	if in.Model == "" {
		return nil, errors.New("the 'model' field is required")
	}
	msgs, err := responsesMessages(in)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"model": in.Model, "messages": msgs}
	if in.MaxOutputTokens != nil {
		out["max_tokens"] = *in.MaxOutputTokens
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
	if tools := responsesTools(in.Tools); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := responsesToolChoice(in.ToolChoice); ok {
			out["tool_choice"] = tc
		}
		if in.ParallelToolCalls != nil {
			out["parallel_tool_calls"] = *in.ParallelToolCalls
		}
	}
	if in.Text != nil && in.Text.Format != nil {
		switch f := in.Text.Format; f.Type {
		case "json_object":
			out["response_format"] = map[string]string{"type": "json_object"}
		case "json_schema":
			schema := map[string]any{"name": f.Name, "schema": f.Schema}
			if f.Strict != nil {
				schema["strict"] = *f.Strict
			}
			out["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		}
	}
	return json.Marshal(out)
}

// responsesMessages turns the instructions and the input into chat messages.
func responsesMessages(in responsesRequest) ([]oaiMessage, error) {
	var msgs []oaiMessage
	if in.Instructions != nil && *in.Instructions != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: *in.Instructions})
	}
	if len(in.Input) == 0 || string(in.Input) == "null" {
		return nil, errors.New("the 'input' field is required")
	}
	var text string
	if json.Unmarshal(in.Input, &text) == nil {
		return append(msgs, oaiMessage{Role: "user", Content: text}), nil
	}
	var items []responsesItem
	if err := json.Unmarshal(in.Input, &items); err != nil {
		return nil, errors.New("the 'input' field must be a string or an array of items")
	}
	for i, it := range items {
		var err error
		if msgs, err = addItem(msgs, it); err != nil {
			return nil, errors.New("input[" + strconv.Itoa(i) + "]: " + err.Error())
		}
	}
	return msgs, nil
}

// addItem appends one input item to the conversation.
func addItem(msgs []oaiMessage, it responsesItem) ([]oaiMessage, error) {
	switch it.Type {
	case "", "message":
		return addMessageItem(msgs, it)
	case "function_call":
		call := oaiToolCall{
			ID: it.CallID, Type: "function",
			Function: oaiFunction{Name: it.Name, Arguments: it.Arguments},
		}
		if call.Function.Arguments == "" {
			call.Function.Arguments = "{}"
		}
		// Calls made in one turn arrive as separate items. The chat shape wants
		// them on one assistant message, with whatever text came before them.
		if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
			msgs[n-1].ToolCalls = append(msgs[n-1].ToolCalls, call)
			if s, ok := msgs[n-1].Content.(string); ok && s == "" {
				msgs[n-1].Content = nil
			}
			return msgs, nil
		}
		return append(msgs, oaiMessage{Role: "assistant", ToolCalls: []oaiToolCall{call}}), nil
	case "function_call_output":
		text, _ := responsesContent(it.Output)
		return append(msgs, oaiMessage{Role: "tool", ToolCallID: it.CallID, Content: text}), nil
	}
	// Reasoning items, item references and the calls of built-in tools have no
	// counterpart, and the model answers without them.
	return msgs, nil
}

// addMessageItem appends a message. The developer role is the system role
// under the name OpenAI now uses for it; self-hosted chat templates know only
// the old one.
func addMessageItem(msgs []oaiMessage, it responsesItem) ([]oaiMessage, error) {
	role := it.Role
	switch role {
	case "developer":
		role = "system"
	case "user", "assistant", "system":
	default:
		return nil, errors.New("'role' must be 'user', 'assistant', 'system' or 'developer'")
	}
	text, parts := responsesContent(it.Content)
	msg := oaiMessage{Role: role, Content: text}
	// Only a user can send an image; everyone else's content is text.
	if role == "user" && len(parts) > 0 {
		msg.Content = parts
	}
	return append(msgs, msg), nil
}

// responsesContent reads content that is a string or an array of parts. It
// returns the text joined, and the parts in the chat shape when any of them
// is an image.
func responsesContent(raw json.RawMessage) (string, []oaiPart) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []responsesPart
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw), nil
	}
	var (
		texts   []string
		out     []oaiPart
		picture bool
	)
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, p.Text)
			out = append(out, oaiPart{Type: "text", Text: p.Text})
		case "refusal":
			texts = append(texts, p.Refusal)
			out = append(out, oaiPart{Type: "text", Text: p.Refusal})
		case "input_image":
			// An image named by file id lives on OpenAI's servers, and no
			// other model can fetch it.
			if p.ImageURL != "" {
				out = append(out, oaiPart{Type: "image_url", ImageURL: &oaiImageURL{URL: p.ImageURL}})
				picture = true
			}
		}
	}
	if !picture {
		out = nil
	}
	return strings.Join(texts, "\n\n"), out
}

// responsesTools declares the function tools in the chat shape. Built-in tools
// run on OpenAI's servers, so no other model has them.
func responsesTools(in []responsesTool) []oaiTool {
	tools := make([]oaiTool, 0, len(in))
	for _, t := range in {
		if t.Type != "function" || t.Name == "" {
			continue
		}
		schema := t.Parameters
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

// responsesToolChoice maps the tool choice onto its chat spelling. The strings
// are the same in both; a named function is nested one level deeper.
func responsesToolChoice(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s == "auto" || s == "none" || s == "required"
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &named) != nil || named.Type != "function" || named.Name == "" {
		return nil, false
	}
	return map[string]any{"type": "function", "function": map[string]string{"name": named.Name}}, true
}

// ------------------------------------------------------------------ responses

// responsesOut is a completed response.
type responsesOut struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	CreatedAt         int64           `json:"created_at"`
	Status            string          `json:"status"`
	Error             any             `json:"error"`
	IncompleteDetails any             `json:"incomplete_details"`
	Model             string          `json:"model"`
	Output            []any           `json:"output"`
	Usage             *responsesUsage `json:"usage"`
}

// responsesUsage is the usage record of the Responses API. Cached tokens are
// part of the input, as in the chat shape.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

func newResponsesUsage(u *tokenUsage) *responsesUsage {
	if u == nil {
		return nil
	}
	out := &responsesUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		TotalTokens: u.InputTokens + u.OutputTokens}
	out.InputTokensDetails.CachedTokens = u.cached()
	return out
}

func (u *responsesUsage) tokens() *tokenUsage {
	t := &tokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		TotalTokens: u.InputTokens + u.OutputTokens}
	t.InputDetails.CachedTokens = u.InputTokensDetails.CachedTokens
	return t
}

// messageItem and callItem are the two output items a chat completion can
// become.
func messageItem(itemID, status, text string) map[string]any {
	return map[string]any{
		"type": "message", "id": itemID, "status": status, "role": "assistant",
		"content": []any{outputText(text)},
	}
}

func outputText(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}

func callItem(itemID, status, callID, name, args string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": itemID, "status": status,
		"call_id": callID, "name": name, "arguments": args,
	}
}

// responseStatus maps the chat finish reason onto a response's status and
// the reason it stopped short, if it did.
func responseStatus(finish string) (string, any) {
	switch finish {
	case "length":
		return "incomplete", map[string]string{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]string{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

// callID keeps the upstream's identifier when it has one, since the client
// sends it back with the tool's output.
func callID(s string) string {
	if s == "" {
		return id.New("call")
	}
	return s
}

// encode turns a buffered chat completion into a response. Errors are already
// in the shape this API uses, which is the chat one.
func (responsesShape) encode(raw []byte, alias string, status int) ([]byte, int) {
	if status >= 300 {
		return raw, status
	}
	var in oaiCompletion
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Choices) == 0 {
		return responsesErrorBody("the inference plane returned a response the gateway could not read"),
			http.StatusBadGateway
	}
	choice := in.Choices[0]
	out := responsesOut{
		ID: id.New("resp"), Object: "response", CreatedAt: time.Now().Unix(),
		// The alias, not the backend model, so the backend stays swappable.
		Model: alias, Output: []any{}, Usage: newResponsesUsage(in.Usage),
	}
	out.Status, out.IncompleteDetails = responseStatus(choice.FinishReason)
	if choice.Message.Content != "" {
		out.Output = append(out.Output, messageItem(id.New("msg"), "completed", choice.Message.Content))
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Output = append(out.Output, callItem(id.New("fc"), "completed",
			callID(tc.ID), tc.Function.Name, tc.Function.Arguments))
	}
	body, err := json.Marshal(out)
	if err != nil {
		return responsesErrorBody("the gateway could not encode the response"), http.StatusInternalServerError
	}
	return body, status
}

func responsesErrorBody(msg string) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": "server_error", "code": nil,
	}})
	return body
}

func (responsesShape) contentType() string { return "application/json" }

// writeError renders a refusal in the chat shape's envelope, which the
// Responses API shares.
func (responsesShape) writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	httpx.WriteError(w, status, typ, code, msg)
}

// ---------------------------------------------------------------- native

// responsesDialect implements dialect for `/v1/responses`, forwarded to
// OpenAI as it is.
type responsesDialect struct{}

func (responsesDialect) provider() string { return "openai" }

func (responsesDialect) path() string { return "/responses" }

// text is the instructions, and the text of every message and tool output in
// the input. Function call arguments are not text, as on the chat surface.
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
	for _, item := range objects(input) {
		field := "content"
		if item["type"] == "function_call_output" {
			field = "output"
		}
		d.addField(item, field)
		for _, part := range objects(item[field]) {
			switch part["type"] {
			case "input_text", "output_text", "text":
				d.addField(part, "text")
			}
		}
	}
	return d, nil
}

// addSystem puts the guardrail's prompt ahead of the client's instructions.
func (responsesDialect) addSystem(b *body, prompt string) error {
	if s, ok := b.str("instructions"); ok && s != "" {
		prompt += "\n\n" + s
	} else if _, present := b.value("instructions"); present {
		return errors.New("the 'instructions' field must be a string; a guardrail on this key " +
			"adds a system prompt to every request")
	}
	b.setString("instructions", prompt)
	return nil
}

func (responsesDialect) clamp(b *body, limit int) {
	clampFields(b, limit, "max_output_tokens")
}

// needsNative names the fields that continue a conversation OpenAI stored.
// The gateway stores none, so no other model can read it.
func (responsesDialect) needsNative(b *body) string {
	for _, field := range []string{"previous_response_id", "conversation"} {
		if raw, ok := b.value(field); ok && !bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) {
			return "the '" + field + "' field continues a conversation OpenAI stored, and only " +
				"OpenAI's own models can read it; send the whole conversation in 'input' instead"
		}
	}
	return ""
}

// auth is the bearer token every OpenAI endpoint takes.
func (responsesDialect) auth(http.Header) func(http.Header, string) { return nil }

func (responsesDialect) usage(raw []byte) *tokenUsage {
	var doc struct {
		Usage *responsesUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Usage == nil {
		return nil
	}
	return doc.Usage.tokens()
}

func (responsesDialect) rename(raw []byte, alias string) []byte {
	return renameIn(raw, "", alias)
}

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
}

var (
	deltaType   = []byte(`.delta"`)
	responseKey = []byte(`"response":`)
)

func (s *responsesNativeStream) event(payload []byte) []byte {
	if bytes.Contains(payload, deltaType) {
		s.deltas++
		return nil
	}
	if !bytes.Contains(payload, responseKey) {
		return nil
	}
	var ev struct {
		Response struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &ev) == nil && ev.Response.Usage != nil {
		s.usage = ev.Response.Usage.tokens()
	}
	return renameIn(payload, "response", s.alias)
}

func (s *responsesNativeStream) counts() (*tokenUsage, int, bool) {
	return s.usage, s.deltas, false
}
