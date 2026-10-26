package gateway

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
)

// Hosted tools run on the provider's own servers: web search, fetching a URL,
// running code, a remote MCP server. They reach the outside world from the
// provider's side, where no guardrail of this gateway sees what went where.
//
// On the deployment's own provider key they are taken out before forwarding,
// as the provider charges for them apart from tokens. Only the tools the
// client runs itself are kept. The list names what is kept, not what is taken
// out, so a hosted tool released next month is taken out too.

// anthropicClientTools and openAIClientTools are the tool types the client
// runs, by type, or by prefix for entries ending in '_'. An Anthropic tool
// with no type is one the client declared, like a function. An OpenAI
// namespace holds only functions and custom tools: Codex puts each MCP server
// and its own subagent tools in one.
var (
	anthropicClientTools = []string{"custom", "bash_", "text_editor_", "computer_", "memory_"}
	openAIClientTools    = []string{
		"function", "custom", "namespace", "local_shell",
		"computer_use_preview", "shell", "apply_patch",
	}
)

// keepsTool reports whether a tool of this type is one the client runs.
func keepsTool(typ string, client []string) bool {
	if typ == "" {
		return true
	}
	return slices.ContainsFunc(client, func(p string) bool {
		return typ == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(typ, p))
	})
}

// stripTools removes the hosted tools from a request's tools array and returns
// their names. A tool_choice that named one goes too, or the provider would
// refuse the request for naming a tool it was not offered.
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
// name a hosted tool to use.
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
// Anthropic to connect to the servers it lists.
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
