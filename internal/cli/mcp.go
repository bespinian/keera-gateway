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
	fs.StringVar(&f.apiKey, "api-key", "", "credential to store encrypted; @- reads it from stdin")
	fs.BoolVar(&f.noAPIKey, "no-api-key", false, "remove the stored credential")
	return f
}

// mcpPut is the body of PUT /v1/mcp-servers/{alias}.
type mcpPut struct {
	URL         string  `json:"url"`
	Description string  `json:"description"`
	AuthHeader  string  `json:"auth_header"`
	Enabled     bool    `json:"enabled"`
	APIKey      *string `json:"api_key,omitempty"`
}

func mcpCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("mcp "+sub, flag.ExitOnError)
	asJSON := fs.Bool("json", false, jsonUsage)
	org := fs.String("org", "", orgUsage)
	fs.Usage = func() { _ = printHelp(fs, "mcp", sub) }
	if want, ok := wantsHelp(args); ok {
		registerMCPFlags(fs)
		fs.Bool("disabled", false, "add the server without serving it yet")
		fs.Bool("yes", false, yesUsage)
		registerCallFlags(fs)
		return printHelp(fs, "mcp", want)
	}
	c := newClient()
	m := &mcpRun{c: c, fs: fs, args: rest, asJSON: asJSON}
	switch sub {
	case "list", "ls", "":
		if err := parse(fs, rest); err != nil {
			return err
		}
		if err := m.resolveOrg(ctx, *org); err != nil {
			return err
		}
		servers, err := m.servers(ctx)
		if err != nil {
			return err
		}
		return out(*asJSON, servers, func(w *table) { printMCPServers(w, servers) })
	case "add", "create", "new":
		return m.save(ctx, org, true)
	case "set", "edit", "update":
		return m.save(ctx, org, false)
	case "enable", "disable":
		return m.toggle(ctx, org, sub)
	case "delete", "rm", "remove":
		return m.delete(ctx, org)
	case "calls":
		return mcpCalls(ctx, c, fs, rest, asJSON, org)
	case "connect":
		return m.connect(ctx, org)
	default:
		return unknownSub("mcp", sub)
	}
}

// mcpRun is one 'keera mcp' invocation.
type mcpRun struct {
	c      *client
	fs     *flag.FlagSet
	args   []string
	asJSON *bool
	// org is the organisation whose servers to use. Empty leaves it to the
	// control plane: the caller's own.
	org string
}

// path is a control API path for one of the organisation's servers.
func (m *mcpRun) path(alias string) string {
	return inOrg("/v1/mcp-servers/"+url.PathEscape(alias), m.org)
}

// resolveOrg fills in the organisation, as every command does.
func (m *mcpRun) resolveOrg(ctx context.Context, given string) (err error) {
	m.org, err = resolveOrg(ctx, m.c, given)
	return err
}

func (m *mcpRun) servers(ctx context.Context) ([]policy.MCPServer, error) {
	return list[policy.MCPServer](ctx, m.c, inOrg("/v1/mcp-servers", m.org))
}

// require reads one server. There is no endpoint for one; the list is small.
func (m *mcpRun) require(ctx context.Context, alias string) (policy.MCPServer, error) {
	servers, err := m.servers(ctx)
	if err != nil {
		return policy.MCPServer{}, err
	}
	for _, s := range servers {
		if s.Alias == alias {
			// The writes go to the organisation it belongs to.
			m.org = s.OrgID
			return s, nil
		}
	}
	return policy.MCPServer{}, fmt.Errorf("no MCP server %s (see: keera mcp list)", alias)
}

// save is 'add' and 'set': 'set' reads the server first, so a flag left out
// keeps what is there.
func (m *mcpRun) save(ctx context.Context, org *string, adding bool) error {
	fs, args := m.fs, m.args
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
	if err := m.resolveOrg(ctx, *org); err != nil {
		return err
	}
	put := mcpPut{Enabled: !f.disabled}
	if !adding {
		cur, err := m.require(ctx, alias)
		if err != nil {
			return err
		}
		put = mcpPut{URL: cur.URL, Description: cur.Description, AuthHeader: cur.AuthHeader,
			Enabled: cur.Enabled}
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
	return m.put(ctx, alias, put)
}

func (m *mcpRun) put(ctx context.Context, alias string, put mcpPut) error {
	var saved policy.MCPServer
	if err := m.c.do(ctx, "PUT", m.path(alias), put, &saved); err != nil {
		return err
	}
	return out(*m.asJSON, saved, func(w *table) { printMCPServer(w, saved) })
}

func (m *mcpRun) toggle(ctx context.Context, org *string, sub string) error {
	if err := parseArgs(m.fs, m.args, 1, "usage: keera mcp "+sub+" <alias>"); err != nil {
		return err
	}
	if err := m.resolveOrg(ctx, *org); err != nil {
		return err
	}
	cur, err := m.require(ctx, m.fs.Arg(0))
	if err != nil {
		return err
	}
	return m.put(ctx, cur.Alias, mcpPut{URL: cur.URL, Description: cur.Description,
		AuthHeader: cur.AuthHeader, Enabled: sub == "enable"})
}

func (m *mcpRun) delete(ctx context.Context, org *string) error {
	yes := m.fs.Bool("yes", false, yesUsage)
	if err := parseArgs(m.fs, m.args, 1, "usage: keera mcp delete <alias> [--yes]"); err != nil {
		return err
	}
	alias := m.fs.Arg(0)
	if err := m.resolveOrg(ctx, *org); err != nil {
		return err
	}
	srv, err := m.require(ctx, alias)
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Printf("%s\n", style.head("Deleting the MCP server "+alias+":"))
		fmt.Println("  every client configured for it stops reaching its tools")
		if srv.HasAPIKey {
			fmt.Println("  its stored credential is removed")
		}
		fmt.Println("Tool-call history and the audit log are kept.")
		if err := confirmTyping("alias", alias, "nothing was deleted"); err != nil {
			return err
		}
	}
	return deleteAlias(ctx, m.c, m.path(alias), alias, *m.asJSON)
}

// callFlags narrow 'keera mcp calls'.
type callFlags struct {
	server, tool string
	who          *who
	since        time.Duration
	limit        int
	summary      bool
}

func registerCallFlags(fs *flag.FlagSet) *callFlags {
	f := &callFlags{}
	fs.StringVar(&f.server, "server", "", "restrict to one MCP server")
	fs.StringVar(&f.tool, "tool", "", "restrict to one tool")
	f.who = registerWho(fs)
	fs.DurationVar(&f.since, "since", 24*time.Hour, "how far back to look")
	fs.IntVar(&f.limit, "limit", 50, "how many calls to print")
	fs.BoolVar(&f.summary, "summary", false, "add up the calls of each tool instead")
	return f
}

func mcpCalls(ctx context.Context, c *client, fs *flag.FlagSet, args []string, asJSON *bool, org *string) error {
	f := registerCallFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	orgID, err := resolveOrg(ctx, c, *org)
	if err != nil {
		return err
	}
	params, err := f.who.params(ctx, c, orgID)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("from", sinceParam(f.since))
	setIfGiven(q, map[string]string{"org_id": orgID, "server": f.server, "tool": f.tool})
	setIfGiven(q, params)
	if f.summary {
		q.Set("summary", "1")
		var res struct {
			Data []store.ToolSummary `json:"data"`
		}
		if err := c.do(ctx, "GET", "/v1/tool-calls?"+q.Encode(), nil, &res); err != nil {
			return err
		}
		return out(*asJSON, res.Data, func(w *table) { printToolSummary(w, res.Data) })
	}
	q.Set("limit", strconv.Itoa(f.limit))
	var res struct {
		Data []store.ToolCallRow `json:"data"`
	}
	if err := c.do(ctx, "GET", "/v1/tool-calls?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	return out(*asJSON, res.Data, func(w *table) { printToolCalls(w, res.Data) })
}

// mcpConnect prints how to point the common clients at one server through
// the gateway. The key stays in the environment, as it does for 'keera
// connect'.
func (m *mcpRun) connect(ctx context.Context, org *string) error {
	c := m.c
	if err := parseArgs(m.fs, m.args, 1, "usage: keera mcp connect <alias>"); err != nil {
		return err
	}
	alias := m.fs.Arg(0)
	if err := m.resolveOrg(ctx, *org); err != nil {
		return err
	}
	if _, err := m.require(ctx, alias); err != nil {
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
	w.header("ALIAS\tURL\tCREDENTIAL\tENABLED\tDESCRIPTION")
	for _, m := range servers {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.Alias, dash(m.URL), mcpCredential(m),
			statusWord(strconv.FormatBool(m.Enabled)), dash(m.Description))
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

// mcpCredential says whether a server's credential is stored, never what it
// is.
func mcpCredential(m policy.MCPServer) string {
	if m.HasAPIKey {
		return "stored"
	}
	return "-"
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
