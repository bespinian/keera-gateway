package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// Hosted tools are the tools a provider runs on its own servers: web search,
// fetching a URL, running code, connecting to a remote MCP server. The model
// asks for one and the provider carries it out, so the request reaches the
// outside world from the provider's side, where no guardrail of this gateway
// sees what went where.
//
// A guardrail with block_hosted_tools takes them out of a request before it
// is forwarded. The model answers without them, as it would if the client had
// never offered them. The ones kept are the tools the client runs itself.
//
// What is kept is a list, not what is taken out: a hosted tool released next
// month is one this build has never heard of, and it is taken out too.

// clientTools are the tool types the client runs, by prefix. An Anthropic
// tool with no type is one the client declared, like a function.
var (
	anthropicClientTools = []string{"custom", "bash_", "text_editor_", "computer_", "memory_"}
	openAIClientTools    = []string{"function", "custom", "local_shell", "computer_use_preview",
		"shell", "apply_patch"}
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
		removed = append(removed, cmpOr(tool.Name, tool.Type))
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
var choiceKinds = []string{"auto", "any", "none", "required", "tool", "function", "custom",
	"allowed_tools"}

// hostedChoice reports whether a tool_choice type names a hosted tool, as
// {"type": "web_search_preview"} does on the Responses API.
func hostedChoice(typ string, client []string) bool {
	return typ != "" && !slices.Contains(choiceKinds, typ) && !keepsTool(typ, client)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// removedToolsHeader names what was taken out of a request, so a developer
// whose agent stopped searching the web can find out why.
func removedToolsHeader(w http.ResponseWriter, removed []string) {
	if len(removed) > 0 {
		w.Header().Set("X-Keera-Removed-Tools", strings.Join(removed, ", "))
	}
}
