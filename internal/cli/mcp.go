package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// mcpFlags are the fields of an MCP server, as flags. Each one left out means
// "leave this as it is", so `keera mcp set` can change one field.
type mcpFlags struct {
	endpoint    string
	description string
	authHeader  string
	apiKeyEnv   string
	apiKey      string
	noAPIKey    bool
	disabled    bool
}

func registerMCPFlags(fs *flag.FlagSet) *mcpFlags {
	f := &mcpFlags{}
	// Not --url, which every command already reads as the gateway to talk to.
	fs.StringVar(&f.endpoint, "endpoint", "", "the server's Streamable HTTP endpoint")
	fs.StringVar(&f.description, "description", "", "what the server is for, in a sentence")
	fs.StringVar(&f.authHeader, "auth-header", "",
		"header the credential goes in (default: Authorization, as a bearer token)")
	fs.StringVar(&f.apiKeyEnv, "api-key-env", "",
		"environment variable on the gateway that holds the credential")
	fs.StringVar(&f.apiKey, "api-key", "", "credential to store encrypted; @- reads it from stdin")
	fs.BoolVar(&f.noAPIKey, "no-api-key", false, "remove the stored credential")
	return f
}

// mcpPut is the body of PUT /v1/mcp-servers/{alias}.
type mcpPut struct {
	URL         string  `json:"url"`
	Description string  `json:"description"`
	AuthHeader  string  `json:"auth_header"`
	APIKeyEnv   string  `json:"api_key_env"`
	Enabled     bool    `json:"enabled"`
	APIKey      *string `json:"api_key,omitempty"`
}

func mcpCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("mcp "+sub, flag.ExitOnError)
	asJSON := fs.Bool("json", false, jsonUsage)
	fs.Usage = func() { _ = printHelp(fs, "mcp", sub) }
	if want, ok := wantsHelp(args); ok {
		registerMCPFlags(fs)
		fs.Bool("disabled", false, "add the server without serving it yet")
		fs.Bool("yes", false, yesUsage)
		registerCallFlags(fs)
		return printHelp(fs, "mcp", want)
	}
	c := newClient()
	switch sub {
	case "list", "ls", "":
		if err := parse(fs, rest); err != nil {
			return err
		}
		servers, err := mcpServers(ctx, c)
		if err != nil {
			return err
		}
		return out(*asJSON, servers, func(w *table) { printMCPServers(w, servers) })
	case "add", "create", "new":
		return mcpSave(ctx, c, fs, rest, *asJSON, true)
	case "set", "edit", "update":
		return mcpSave(ctx, c, fs, rest, *asJSON, false)
	case "enable", "disable":
		return mcpToggle(ctx, c, fs, sub, rest, *asJSON)
	case "delete", "rm", "remove":
		return mcpDelete(ctx, c, fs, rest, *asJSON)
	case "calls":
		return mcpCalls(ctx, c, fs, rest, *asJSON)
	case "connect":
		return mcpConnect(ctx, c, fs, rest)
	default:
		return unknownSub("mcp", sub)
	}
}

func mcpServers(ctx context.Context, c *client) ([]policy.MCPServer, error) {
	return list[policy.MCPServer](ctx, c, "/v1/mcp-servers")
}

// requireMCP reads one server. There is no endpoint for one; the list is
// small.
func requireMCP(ctx context.Context, c *client, alias string) (policy.MCPServer, error) {
	servers, err := mcpServers(ctx, c)
	if err != nil {
		return policy.MCPServer{}, err
	}
	for _, m := range servers {
		if m.Alias == alias {
			return m, nil
		}
	}
	return policy.MCPServer{}, fmt.Errorf("no MCP server %s (see: keera mcp list)", alias)
}

// mcpSave is 'add' and 'set': 'set' reads the server first, so a flag left
// out keeps what is there.
func mcpSave(ctx context.Context, c *client, fs *flag.FlagSet, args []string, asJSON, adding bool) error {
	f := registerMCPFlags(fs)
	verb := "set"
	if adding {
		fs.BoolVar(&f.disabled, "disabled", false, "add the server without serving it yet")
		verb = "add"
	}
	if err := parseArgs(fs, args, 1, "usage: keera mcp "+verb+" <alias> [flags]"); err != nil {
		return err
	}
	alias := fs.Arg(0)
	put := mcpPut{Enabled: !f.disabled}
	if !adding {
		cur, err := requireMCP(ctx, c, alias)
		if err != nil {
			return err
		}
		put = mcpPut{URL: cur.URL, Description: cur.Description, AuthHeader: cur.AuthHeader,
			APIKeyEnv: cur.APIKeyEnv, Enabled: cur.Enabled}
	}
	given := func(name string) bool {
		seen := false
		fs.Visit(func(fl *flag.Flag) { seen = seen || fl.Name == name })
		return seen
	}
	for name, set := range map[string]func(){
		"endpoint":    func() { put.URL = f.endpoint },
		"description": func() { put.Description = f.description },
		"auth-header": func() { put.AuthHeader = f.authHeader },
		"api-key-env": func() { put.APIKeyEnv = f.apiKeyEnv },
	} {
		if given(name) {
			set()
		}
	}
	if adding && put.URL == "" {
		return fmt.Errorf("--endpoint is required: the server's Streamable HTTP endpoint")
	}
	cred, err := credential(&modelFlags{apiKey: f.apiKey, noAPIKey: f.noAPIKey})
	if err != nil {
		return err
	}
	put.APIKey = cred
	return putMCP(ctx, c, alias, put, asJSON)
}

func putMCP(ctx context.Context, c *client, alias string, put mcpPut, asJSON bool) error {
	var saved policy.MCPServer
	if err := c.do(ctx, "PUT", "/v1/mcp-servers/"+url.PathEscape(alias), put, &saved); err != nil {
		return err
	}
	return out(asJSON, saved, func(w *table) { printMCPServer(w, saved) })
}

func mcpToggle(ctx context.Context, c *client, fs *flag.FlagSet, sub string, args []string, asJSON bool) error {
	if err := parseArgs(fs, args, 1, "usage: keera mcp "+sub+" <alias>"); err != nil {
		return err
	}
	cur, err := requireMCP(ctx, c, fs.Arg(0))
	if err != nil {
		return err
	}
	return putMCP(ctx, c, cur.Alias, mcpPut{URL: cur.URL, Description: cur.Description,
		AuthHeader: cur.AuthHeader, APIKeyEnv: cur.APIKeyEnv, Enabled: sub == "enable"}, asJSON)
}

func mcpDelete(ctx context.Context, c *client, fs *flag.FlagSet, args []string, asJSON bool) error {
	yes := fs.Bool("yes", false, yesUsage)
	if err := parseArgs(fs, args, 1, "usage: keera mcp delete <alias> [--yes]"); err != nil {
		return err
	}
	alias := fs.Arg(0)
	if !*yes {
		if err := confirmTyping("MCP server", alias,
			"Every client configured for it stops reaching its tools."); err != nil {
			return err
		}
	}
	var gone struct {
		Alias   string `json:"alias"`
		Deleted bool   `json:"deleted"`
	}
	if err := c.do(ctx, "DELETE", "/v1/mcp-servers/"+url.PathEscape(alias), nil, &gone); err != nil {
		return err
	}
	return out(asJSON, gone, func(w *table) { _, _ = fmt.Fprintf(w, "deleted %s\n", gone.Alias) })
}

// callFlags narrow 'keera mcp calls'.
type callFlags struct {
	org, server, tool, team, key string
	since                        time.Duration
	limit                        int
	summary                      bool
}

func registerCallFlags(fs *flag.FlagSet) *callFlags {
	f := &callFlags{}
	fs.StringVar(&f.org, "org", "", "restrict to one organisation")
	fs.StringVar(&f.server, "server", "", "restrict to one MCP server")
	fs.StringVar(&f.tool, "tool", "", "restrict to one tool")
	fs.StringVar(&f.team, "team", "", "restrict to one team id")
	fs.StringVar(&f.key, "key", "", "restrict to one key id")
	fs.DurationVar(&f.since, "since", 24*time.Hour, "how far back to look")
	fs.IntVar(&f.limit, "limit", 50, "how many calls to print")
	fs.BoolVar(&f.summary, "summary", false, "add up the calls of each tool instead")
	return f
}

func mcpCalls(ctx context.Context, c *client, fs *flag.FlagSet, args []string, asJSON bool) error {
	f := registerCallFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("from", sinceParam(f.since))
	for k, v := range map[string]string{"org_id": f.org, "server": f.server, "tool": f.tool,
		"team_id": f.team, "key_id": f.key} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.summary {
		q.Set("summary", "true")
		var res struct {
			Data []store.ToolSummary `json:"data"`
		}
		if err := c.do(ctx, "GET", "/v1/tool-calls?"+q.Encode(), nil, &res); err != nil {
			return err
		}
		return out(asJSON, res.Data, func(w *table) { printToolSummary(w, res.Data) })
	}
	q.Set("limit", strconv.Itoa(f.limit))
	var res struct {
		Data []store.ToolCallRow `json:"data"`
	}
	if err := c.do(ctx, "GET", "/v1/tool-calls?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(asJSON, res.Data, func(w *table) { printToolCalls(w, res.Data) })
}

// mcpConnect prints how to point the common clients at one server through
// the gateway. The key stays in the environment, as it does for 'keera
// connect'.
func mcpConnect(ctx context.Context, c *client, fs *flag.FlagSet, args []string) error {
	if err := parseArgs(fs, args, 1, "usage: keera mcp connect <alias>"); err != nil {
		return err
	}
	alias := fs.Arg(0)
	if _, err := requireMCP(ctx, c, alias); err != nil {
		return err
	}
	var cat struct {
		GatewayURL string `json:"gateway_url"`
	}
	if err := c.do(ctx, "GET", "/v1/connect", nil, &cat); err != nil {
		return err
	}
	endpoint := strings.TrimRight(cat.GatewayURL, "/") + "/mcp/" + alias
	fmt.Printf(`# Claude Code
claude mcp add --transport http %[1]s %[2]s \
  --header "Authorization: Bearer $KEERA_API_KEY"

# Codex, in ~/.codex/config.toml
[mcp_servers.%[1]s]
url = "%[2]s"
bearer_token_env_var = "KEERA_API_KEY"

# Anything else that reads an mcp.json
{"mcpServers": {"%[1]s": {"type": "http", "url": "%[2]s",
  "headers": {"Authorization": "Bearer ${KEERA_API_KEY}"}}}}
`, alias, endpoint)
	return nil
}

func printMCPServers(w *table, servers []policy.MCPServer) {
	w.header("ALIAS\tURL\tCREDENTIAL\tSOURCE\tENABLED\tDESCRIPTION")
	for _, m := range servers {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Alias, dash(m.URL), mcpCredential(m),
			mcpSource(m), statusWord(strconv.FormatBool(m.Enabled)), dash(m.Description))
	}
}

func printMCPServer(w *table, m policy.MCPServer) {
	show(w, "alias", m.Alias)
	show(w, "endpoint", m.URL)
	if m.Description != "" {
		show(w, "description", m.Description)
	}
	header := m.AuthHeader
	if header == "" {
		header = "Authorization (bearer)"
	}
	show(w, "auth header", header)
	show(w, "credential", mcpCredential(m))
	show(w, "enabled", m.Enabled)
}

// mcpCredential says where a server's credential comes from, never what it is.
func mcpCredential(m policy.MCPServer) string {
	switch {
	case m.HasAPIKey:
		return "stored"
	case m.APIKeyEnv != "":
		return "$" + m.APIKeyEnv
	default:
		return "-"
	}
}

func mcpSource(m policy.MCPServer) string {
	if m.Managed {
		return "catalogue file"
	}
	return "control plane"
}

func printToolCalls(w *table, calls []store.ToolCallRow) {
	w.header("WHEN\tTOOL\tOUTCOME\tMS\tSENT\tBACK\tKEY\tERROR")
	for _, t := range calls {
		_, _ = fmt.Fprintf(w, "%s\t%s/%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
			t.TS.Local().Format("01-02 15:04:05"), t.Server, t.Tool, outcomeWord(t.Outcome),
			t.LatencyMS, t.ArgBytes, t.ResultBytes, dash(t.KeyID), dash(firstLine(t.Error)))
	}
}

func printToolSummary(w *table, rows []store.ToolSummary) {
	w.header("TOOL\tCALLS\tFAILED\tDENIED\tREFUSED\tAVG MS\tSENT\tBACK")
	for _, t := range rows {
		_, _ = fmt.Fprintf(w, "%s/%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", t.Server, t.Tool, t.Calls,
			t.Failed, t.Denied, t.Refused, t.AvgMS, t.ArgBytes, t.ResultBytes)
	}
}

// outcomeWord paints a tool call's outcome. The word says the same without
// the colour.
func outcomeWord(o store.ToolOutcome) string {
	switch o {
	case store.ToolOK:
		return style.ok(string(o))
	case store.ToolDenied, store.ToolRefused:
		return style.warn(string(o))
	default:
		return style.bad(string(o))
	}
}
