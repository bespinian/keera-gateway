package gateway

import (
	"encoding/base64"
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

// responsesShape implements shape for `/v1/responses`. tools is what the
// request declared under another name or kind than the chat shape knows it
// by, so the answer names each call the way the client declared it.
type responsesShape struct {
	openAIErrors
	tools toolNames
}

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
	// Format is what a custom tool takes: free text, or text that matches a
	// grammar.
	Format *struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	} `json:"format"`
	// Tools are a namespace's own: functions and custom tools.
	Tools []responsesTool `json:"tools"`
}

// responsesItem is one item of the input. Items are told apart by `type`, and
// a message may leave it out.
type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// function_call and custom_tool_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`

	// function_call_output and custom_tool_call_output
	Output json.RawMessage `json:"output"`

	// local_shell_call: the command, kept as the call's arguments
	Action json.RawMessage `json:"action"`
}

type responsesPart struct {
	Type     string   `json:"type"`
	Text     jsonText `json:"text"`
	Refusal  jsonText `json:"refusal"`
	ImageURL jsonText `json:"image_url"`

	// input_file, sent inline as a data URL
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

// decodeBody turns a Responses request into a chat completion request, and
// returns the shape that answers it in the terms of the tools it declared.
func (responsesShape) decodeBody(raw []byte) (*body, shape, error) {
	var in responsesRequest
	if err := decodeRequest(raw, &in, "Responses"); err != nil {
		return nil, nil, err
	}
	msgs, err := responsesMessages(in)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{}
	addSampling(out, in.MaxOutputTokens, in.Stream, in.Temperature, in.TopP)
	tools, names := responsesTools(in.Tools)
	if len(tools) > 0 {
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
	b, err := chatBody(in.Model, msgs, out, len(raw))
	return b, responsesShape{tools: names}, err
}

func responsesMessages(in responsesRequest) ([]chatMessage, error) {
	var msgs []chatMessage
	if in.Instructions != nil && *in.Instructions != "" {
		msgs = append(msgs, chatMessage{role: "system", content: quoteText(*in.Instructions)})
	}
	if len(in.Input) == 0 || string(in.Input) == "null" {
		return nil, errors.New("the 'input' field is required")
	}
	if text, err := textOf(in.Input); err == nil {
		return append(msgs, chatMessage{role: "user", content: text}), nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(in.Input, &items); err != nil {
		return nil, errors.New("the 'input' field must be a string or an array of items")
	}
	// The chat shape's tool role carries only text, so images a tool returned
	// wait for the end of the tool results and follow them as a user turn.
	// Put between two results, they would part the second from its call.
	var images []chatPart
	for i, raw := range items {
		var it responsesItem
		if err := json.Unmarshal(raw, &it); err != nil {
			// An item the translation does not use is dropped, even when its
			// fields do not fit the ones it reads.
			if !translatedItem(typeOf(raw)) {
				continue
			}
			return nil, errors.New("input[" + strconv.Itoa(i) + "]: " + err.Error())
		}
		if !isToolOutput(it.Type) && len(images) > 0 {
			msgs = append(msgs, chatMessage{role: "user", parts: images})
			images = nil
		}
		var (
			err  error
			more []chatPart
		)
		if msgs, more, err = addItem(msgs, it); err != nil {
			return nil, errors.New("input[" + strconv.Itoa(i) + "]: " + err.Error())
		}
		images = append(images, more...)
	}
	if len(images) > 0 {
		msgs = append(msgs, chatMessage{role: "user", parts: images})
	}
	return msgs, nil
}

func translatedItem(typ string) bool {
	switch typ {
	case "", "message", "function_call", "custom_tool_call", "local_shell_call":
		return true
	}
	return isToolOutput(typ)
}

func isToolOutput(typ string) bool {
	switch typ {
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return true
	}
	return false
}

// addItem appends one item, and returns the images a tool's output held.
func addItem(msgs []chatMessage, it responsesItem) ([]chatMessage, []chatPart, error) {
	switch it.Type {
	case "", "message":
		msgs, err := addMessageItem(msgs, it)
		return msgs, nil, err
	case "function_call":
		args := it.Arguments
		if args == "" {
			args = "{}"
		}
		return addCall(msgs, it, args), nil, nil
	case "custom_tool_call":
		args, err := json.Marshal(map[string]string{"input": it.Input})
		if err != nil {
			return nil, nil, err
		}
		return addCall(msgs, it, string(args)), nil, nil
	case "local_shell_call":
		// The tool itself runs only at OpenAI, but the command and its output
		// stay in the conversation, so the model knows what already ran.
		it.Name = "local_shell"
		args := "{}"
		if len(it.Action) > 0 && it.Action[0] == '{' {
			args = string(it.Action)
		}
		return addCall(msgs, it, args), nil, nil
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		text, parts := responsesContent(it.Output)
		var images []chatPart
		for _, p := range parts {
			if p.imageURL != nil {
				images = append(images, p)
			}
		}
		return append(msgs, chatMessage{role: "tool", toolCallID: it.CallID, content: text}), images, nil
	}
	// Reasoning items, item references and the calls of built-in tools have no
	// counterpart, and the model answers without them.
	return msgs, nil, nil
}

// addCall appends a call the model made, under the name the chat shape knows
// its tool by.
func addCall(msgs []chatMessage, it responsesItem, args string) []chatMessage {
	call := oaiToolCall{
		ID: it.CallID, Type: "function",
		Function: oaiFunction{Name: chatToolName(it.Namespace, it.Name), Arguments: args},
	}
	// Calls made in one turn arrive as separate items. The chat shape wants
	// them on one assistant message, with whatever text came before them.
	if n := len(msgs); n > 0 && msgs[n-1].role == "assistant" {
		msgs[n-1].toolCalls = append(msgs[n-1].toolCalls, call)
		if msgs[n-1].content.isEmpty() {
			msgs[n-1].content = nil
		}
		return msgs
	}
	return append(msgs, chatMessage{role: "assistant", toolCalls: []oaiToolCall{call}})
}

// addMessageItem appends a message. The developer role is the system role
// under the name OpenAI now uses for it; self-hosted chat templates know only
// the old one.
func addMessageItem(msgs []chatMessage, it responsesItem) ([]chatMessage, error) {
	role := it.Role
	switch role {
	case "developer":
		role = "system"
	case "user", "assistant", "system":
	default:
		return nil, errors.New("'role' must be 'user', 'assistant', 'system' or 'developer'")
	}
	text, parts := responsesContent(it.Content)
	msg := chatMessage{role: role, content: text}
	// Only a user can send an image; everyone else's content is text.
	if role == "user" && len(parts) > 0 {
		msg.parts = parts
	}
	return append(msgs, msg), nil
}

// responsesContent reads content that is a string or an array of parts. It
// returns the text joined, and the parts in the chat shape when any of them
// is an image.
func responsesContent(raw json.RawMessage) (jsonText, []chatPart) {
	if len(raw) == 0 {
		return emptyText, nil
	}
	if text, err := textOf(raw); err == nil {
		return text, nil
	}
	var parts []responsesPart
	if json.Unmarshal(raw, &parts) != nil {
		return quoteText(string(raw)), nil
	}
	var (
		texts   []jsonText
		out     []chatPart
		picture bool
	)
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, p.Text)
			out = append(out, chatPart{text: orEmpty(p.Text)})
		case "refusal":
			texts = append(texts, p.Refusal)
			out = append(out, chatPart{text: orEmpty(p.Refusal)})
		case "input_file":
			if text := fileText(p); text != nil {
				texts = append(texts, text)
				out = append(out, chatPart{text: text})
			}
		case "input_image":
			// An image named by file id lives on OpenAI's servers, and no
			// other model can fetch it.
			if !p.ImageURL.isEmpty() {
				out = append(out, chatPart{imageURL: p.ImageURL})
				picture = true
			}
		}
	}
	if !picture {
		out = nil
	}
	return orEmpty(joinTexts(texts)), out
}

// fileText is the contents of a text file sent inline, under its name. A file
// of any other kind, or one named by id, is nil: only OpenAI can read those.
func fileText(p responsesPart) jsonText {
	media, data, ok := strings.Cut(strings.TrimPrefix(p.FileData, "data:"), ",")
	if !ok || !strings.HasPrefix(p.FileData, "data:") {
		return nil
	}
	media, base64Encoded := strings.CutSuffix(media, ";base64")
	media, _, _ = strings.Cut(media, ";")
	if !strings.HasPrefix(media, "text/") && media != "application/json" {
		return nil
	}
	content := []byte(data)
	if base64Encoded {
		var err error
		if content, err = base64.StdEncoding.DecodeString(data); err != nil {
			return nil
		}
	}
	return joinTexts([]jsonText{quoteText(p.Filename), quoteText(string(content))})
}

// orEmpty is t, or empty text in place of none, for a field the chat shape
// needs.
func orEmpty(t jsonText) jsonText {
	if t == nil {
		return emptyText
	}
	return t
}

// declaredTool is how the client declared a tool that the chat shape knows by
// another name or as another kind: inside a namespace, or as a custom tool,
// which takes text instead of JSON arguments.
type declaredTool struct {
	namespace, name string
	custom          bool
}

// toolNames maps a tool's name in the chat shape to how it was declared, for
// the tools that are not plain functions.
type toolNames map[string]declaredTool

// responsesTools declares the client's tools in the chat shape, which has
// neither namespaces nor custom tools. A namespace's tools get names that carry
// the namespace, and a custom tool becomes a function of its text. Codex uses
// namespaces for MCP servers and subagents, and declares apply_patch as custom.
// Built-in tools run only on OpenAI's servers, so they are dropped.
func responsesTools(in []responsesTool) ([]oaiTool, toolNames) {
	tools := make([]oaiTool, 0, len(in))
	names := toolNames{}
	// A name declared twice would be refused by some backends, and its calls
	// could not be told apart. The first one keeps it.
	taken := map[string]bool{}
	add := func(namespace string, t responsesTool) {
		name := chatToolName(namespace, t.Name)
		if t.Name == "" || taken[name] {
			return
		}
		switch t.Type {
		case "function":
			tools = append(tools, functionTool(name, t.Description, t.Parameters))
		case "custom":
			tools = append(tools, customTool(name, t))
		default:
			return
		}
		taken[name] = true
		if namespace != "" || t.Type == "custom" {
			names[name] = declaredTool{namespace: namespace, name: t.Name, custom: t.Type == "custom"}
		}
	}
	for _, t := range in {
		if t.Type != "namespace" {
			add("", t)
			continue
		}
		for _, inner := range t.Tools {
			add(t.Name, inner)
		}
	}
	return tools, names
}

// chatToolName is the name a tool in a namespace goes by in the chat shape:
// both names, joined by two underscores. Codex names its MCP namespaces
// "mcp__server__", so a tool there becomes "mcp__server__tool".
func chatToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return strings.TrimRight(namespace, "_") + "__" + strings.TrimLeft(name, "_")
}

// customSchema is a custom tool's parameters in the chat shape: its text.
var customSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},` +
	`"required":["input"],"additionalProperties":false}`)

// customTool declares a custom tool as a function of its text. A grammar the
// text has to match goes in the description, since the chat shape has no
// place for one and the model writes better input for seeing it.
func customTool(name string, t responsesTool) oaiTool {
	description := t.Description
	if f := t.Format; f != nil && f.Type == "grammar" && f.Definition != "" {
		description += "\n\nThe input must match this " + f.Syntax + " grammar:\n" + f.Definition
	}
	return functionTool(name, strings.TrimSpace(description), customSchema)
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
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if json.Unmarshal(raw, &named) != nil || named.Name == "" ||
		(named.Type != "function" && named.Type != "custom") {
		return nil, false
	}
	return map[string]any{"type": "function", "function": map[string]string{
		"name": chatToolName(named.Namespace, named.Name),
	}}, true
}

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

// responsesUsage is the usage record of the Responses API. Cached and written
// tokens are part of the input, as in the chat shape.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
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
	out.InputTokensDetails.CacheWriteTokens = u.InputDetails.CacheWriteTokens
	return out
}

func (u *responsesUsage) tokens() *tokenUsage {
	t := &tokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	t.InputDetails.CachedTokens = u.InputTokensDetails.CachedTokens
	t.InputDetails.CacheWriteTokens = u.InputTokensDetails.CacheWriteTokens
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

// call is the output item for a call the model made, named as the client
// declared its tool: in its namespace, and as a custom tool call with its
// text if it is one.
func (n toolNames) call(status, callID, name, args string) map[string]any {
	d, ok := n[name]
	if !ok {
		return callItem(id.New("fc"), status, callID, name, args)
	}
	var item map[string]any
	if d.custom {
		item = map[string]any{
			"type": "custom_tool_call", "id": id.New("ctc"), "status": status,
			"call_id": callID, "name": d.name, "input": customInput(args),
		}
	} else {
		item = callItem(id.New("fc"), status, callID, d.name, args)
	}
	if d.namespace != "" {
		item["namespace"] = d.namespace
	}
	return item
}

// customInput is the text a custom tool was called with, which the chat shape
// carries as the call's one argument. A model that wrote the text bare is
// taken at its word.
func customInput(args string) string {
	var a struct {
		Input *string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &a) == nil && a.Input != nil {
		return *a.Input
	}
	return args
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
func callID(s string) string { return upstreamID(s, "call") }

// encode turns a buffered chat completion into a response. Errors use the
// chat envelope, which this API shares.
func (s responsesShape) encode(raw []byte, alias string, status int) ([]byte, int) {
	if status >= 300 {
		return openAIErrorBody(raw, status), status
	}
	choice, usage, ok := firstChoice(raw)
	if !ok {
		return httpx.ErrorBody("server_error", "", msgUnreadableAnswer), http.StatusBadGateway
	}
	out := responsesOut{
		ID: id.New("resp"), Object: "response", CreatedAt: time.Now().Unix(),
		// The alias, not the backend model, so the backend stays swappable.
		Model: alias, Output: []any{}, Usage: newResponsesUsage(usage),
	}
	out.Status, out.IncompleteDetails = responseStatus(choice.FinishReason)
	if choice.Message.Content != "" {
		out.Output = append(out.Output, messageItem(id.New("msg"), "completed", choice.Message.Content))
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Output = append(out.Output, s.tools.call("completed",
			callID(tc.ID), tc.Function.Name, tc.Function.Arguments))
	}
	body, err := json.Marshal(out)
	if err != nil {
		return httpx.ErrorBody("server_error", "", msgUnencodable), http.StatusInternalServerError
	}
	return body, status
}

func (responsesShape) contentType() string { return "application/json" }
