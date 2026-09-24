package connect

import (
	"slices"
	"strings"
	"unicode"
)

// Identify reads which client made a request. It lives beside the connect
// catalogue so both use the same client names; a name that differed would be
// two clients in every report.
//
// This is not a security boundary: a User-Agent is whatever the client says,
// so it tells what is connected, never who is allowed.

// ClientHeader is where a client may state its own name. It wins over the
// User-Agent, because a tool built on an HTTP library looks like that library:
// OpenCode through the AI SDK looks like Node.
const ClientHeader = "X-Keera-Client"

// maxClientName bounds the stored name, since the client chooses it.
const maxClientName = 32

// agents maps User-Agent product tokens to client keys.
//
// The first three have no label because the connect catalogue names them, so a
// client is called the same on every screen. The rest are clients Keera does
// not configure but can recognise. An unknown client falls back to its own
// product token.
var agents = []struct {
	key   string
	label string
	// products are User-Agent product tokens (the part before the slash),
	// matched whole. A substring match would put "openapi" under Pi.
	products []string
	// generic marks a runtime or library rather than a tool. It counts only
	// when nothing more specific matched: "python-requests/2.32 aider/0.60" is
	// Aider.
	generic bool
}{
	{key: "pi", products: []string{"pi", "pi-agent"}},
	{key: "opencode", products: []string{"opencode"}},
	{key: "claude-code", products: []string{"claude-cli", "claude-code"}},
	{key: "codex", label: "Codex", products: []string{"codex", "codex-cli", "codex_cli_rs"}},
	{key: "aider", label: "Aider", products: []string{"aider"}},
	{key: "keera", label: "Keera CLI", products: []string{"keera", "keera-cli"}},
	// The panel's playground states its name in the header. Its User-Agent is
	// the operator's browser, which says nothing about the deployment.
	{key: "keera-playground", label: "Keera Playground"},
	{key: "openai-sdk", label: "OpenAI SDK", generic: true,
		products: []string{"openai", "openai-python", "openai-node", "async-openai"}},
	{key: "anthropic-sdk", label: "Anthropic SDK", generic: true,
		products: []string{"anthropic", "anthropic-sdk-python", "anthropic-ai-sdk"}},
	{key: "langchain", label: "LangChain", generic: true,
		products: []string{"langchain", "langchainjs"}},
	{key: "curl", label: "curl", generic: true, products: []string{"curl"}},
	{key: "python", label: "Python", generic: true,
		products: []string{"python-requests", "python-httpx", "python-urllib3", "httpx", "urllib3"}},
	{key: "node", label: "Node", generic: true,
		products: []string{"node", "node-fetch", "undici", "axios"}},
	{key: "go", label: "Go client", generic: true, products: []string{"go-http-client"}},
}

// Identify names the client behind a request: from the stated header if set,
// otherwise from the User-Agent. It returns "" when the request says nothing,
// which some HTTP libraries do.
func Identify(stated, userAgent string) string {
	// A stated name is one name, maybe with a version: "My Editor/1.2".
	head, _, _ := strings.Cut(stated, "/")
	if name := clean(head); name != "" {
		if key, _, ok := match(name); ok {
			return key
		}
		return name
	}
	// A known tool wins over a runtime, and a runtime over an unknown name,
	// whatever order the tokens come in: "node/22 undici/6" is Node.
	var fallback, first string
	for _, p := range products(userAgent) {
		key, generic, ok := match(p)
		switch {
		case ok && !generic:
			return key
		case ok && fallback == "":
			fallback = key
		case !ok && first == "":
			first = p
		}
	}
	if fallback != "" {
		return fallback
	}
	return first
}

// ClientLabel is how a client key is shown. An unknown key is shown as it is,
// since it is the client's own name for itself.
func ClientLabel(key string) string {
	if key == "" {
		return "Unidentified"
	}
	for _, a := range agents {
		if a.key == key && a.label != "" {
			return a.label
		}
	}
	for _, c := range clients {
		if c.Key == key {
			return c.Label
		}
	}
	return key
}

// match resolves one product token to a client key, and says whether that
// client is only a runtime or library.
func match(product string) (key string, generic, ok bool) {
	for _, a := range agents {
		if a.key == product || slices.Contains(a.products, product) {
			return a.key, a.generic, true
		}
	}
	return "", false, false
}

// products returns the cleaned product tokens of a User-Agent, in order.
//
// Comments in parentheses are skipped at any depth, or "claude-cli/1.0.0
// (external, cli)" could be named after the platform in its comment.
func products(ua string) []string {
	var out []string
	depth := 0
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		name, _, _ := strings.Cut(token.String(), "/")
		token.Reset()
		if name = clean(name); name != "" {
			out = append(out, name)
		}
	}
	for _, r := range ua {
		switch {
		case r == '(':
			flush()
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		case depth > 0:
		case unicode.IsSpace(r):
			flush()
		default:
			token.WriteRune(r)
		}
	}
	flush()
	return out
}

// clean turns a client-written name into a report key: lower case, product
// token characters only, and bounded. A result with no letter or digit is no
// name.
func clean(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if b.Len() >= maxClientName {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			// Punctuation may not start a name.
			if b.Len() > 0 {
				b.WriteRune(r)
			}
		}
	}
	name := strings.TrimRight(b.String(), "-_.")
	if !strings.ContainsFunc(name, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	}) {
		return ""
	}
	return name
}
