// Command demo fills a new organisation with a month of made-up activity, to
// show Keera to someone: projects, people, keys, models, guardrails, filters,
// routers, MCP servers, and the requests, sessions and tool calls of eight
// developers and three applications.
//
// The setup goes through the keera command, so the gateway checks it and the
// audit log records it. The traffic cannot: no API writes the past. It is
// written to Postgres the way the gateway writes its own.
//
// With -live, it then sends real requests to a model until Ctrl+C, so the
// panel moves. For an organisation that exists already, it only does that.
//
//	go run ./scripts/demo -org "Example Bank" -live keera-speed
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// devDatabase is the Postgres of `make dev`.
const devDatabase = "postgres://keera:keera@127.0.0.1:5432/keera?sslmode=disable"

type options struct {
	org, domain, database, gateway, live string
	days                                 int
	replace                              bool
}

func main() {
	var o options
	flag.StringVar(&o.org, "org", "Muster AG", "name of the organisation to create")
	flag.StringVar(&o.domain, "domain", "muster.example", "email domain of its people")
	flag.IntVar(&o.days, "days", 30, "days of traffic, up to now")
	flag.BoolVar(&o.replace, "replace", false,
		"delete an organisation of the same name first, with its request log")
	flag.StringVar(&o.live, "live", "",
		"then send requests to this model or router every few seconds until Ctrl+C,\n"+
			"such as "+localModel+". An organisation that exists already only gets these")
	flag.StringVar(&o.gateway, "gateway",
		cmp.Or(os.Getenv("KEERA_CONTROL_URL"), "http://127.0.0.1:8080"),
		"the gateway the live requests go to")
	flag.StringVar(&o.database, "database", cmp.Or(os.Getenv("KEERA_DATABASE_URL"), devDatabase),
		"the gateway's Postgres")
	flag.Usage = func() {
		_, _ = fmt.Fprint(flag.CommandLine.Output(), "Usage: go run ./scripts/demo [flags]\n\n"+
			"Creates an organisation full of demo data. With -live, it then sends requests\n"+
			"to a model until Ctrl+C. The installed keera command must be signed in as an\n"+
			"operator, or KEERA_CONTROL_URL and KEERA_OPERATOR_KEY set.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if o.days < 1 {
		fmt.Fprintln(os.Stderr, "demo: -days must be at least 1")
		os.Exit(2)
	}
	// Ctrl+C ends the live requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
	st, err := store.Open(ctx, o.database, 4)
	if err != nil {
		return fmt.Errorf("the database at -database: %w", err)
	}
	defer st.Close()
	d := &demo{st: st, domain: o.domain}

	var oldID string
	err = st.Pool().QueryRow(ctx, "SELECT id FROM orgs WHERE lower(name) = lower($1)",
		o.org).Scan(&oldID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	switch {
	case oldID != "" && !o.replace && o.live != "":
		fmt.Printf("%q exists already, so it is left as it is.\n", o.org)
		d.orgID = oldID
	case oldID != "" && !o.replace:
		return fmt.Errorf("an organisation called %q exists already; -replace deletes it "+
			"and everything in it, and -live only sends requests to it", o.org)
	default:
		if err := d.generate(ctx, o, oldID); err != nil {
			return err
		}
	}
	if o.live == "" {
		return nil
	}
	return d.live(ctx, o.gateway, o.live)
}

// generate creates the organisation, after deleting oldID if it is not empty,
// and fills it.
func (d *demo) generate(ctx context.Context, o options, oldID string) error {
	if _, err := d.keera(ctx, "whoami"); err != nil {
		return err
	}
	if oldID != "" {
		if err := d.clear(ctx, o.org, oldID); err != nil {
			return err
		}
	}
	var org store.Org
	if err := d.keeraJSON(ctx, &org, "org", "create", o.org); err != nil {
		return err
	}
	d.orgID = org.ID
	fmt.Printf("organisation %s (%s)\n", org.Name, org.ID)

	steps := []struct {
		what string
		do   func(context.Context) error
	}{
		{"models", d.models},
		{"MCP servers", d.mcpServers},
		{"projects", d.makeProjects},
		{"people", d.people},
		{"filters", d.filters},
		{"routers", d.routers},
		{"guardrails", d.guardrails},
		{"keys", d.keys},
		{"credit", d.credit},
	}
	for _, s := range steps {
		fmt.Printf("  %s\n", s.what)
		if err := s.do(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
	}
	start := time.Now().AddDate(0, 0, -o.days)
	if err := d.backdate(ctx, start); err != nil {
		return err
	}

	fmt.Printf("  %d days of traffic\n", o.days)
	n, cost, err := d.traffic(ctx, start)
	if err != nil {
		return err
	}
	fmt.Printf("\n%d requests, %s, in %q. Switch to it in the panel, or:\n"+
		"  keera org switch %q\n", n, policy.FormatMoney(cost), org.Name, org.Name)
	return nil
}

// demo is the organisation being filled, and what was made in it.
type demo struct {
	st         *store.Store
	domain     string
	orgID      string
	projectIDs map[string]string // name to id
	userIDs    map[string]string // email to id
	keyIDs     map[string]string // name to id
	// secrets are the keys made in this run, for the live requests. They
	// are never printed.
	secrets map[string]string
}

// keera runs the keera command in the new organisation and returns its output.
func (d *demo) keera(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "keera", args...)
	// The shell's KEERA_ORG must not send anything to another organisation.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "KEERA_ORG=")
	})
	if d.orgID != "" {
		cmd.Env = append(cmd.Env, "KEERA_ORG="+d.orgID)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the keera command was not found: install it, " +
				"or put the one 'make build' wrote on your PATH")
		}
		return nil, fmt.Errorf("keera %s: %w\n%s", strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// keeraJSON runs a command with --json and decodes what it prints into v.
func (d *demo) keeraJSON(ctx context.Context, v any, args ...string) error {
	out, err := d.keera(ctx, append(args, "--json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// clear deletes an organisation with its request log. The gateway keeps the
// log of a deleted organisation, which for demo data would only pile up.
func (d *demo) clear(ctx context.Context, name, oldID string) error {
	if _, err := d.keera(ctx, "org", "delete", oldID, "--yes"); err != nil {
		return err
	}
	for _, q := range []string{
		"DELETE FROM usage_events WHERE org_id = $1",
		"DELETE FROM usage_daily WHERE org_id = $1",
		"DELETE FROM filter_runs WHERE org_id = $1",
		"DELETE FROM tool_calls WHERE org_id = $1",
		// Payments are kept too, as the record of what was paid.
		"DELETE FROM payments WHERE org_id = $1",
		"DELETE FROM credit_accounts WHERE org_id = $1",
		// A router's spend is not deleted with the organisation.
		"DELETE FROM spend WHERE scope_type = 'router' AND scope_id LIKE $1 || '/%'",
	} {
		if _, err := d.st.Pool().Exec(ctx, q, oldID); err != nil {
			return err
		}
	}
	fmt.Printf("deleted the old %s (%s)\n", name, oldID)
	return nil
}

// The models every new organisation gets from compose/models.yaml. The
// filters and routers run on them.
const (
	localModel    = "keera-speed"
	decisionModel = "keera-decider"
)

// hosted are the models the demo adds, one per place a prompt can go. They
// have no API key, so they answer nothing until an administrator sets one.
var hosted = []struct{ alias, provider, model, description string }{
	{
		"claude-opus-5-5", "anthropic", "claude-opus-5-5",
		"Design, architecture, multi-step reasoning and whole-system questions.",
	},
	{"qwen3-coder-next", "stepping-stone", "Qwen/Qwen3-Coder-Next", ""},
	{"codestral-2508", "mistral", "codestral-2508", ""},
	{"bge-m3", "stepping-stone", "BAAI/bge-m3", ""},
}

func (d *demo) models(ctx context.Context) error {
	have, err := d.st.ListModels(ctx, d.orgID)
	if err != nil {
		return err
	}
	for _, want := range []string{localModel, decisionModel} {
		if !slices.ContainsFunc(have, func(m policy.Model) bool { return m.Alias == want }) {
			return fmt.Errorf("the gateway gave the organisation no %s. Its models file "+
				"must declare %s and %s, as compose/models.yaml does", want, localModel, decisionModel)
		}
	}
	for _, m := range hosted {
		args := []string{"model", "add", m.alias, "--provider", m.provider, "--backend-model", m.model}
		if m.description != "" {
			args = append(args, "--description", m.description)
		}
		if _, err := d.keera(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func (d *demo) mcpServers(ctx context.Context) error {
	for _, s := range []struct{ alias, endpoint, description string }{
		{"github", "https://api.githubcopilot.com/mcp/", "Repositories, issues and pull requests."},
		{"atlassian", "https://mcp.atlassian.com/v1/mcp", "Jira tickets and Confluence pages."},
	} {
		if _, err := d.keera(ctx, "mcp", "add", s.alias, "--endpoint", s.endpoint,
			"--description", s.description); err != nil {
			return err
		}
	}
	return nil
}

var projects = []struct{ name, description string }{
	{"payments", "The payments platform"},
	{"customer-portal", "E-banking on the web and in the app"},
	{"support-assistant", "The chat assistant on the support pages"},
	{"data-science", "Risk models and reporting"},
}

func (d *demo) makeProjects(ctx context.Context) error {
	d.projectIDs = map[string]string{}
	for _, p := range projects {
		var made store.Project
		if err := d.keeraJSON(ctx, &made, "project", "create", p.name,
			"--description", p.description); err != nil {
			return err
		}
		d.projectIDs[p.name] = made.ID
	}
	return nil
}

// people are written to the database, because the control API adds only
// people who can sign in, and a demo gateway usually has no identity provider.
func (d *demo) people(ctx context.Context) error {
	d.userIDs = map[string]string{}
	for _, p := range team {
		email := p.email(d.domain)
		role := "member"
		if p.admin {
			role = "admin"
		}
		u, err := d.st.AddUser(ctx, id.New("user"), d.orgID, email, "", role)
		if err != nil {
			return err
		}
		// An identity provider sets the name at sign-in, and nobody signs in.
		if _, err := d.st.Pool().Exec(ctx, "UPDATE users SET name = $2 WHERE id = $1",
			u.ID, p.name); err != nil {
			return err
		}
		d.userIDs[email] = u.ID
	}
	return nil
}

// secretRules are the pattern filter every request passes through.
const secretRules = `(?i)\b(?:sk|pk|rk)-[a-z0-9_-]{20,}\b => [CREDENTIAL]
(?i)\bpostgres(?:ql)?://[^\s:@]+:[^\s@]+@ => postgres://[CREDENTIAL]@
\b756\.\d{4}\.\d{4}\.\d{2}\b => [AHV NUMBER]
\bCH\d{2}(?: ?[0-9A-Z]{4}){4} ?[0-9A-Z]\b => [IBAN]
-----BEGIN [A-Z ]*PRIVATE KEY----- => REFUSE: that moves a private key out`

func (d *demo) filters(ctx context.Context) error {
	for _, args := range [][]string{
		{
			"redact-secrets", "--mode", "pattern", "--rules", secretRules,
			"--description", "Credentials, AHV numbers and IBANs, everywhere.",
		},
		{
			"no-client-data", "--mode", "gate", "--model", decisionModel,
			"--prompt", "Refuse requests containing real client data, such as a client's name, account or balance.",
			"--description", "Keeps client data out of the payments team's prompts.",
		},
		{
			"redact-names", "--mode", "rewrite", "--model", localModel, "--shadow",
			"--prompt", "Replace the name of every person with [NAME]. Change nothing else.",
			"--description", "Takes names out of support chats. In shadow while it is tuned.",
		},
	} {
		if _, err := d.keera(ctx, append([]string{"filter", "add"}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

// autoRouter sends the easy half of the payments team's work to the local
// model, and the rest to the frontier model while its budget lasts.
var autoRouter = map[string]any{
	"id": "route", "kind": "route", "mode": "instruction",
	"model": decisionModel,
	"prompt": "Send renames, small edits and questions about syntax to keera-speed, " +
		"and design, debugging and work across many files to claude-opus-5-5.",
	"destinations": []string{localModel, "claude-opus-5-5"},
	"fallback":     localModel,
	"branches": map[string]any{
		"claude-opus-5-5": map[string]any{
			"id": autoBudgetStep, "kind": "budget", "budget_micros": 250_000_000, "period": "month",
			"exceeded": map[string]any{"id": "local", "kind": "model", "ref": localModel},
			"next":     map[string]any{"id": "frontier", "kind": "model", "ref": "claude-opus-5-5"},
		},
	},
}

// autoBudgetStep is the budget step in autoRouter, which counts spend under
// its id.
const autoBudgetStep = "frontier-budget"

func (d *demo) routers(ctx context.Context) error {
	f, err := os.CreateTemp("", "keera-demo-router-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	err = json.NewEncoder(f).Encode(autoRouter)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := d.keera(ctx, "router", "add", "auto", "--file", f.Name(),
		"--display-name", "Auto",
		"--description", "Local model for the easy half, frontier model for the rest."); err != nil {
		return err
	}
	_, err = d.keera(ctx, "router", "add", "swiss-first", "--mode", "fallback",
		"--destinations", "qwen3-coder-next,codestral-2508", "--display-name", "Swiss first",
		"--description", "Stays in Switzerland, and goes to the EU only when that is down.")
	return err
}

func (d *demo) guardrails(ctx context.Context) error {
	for _, args := range [][]string{
		{"org", "--filters", "redact-secrets", "--budget", "600", "--period", "month"},
		{
			"project", "payments", "--models", "auto,keera-speed,claude-opus-5-5",
			"--filters", "no-client-data", "--requests", "600", "--budget", "300",
		},
		{
			"project", "customer-portal", "--locations", "ch,eu,onprem", "--tokens", "400000",
			"--budget", "120",
		},
		{
			"project", "support-assistant", "--locations", "ch,onprem", "--filters", "redact-names",
			"--budget", "80", "--system-prompt",
			"Answer in the language of the question. Never promise a refund; offer to open a ticket.",
		},
		{
			"project", "data-science", "--models", "codestral-2508,qwen3-coder-next,bge-m3,keera-speed",
			"--budget", "50",
		},
	} {
		if _, err := d.keera(ctx, append([]string{"guardrail", "set"}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

func (d *demo) keys(ctx context.Context) error {
	d.keyIDs, d.secrets = map[string]string{}, map[string]string{}
	issue := func(name, project string, owner ...string) error {
		args := []string{"key", "create", "--name", name, "--project", project}
		if len(owner) > 0 {
			args = append(args, "--user", owner[0])
		} else {
			args = append(args, "--no-user")
		}
		var k struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		}
		if err := d.keeraJSON(ctx, &k, args...); err != nil {
			return err
		}
		d.keyIDs[name], d.secrets[name] = k.ID, k.Key
		return nil
	}
	for _, p := range team {
		if err := issue(p.keyName(), p.project, p.email(d.domain)); err != nil {
			return err
		}
	}
	for _, s := range services {
		if err := issue(s.name, s.project); err != nil {
			return err
		}
	}
	// The notebooks run unattended, so they get a budget per day of their own.
	_, err := d.keera(ctx, "budget", "key", d.keyIDs[notebooksKey], "--budget", "3",
		"--period", "day")
	return err
}

// credit grants the organisation credit, as if it had paid. Without any, a
// deployment that takes payments shows its administrators the getting
// started screen instead of the dashboard.
func (d *demo) credit(ctx context.Context) error {
	// The bill says whether the deployment takes payments. 'billing credit'
	// is refused where it does not.
	var bill struct {
		Payments bool `json:"payments"`
	}
	if err := d.keeraJSON(ctx, &bill, "billing"); err != nil {
		return err
	}
	if !bill.Payments {
		fmt.Println("    none: this deployment takes no payments")
		return nil
	}
	_, err := d.keera(ctx, "billing", "grant", "--org", d.orgID, "--amount", "500",
		"--note", "Demo credit")
	return err
}

// backdate makes everything look as old as the traffic it is given, so no key
// was created after its first request.
func (d *demo) backdate(ctx context.Context, start time.Time) error {
	for _, q := range []string{
		"UPDATE orgs SET created_at = $2 WHERE id = $1",
		"UPDATE projects SET created_at = $2 WHERE org_id = $1",
		"UPDATE users SET created_at = $2 WHERE org_id = $1",
		"UPDATE api_keys SET created_at = $2 WHERE org_id = $1",
		"UPDATE payments SET ts = $2, settled_at = $2 WHERE org_id = $1",
	} {
		if _, err := d.st.Pool().Exec(ctx, q, d.orgID, start.Add(-24*time.Hour)); err != nil {
			return err
		}
	}
	return nil
}
