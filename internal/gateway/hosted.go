package gateway

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
)

// Hosted tools, such as web search or a remote MCP server, run on the
// provider's servers, out of sight of this gateway's guardrails.
//
// On the deployment's own provider key they are removed, since the provider
// charges for them apart from tokens. The list names the tools that are kept,
// so a new hosted tool is removed too.

// anthropicClientTools and openAIClientTools are the tool types the client
// runs, matched by prefix for entries ending in '_'. An Anthropic tool with no
// type is a client tool. An OpenAI namespace holds only functions and custom
// tools; Codex puts each MCP server in one.
var (
	anthropicClientTools = []string{"custom", "bash_", "text_editor_", "computer_", "memory_"}
	openAIClientTools    = []string{
		"function", "custom", "namespace", "local_shell",
		"computer_use_preview", "shell", "apply_patch",
	}
)

func keepsTool(typ string, client []string) bool {
	if typ == "" {
		return true
	}
	return slices.ContainsFunc(client, func(p string) bool {
		return typ == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(typ, p))
	})
}

// stripTools removes the hosted tools and returns their names. A tool_choice
// naming one goes too, or the provider would refuse the request.
func stripTools(b *body, client []string) []string {
	raw, ok := b.value("tools")
	if !ok {
		return nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	kept := tools[:0:0]
	var removed []string
	for _, t := range tools {
		var tool struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &tool)
		if keepsTool(tool.Type, client) {
			kept = append(kept, t)
			continue
		}
		removed = append(removed, cmp.Or(tool.Name, tool.Type))
	}
	if len(removed) == 0 {
		return nil
	}
	if len(kept) == 0 {
		b.remove("tools")
	} else {
		encoded, _ := json.Marshal(kept)
		b.set("tools", encoded)
	}
	if choice, ok := b.value("tool_choice"); ok {
		var named struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(choice, &named) == nil &&
			(slices.Contains(removed, named.Name) || hostedChoice(named.Type, client)) {
			b.remove("tool_choice")
		}
	}
	return removed
}

// choiceKinds are the tool_choice types that say how to choose, rather than
// name a hosted tool.
var choiceKinds = []string{
	"auto", "any", "none", "required", "tool", "function", "custom",
	"allowed_tools",
}

// hostedChoice reports whether a tool_choice type names a hosted tool, as
// {"type": "web_search_preview"} does on the Responses API.
func hostedChoice(typ string, client []string) bool {
	return typ != "" && !slices.Contains(choiceKinds, typ) && !keepsTool(typ, client)
}

// stripHostedTools on the Messages API also drops mcp_servers, which asks
// Anthropic to connect to those servers.
func (anthropicDialect) stripHostedTools(b *body) []string {
	removed := stripTools(b, anthropicClientTools)
	if b.remove("mcp_servers") {
		removed = append(removed, "mcp_servers")
	}
	return removed
}

func (responsesDialect) stripHostedTools(b *body) []string {
	return stripTools(b, openAIClientTools)
}
