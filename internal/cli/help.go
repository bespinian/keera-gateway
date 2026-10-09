package cli

// The help this command line gives.
//
// Help is a structure rather than one block of prose, so the answer can be
// the size of the question: `keera` prints the summaries, `keera help
// <command>` one section, and `keera help <command> <subcommand>` one verb
// with only its own flags.
//
// Flag descriptions live on the FlagSet each command parses with, and
// printHelp is handed that FlagSet, so each flag is worded once. This file
// adds which verb reads which flag, which the shared FlagSet cannot say.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// command is one noun this command line understands.
type command struct {
	name    string
	aliases []string
	// summary is the one line `keera` prints beside the name.
	summary string
	// prose explains the idea rather than the syntax. Printed under the
	// subcommand list.
	prose string
	// subs are the verbs. A command with none, such as `keera usage`, takes
	// flags directly and names them in args and flags instead.
	subs  []subcommand
	args  string
	flags []string
	// examples are printed last, where they are easiest to find.
	examples []string
	// hidden keeps a command out of the overview without making it an error.
	hidden bool
}

// subcommand is one verb of one command.
type subcommand struct {
	name string
	// aliases are the other spellings the dispatcher accepts for this verb,
	// so help can print them and 'keera help <cmd> <alias>' finds the verb.
	aliases []string
	args    string
	summary string
	// flags names the flags this verb reads, out of the ones its command
	// declares. help_test checks them against the real FlagSet.
	flags    []string
	prose    string
	examples []string
}

// tagline is the first line of the help.
const tagline = "keera - administer a Keera Gateway"

// commands is every command, in the order the overview lists them: set-up
// first, then what sits in front of a request, then the reports.
var commands = []command{
	{
		name:    "login",
		summary: "sign in to a deployment through its identity provider or a passkey",
		args:    "[flags]",
		flags:   []string{"provider", "no-browser"},
		prose: "Opens a browser and signs you in, so commands run as you and the audit log " +
			"names you. --url also makes that gateway the default for every later command; " +
			"'keera whoami' says which one is in use. See docs/sso.md.",
		examples: []string{
			"keera login                # the hosted gateway",
			"keera login --url https://keera.example.ch",
			"keera login --no-browser   # a terminal with no browser on it",
		},
	},
	{
		name:    "logout",
		summary: "end this machine's sign-in",
		args:    "[flags]",
		flags:   []string{"all"},
		prose: "Deletes this machine's credential and tells the gateway to forget it. " +
			"With --all, every sign-in this account holds anywhere - every other machine, " +
			"and the panel in every browser.",
	},
	{
		name:    "whoami",
		summary: "who the credential in hand belongs to, and which gateway",
		args:    "[flags]",
		flags:   []string{"json"},
		prose: "Run it when something is refused, to see which credential this shell uses " +
			"and which deployment it talks to.",
	},
	{
		name:    "doctor",
		summary: "check a deployment end to end and say what is missing",
		args:    "[flags]",
		flags:   []string{"probe", "org", "json"},
		prose: "One command for \"why does this not work\". It reads the deployment's " +
			"configuration and an organisation's models and says what is missing, with the thing to do " +
			"about each. It changes nothing, so it is safe against live traffic.\n\n" +
			"--probe also asks every enabled model to answer. That is the only check that " +
			"catches the failure which quietly breaks coding agents: a backend returning " +
			"prose where a tool call was asked for. It costs a generation per model, so it " +
			"is not the default.",
		examples: []string{
			"keera doctor",
			"keera doctor --probe",
		},
	},
	{
		name:    "org",
		aliases: []string{"orgs"},
		summary: "organisations: one per customer, or exactly one",
		prose: "An organisation's --domain is the part of an address after the @, and it is " +
			"what places a sign-in in the right organisation. Until there are two organisations " +
			"nothing needs it: everyone who signs in lands in the only one there is. From " +
			"the moment there are two, an address matching no organisation's domain is " +
			"refused rather than put in one - so it is set on every organisation before the " +
			"second one exists, including the one that was there first. Through a provider " +
			"with KEERA_OIDC_<NAME>_SIGNUP, such a person creates an organisation of their own " +
			"instead.",
		subs: []subcommand{
			{name: "create", aliases: []string{"add", "new"}, args: "<name>", summary: "create an organisation",
				flags: []string{"domain", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "every organisation", flags: []string{"json"}},
			{name: "set", aliases: []string{"edit", "update", "rename"}, args: "[<org-id>]", summary: "rename it, or set the email domain whose sign-ins land in it",
				flags: []string{"name", "domain", "no-domain", "lift-limit", "json"},
				prose: "Names are unique, ignoring case. Everything refers to an organisation " +
					"by its id, so a rename changes only what it is called. The old name stays " +
					"in the audit log. An organisation somebody created by signing up is " +
					"limited until it buys credit: --lift-limit opens the models and MCP servers " +
					"inside this deployment's network, and sandboxes, to it sooner."},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<org-id>", summary: "delete an organisation and everything in it",
				flags: []string{"yes", "json"},
				prose: "Refused while any of its sandboxes is still live: terminate them first, " +
					"so no machine keeps running after its organisation is gone."},
		},
		examples: []string{
			`keera org create "Example Bank" --domain example.ch`,
			`keera org set org_123 --name "Example Bank AG"`,
		},
	},
	{
		name:    "project",
		aliases: []string{"projects"},
		summary: "projects inside an organisation",
		prose: "A project groups keys, usually those of one product or one group of people, so " +
			"that budgets and reports line up with how the organisation works. The guardrails " +
			"on a project apply to every key in it. Every key is in a project. An " +
			"organisation starts with one, called 'default', which is a project like any " +
			"other. A key issued without --project goes in the organisation's oldest " +
			"project. An organisation with no projects cannot have keys.",
		subs: []subcommand{
			{name: "create", aliases: []string{"add", "new"}, args: "<name>", summary: "create a project",
				flags: []string{"org", "description", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "every project", flags: []string{"org", "json"}},
			{name: "set", aliases: []string{"edit", "update", "rename"}, args: "<project>",
				summary: "rename a project or change its description",
				flags:   []string{"org", "name", "description", "json"},
				prose: "Names are unique in an organisation. The name is a label: keys, " +
					"guardrails, spend and every usage row hold the project by its id, so a " +
					"rename changes only what reports are headed. The old name stays in the " +
					"audit log."},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<project>", summary: "delete a project whose keys are all revoked",
				flags: []string{"org", "yes", "json"},
				prose: "Refused while any key in the project is not revoked, expired ones " +
					"included: deleting it would take those credentials with it. Revoke them first. A key cannot change " +
					"project, so issue a new one in another project for anything that still " +
					"needs one. Revoked keys stay as history, under no project."},
		},
		examples: []string{
			`keera project create payments --description "The payments platform"`,
			`keera project set payments --name payments-platform`,
		},
	},
	{
		name:    "user",
		aliases: []string{"users"},
		summary: "people, and what they may do",
		prose: "A person is added so that a key can be attributed to them, which is what puts " +
			"a name on spend and in the audit log. Where a directory group decides the role, " +
			"'user role' is refused: the next sign-in would undo it.",
		subs: []subcommand{
			{name: "add", aliases: []string{"create", "invite", "new"}, args: "<email>", summary: "add a person",
				flags: []string{"org", "role", "external-id", "passkey", "json"},
				prose: "With --passkey, they sign in with a passkey instead of an identity " +
					"provider, and the command prints a set-up link to send them. This needs " +
					"KEERA_PASSKEYS on the gateway. An administrator can only use their " +
					"organisation's email domain."},
			{name: "list", aliases: []string{"ls"}, summary: "everyone in the organisation", flags: []string{"org", "json"}},
			{name: "role", aliases: []string{"set-role"}, args: "<email-or-id> <member|admin>",
				summary: "change what a person may do; signs them out",
				flags:   []string{"org", "json"}},
			{name: "disable", aliases: []string{"offboard"}, args: "<email-or-id>",
				summary: "turn a person off, for when they leave",
				flags:   []string{"org", "yes", "json"},
				prose: "They are signed out and their keys revoked; leaving the directory alone " +
					"does not revoke keys. See docs/sso.md."},
			{name: "enable", args: "<email-or-id>", summary: "let a disabled person sign in again",
				flags: []string{"org", "json"},
				prose: "Their old keys stay revoked, so they start with none. Their passkeys " +
					"are gone too: send them a new set-up link."},
			{name: "passkey-link", aliases: []string{"passkey"}, args: "<email-or-id>",
				summary: "a new set-up link, to add a passkey",
				flags:   []string{"org", "json"},
				prose: "For a new device, or after a lost passkey. It works once, for 24 hours, " +
					"and replaces any earlier link. Someone who has not signed in yet becomes " +
					"a passkey account. A directory account is refused: leaving the directory " +
					"has to lock them out."},
		},
	},
	{
		name:    "key",
		aliases: []string{"keys"},
		summary: "API keys, and who they belong to",
		prose: "The budget, the rate limit and every line of the audit trail are attributed to " +
			"the key that made the call. --user attributes it to a person as well, which is " +
			"the only thing that makes 'keera usage --by user' say anything.\n\n" +
			"A key is shown once, when it is created. Nothing can print it again.",
		subs: []subcommand{
			{name: "create", aliases: []string{"add", "new"}, args: "[<name>]", summary: "issue an API key",
				flags: []string{"org", "project", "user", "name", "expires", "subscription", "json"},
				prose: "The name says what the key is for. The panel, the reports and the audit " +
					"log show it in place of the key, so a key without one is a row nobody " +
					"can identify later.\n\n" +
					"--subscription issues a key for Claude Code signed in to a Claude plan. It " +
					"reaches only subscription models, which that plan pays for. For a member, " +
					"issue one with --user; their 'keera connect claude-code --subscription' " +
					"then takes it over for their machine.\n\n" +
					"A new key lasts 90 days unless --expires says otherwise; --expires never " +
					"makes one that does not expire.\n\n" +
					"Only an administrator issues keys. A member renames, rotates and revokes " +
					"their own.",
				examples: []string{
					`keera key create --project payments --user ada@example.ch --name "Ada's laptop"`,
				}},
			{name: "list", aliases: []string{"ls"}, summary: "every key", flags: []string{"org", "project", "json"}},
			{name: "set", aliases: []string{"edit", "update", "rename"}, args: "<key>",
				summary: "rename a key",
				flags:   []string{"org", "name", "json"},
				prose: "The name is only a label. The key, its guardrails and its usage stay " +
					"as they are. The old name stays in the audit log. A member may rename " +
					"their own keys; an administrator any key in the organisation.",
				examples: []string{`keera key set "Ada's laptop" --name "Ada's old laptop"`}},
			{name: "revoke", aliases: []string{"delete", "rm", "remove"}, args: "<key>", summary: "stop a key working",
				flags: []string{"org", "yes", "json"},
				prose: "A name finds the working key with that name in this organisation. " +
					"Where more than one has it, the ids are listed rather than one of them " +
					"picked. An id out of 'keera key list' names a key outright, and with " +
					"--yes needs nothing else.\n\n" +
					"Revoking is immediate and cannot be undone: whatever holds the key is " +
					"refused from its next call, and nothing can print it again. To replace " +
					"a key without an outage, rotate it instead.",
			},
			{name: "rotate", args: "<key>",
				summary: "replace a key with an identical one and revoke it",
				flags:   []string{"org", "name", "expires", "yes", "json"},
				prose: "For a key that has leaked, and for the ordinary rotation a policy asks " +
					"for. A name finds the working key with that name; an id out of " +
					"'keera key list' names a key outright. --name and --expires override " +
					"what it had; everything else, including its project and its guardrails, " +
					"is carried over. A member may rotate their own keys, but not choose " +
					"--expires: the new key keeps the old one's lifetime.\n\n" +
					"The old key stops working at once, so it asks for the key's name first; " +
					"--yes skips that, for a script.",
			},
		},
	},
	{
		name:    "model",
		aliases: []string{"models"},
		summary: "the models clients name: your organisation's",
		prose: "Clients name an alias like 'keera-speed', never a backend model id and never an " +
			"inference URL. That is what lets the model behind an alias be swapped without any " +
			"developer changing anything.\n\n" +
			"Models belong to an organisation, and only it can call them. Its administrators " +
			"add, change and remove them. Only an operator with several organisations " +
			"needs --org.\n\n" +
			"A new organisation starts with a copy of each model in the catalogue file " +
			"(KEERA_MODELS_FILE). The copies are its own, to change or remove like any " +
			"other. 'keera model apply' adds the models of a file to an organisation that " +
			"already exists.\n\n" +
			"--description is a sentence saying what a model is for. Clients see it on " +
			"/v1/models, and it is what a router is told about the model when the router is " +
			"choosing between destinations - so a router whose models have no descriptions is " +
			"choosing between bare aliases.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "the models you can use", flags: []string{"org", "json"}},
			{name: "add", aliases: []string{"create", "new"}, args: "<alias>", summary: "add a model",
				flags: []string{"org", "provider", "backend", "product-id", "backend-model", "kind",
					"description", "max-context", "release-date", "location",
					"price-in", "price-out", "price-cached", "price-cache-write",
					"api-key", "subscription", "disabled", "json"},
				prose: "--subscription makes each caller's own Claude subscription pay. The " +
					"gateway then forwards the caller's Claude sign-in and stores no API key, " +
					"and only Claude Code signed in to a Claude plan can use the model. See " +
					"docs/subscriptions.md.",
				examples: []string{
					"keera model add keera-speed --backend http://vllm:8000/v1 \\\n" +
						"    --backend-model Qwen/Qwen2.5-Coder-7B-Instruct --max-context 32768",
					"keera model add keera-swiss --provider infomaniak --product-id 100234 \\\n" +
						"    --backend-model swiss-ai/Apertus-v1.5-70B --api-key @-",
					"keera model add claude-opus --provider anthropic \\\n" +
						"    --backend-model claude-opus-5-5 --subscription",
				}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<alias>", summary: "change any of those on an existing model",
				flags: []string{"org", "provider", "backend", "product-id", "backend-model", "kind",
					"description", "max-context", "release-date", "location",
					"price-in", "price-out", "price-cached", "price-cache-write",
					"api-key", "no-api-key", "subscription", "no-subscription", "json"}},
			{name: "enable", args: "<alias>", summary: "serve this model", flags: []string{"org", "json"}},
			{name: "disable", args: "<alias>", summary: "stop serving it, keeping its declaration",
				flags: []string{"org", "json"}},
			{name: "check", aliases: []string{"probe", "test"}, args: "<alias>", summary: "probe the live backend end to end",
				flags: []string{"org", "json"},
				prose: "The test for the failure that quietly breaks coding agents: a backend " +
					"answering 200 with prose in 'content' where a tool call was asked for, " +
					"which is what a vLLM parser that does not match its model produces.",
			},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<alias>", summary: "remove a model", flags: []string{"org", "yes", "json"}},
			{name: "apply", args: "<file>", summary: "add or update every model a catalogue file declares",
				flags: []string{"org", "json"}},
			{name: "validate", aliases: []string{"lint"}, args: "<file>", summary: "validate a catalogue file without a server",
				flags: []string{"json"}},
			{name: "providers", summary: "hosted endpoints a model can name, and their models",
				flags: []string{"json"}},
		},
	},
	{
		name:    "mcp",
		summary: "the MCP servers agents call tools on, and their calls",
		prose: "The gateway stands in front of MCP servers as it does in front of models. A " +
			"client reaches one at <gateway>/api/mcp/<alias> with its Keera key; the gateway " +
			"holds the server's own credential, shows the client only the tools its " +
			"guardrail allows, runs its filters over what a call sends, and records every " +
			"call without its content.\n\n" +
			"Which tools a scope may call is a guardrail: 'keera guardrail set project project_1 " +
			"--tools github/search_code,jira'. A server's alias allows all its tools; " +
			"alias/tool allows one.\n\n" +
			"A server belongs to one organisation, and only its keys reach it. Its " +
			"administrators add, change and remove it. Only an operator with several " +
			"organisations needs --org.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "the MCP servers", flags: []string{"org", "json"}},
			{name: "add", aliases: []string{"create", "new"}, args: "<alias>", summary: "add an MCP server",
				flags: []string{"org", "endpoint", "description", "auth-header", "api-key",
					"disabled", "json"},
				examples: []string{
					"keera mcp add github --endpoint https://api.githubcopilot.com/mcp/ \\\n" +
						"    --description \"Issues and pull requests\" --api-key @-",
				}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<alias>",
				summary: "change any of those on an existing server",
				flags: []string{"org", "endpoint", "description", "auth-header", "api-key",
					"no-api-key", "json"}},
			{name: "enable", args: "<alias>", summary: "serve this server", flags: []string{"org", "json"}},
			{name: "disable", args: "<alias>", summary: "stop serving it, keeping its declaration",
				flags: []string{"org", "json"}},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<alias>", summary: "remove a server",
				flags: []string{"org", "yes", "json"}},
			{name: "calls", summary: "the tool-call log: which tool, what came of it, how much went each way",
				flags:    []string{"org", "server", "tool", "project", "key", "user", "since", "limit", "summary", "json"},
				examples: []string{"keera mcp calls --since 1h", "keera mcp calls --summary --since 168h"}},
			{name: "connect", args: "<alias>", summary: "how to point Claude Code, Codex and others at a server",
				flags: []string{"org"}},
		},
	},
	{
		name:    "guardrail",
		aliases: []string{"guardrails"},
		summary: "what a scope may do: models, rates, spend, machines",
		prose: "A guardrail is what one scope may do: which models it may reach, how fast, how " +
			"much it may spend, what it passes through on the way and how much machine it may " +
			"hold.\n\n" +
			"They nest: an organisation's is the ceiling a project's fits inside, and a project's " +
			"is the ceiling a key's fits inside. So a scope that sets nothing is not " +
			"unlimited - it is whatever holds it. 'guardrail effective' says which level " +
			"each number came from, and is the thing to read before changing one.\n\n" +
			"'keera limit' and 'keera budget' set one part of a guardrail each.\n\n" +
			"For an organisation the id may be left out: it is then --org, or your own. A " +
			"project or key named by its name is looked up in that organisation too.",
		subs: []subcommand{
			{name: "get", aliases: []string{"show"}, args: "<scope> [<id>]", summary: "what this one scope sets; scope is org, project or key",
				flags: []string{"org", "json"}},
			{name: "effective", args: "<scope> [<id>]",
				summary: "what a request actually meets, and which level decided each part",
				flags:   []string{"org", "json"},
				prose: "The chain collapsed the way the gateway collapses it, with the level " +
					"that decided each value named beside it.\n\n" +
					"Allow-lists intersect; the output-token ceiling takes the minimum. Rate " +
					"limits and budgets are not merged: each level is checked on its own. " +
					"System prompts and filters add up, outermost first.",
				examples: []string{"keera guardrail effective key key_06g9…"},
			},
			{name: "set", aliases: []string{"edit", "update"}, args: "<scope> [<id>]", summary: "set any of them on a scope",
				flags: []string{"org", "models", "rpm", "tpm", "max-output-tokens", "budget", "period",
					"system-prompt", "no-system-prompt", "filters", "no-filters", "tools",
					"block-hosted-tools", "allow-hosted-tools", "max-sandboxes",
					"max-sandbox-ttl", "sandbox-classes", "max-sandbox-cpu", "max-sandbox-memory", "repos", "json"},
				examples: []string{
					"keera guardrail set project project_1 --models keera-speed --rpm 120 \\\n" +
						"    --budget 500 --period month",
				}},
		},
	},
	{
		name:    "limit",
		aliases: []string{"limits"},
		summary: "a scope's rate limits, on their own",
		args:    "<scope> [<id>] [flags]",
		flags:   []string{"org", "rpm", "tpm", "max-output-tokens", "json"},
		prose: "The rate-limiting part of 'guardrail set', and nothing else. Same object, " +
			"same scopes, same nesting - fewer flags to read.\n\n" +
			"With no flags it prints what is in force, and which level set it.",
		examples: []string{
			"keera limit project project_1 --rpm 120",
			"keera limit key key_06g9…",
		},
	},
	{
		name:    "budget",
		aliases: []string{"budgets"},
		summary: "a scope's budget, on its own",
		args:    "<scope> [<id>] [flags]",
		flags:   []string{"org", "budget", "period", "json"},
		prose: "The spending part of 'guardrail set', and nothing else. Spend past a budget " +
			"is refused with 402, not 429: a client reading it as \"slow down\" would retry " +
			"against a limit that only moves when the period rolls over.\n\n" +
			"Budgets do not merge. An organisation's and a project's are two limits, and both " +
			"have to hold. With no flags this prints every budget above the scope.",
		examples: []string{
			"keera budget project project_1 --budget 500 --period month",
			"keera budget org",
		},
	},
	{
		name:    "filter",
		aliases: []string{"filters"},
		summary: "what a request may contain, before it is forwarded",
		prose: "A filter reads each request a guardrail applies it to, before it is forwarded. " +
			"--mode rewrite (the default) and gate use a small model to take data out or refuse " +
			"the request; --mode pattern applies --rules in the gateway and costs nothing. " +
			"See docs/filters.md.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "this organisation's filters", flags: []string{"org", "json"}},
			{name: "add", aliases: []string{"create", "new"}, args: "<alias>", summary: "add a filter",
				flags: []string{"org", "mode", "model", "prompt", "rules", "description",
					"shadow", "enforce", "json"},
				prose: "A pattern filter's --rules are one rule per line: an expression, '=>', " +
					"then what each match becomes - or 'REFUSE: why' to drop the request. " +
					"@path reads them from a file.\n\n" +
					"  (?i)\\bsk-[a-z0-9]{20,}\\b         => [CREDENTIAL]\n" +
					"  \\b[A-Z]{2}\\d{2}[A-Z0-9]{10,28}\\b => [IBAN]\n" +
					"  (?i)\\bexport all customers\\b    => REFUSE: that moves the customer list out\n\n" +
					"With --shadow the filter runs and enforces nothing: the request is " +
					"forwarded as it was sent whatever the filter answered, and 'report' says " +
					"what it would have done. That is how an instruction is tuned against a " +
					"week of an organisation's own traffic rather than against three sample " +
					"segments. --enforce turns it back on; the answers do not change, only " +
					"whether they are acted on.",
				examples: []string{
					"keera filter add redact-keys --mode pattern --rules @redact.rules",
					"keera filter add no-client-data --mode gate --model keera-speed \\\n" +
						"    --prompt 'Refuse anything naming a client.' --shadow",
				}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<alias>", summary: "change any of those on an existing filter",
				flags: []string{"org", "mode", "model", "prompt", "rules", "description",
					"shadow", "enforce", "json"}},
			{name: "check", aliases: []string{"probe", "test"}, args: "<alias>", summary: "run it over a sample and show what it did",
				flags: []string{"org", "json"}},
			{name: "report", aliases: []string{"stats"}, args: "<alias>", summary: "what it has done to real traffic",
				flags: []string{"org", "since", "json"}},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<alias>", summary: "remove a filter",
				flags: []string{"org", "yes", "json"}},
		},
	},
	{
		name:    "router",
		aliases: []string{"routers"},
		summary: "which model answers a request",
		prose: "A router chooses which model answers a request that names it. The modes are " +
			"instruction (the default, a small model reads the request), size, fallback, " +
			"latency and least-busy. See docs/routers.md.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "this organisation's routers", flags: []string{"org", "json"}},
			{name: "add", aliases: []string{"create", "new"}, args: "<alias>", summary: "add a router",
				flags: []string{"org", "mode", "destinations", "description", "model", "prompt",
					"fallback", "no-fallback", "json"},
				prose: "With --fallback a request the router cannot place goes to a named " +
					"destination anyway; with --no-fallback it is refused. Neither is the " +
					"right answer in general - a router that saves money has somewhere safe " +
					"to fall back to, and a router that keeps prompts inside the cluster " +
					"does not.",
				examples: []string{
					"keera router add ha --mode fallback --destinations keera-local,hosted-frontier",
					"keera router add pool --mode least-busy --destinations llama-a,llama-b",
					"keera router add bysize --mode size --destinations keera-speed:4k,keera-frontier",
				}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<alias>", summary: "change any of those on an existing router",
				flags: []string{"org", "mode", "destinations", "description", "model", "prompt",
					"fallback", "no-fallback", "json"}},
			{name: "check", aliases: []string{"probe", "test"}, args: "<alias>", summary: "put sample prompts through it and show where each went",
				flags: []string{"org", "json"},
				prose: "On a router that reads nothing, it asks each destination in turn " +
					"whether it is there and in what order it would be tried.",
			},
			{name: "report", aliases: []string{"stats"}, args: "<alias>", summary: "where it has sent real traffic",
				flags: []string{"org", "since", "json"}},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<alias>", summary: "remove a router",
				flags: []string{"org", "yes", "json"}},
		},
	},
	{
		name:    "sandbox",
		aliases: []string{"sandboxes", "sbx"},
		summary: "the machines this gateway lends out",
		prose: "A sandbox is a machine for one task or one working day, with its own API key " +
			"and an expiry. 'sandbox ssh' attaches through the gateway's one port, and " +
			"'sandbox agent' starts one task's machine that ends by pushing a branch. " +
			"See docs/sandboxes.md.",
		subs: []subcommand{
			{name: "classes", summary: "the machines this organisation offers", flags: []string{"org", "json"}},
			{name: "apply", args: "<file>", summary: "add or update every class a catalogue file declares",
				flags: []string{"org", "json"}},
			{name: "delete-class", aliases: []string{"remove-class", "rm-class"}, args: "<name>",
				summary: "remove a class; sandboxes already running on it keep working",
				flags:   []string{"org", "yes", "json"}},
			{name: "create", aliases: []string{"add", "up", "new"}, args: "<name>", summary: "start one",
				flags: []string{"org", "project", "class", "purpose", "ttl", "repo", "branch",
					"task", "ssh-key", "json"}},
			{name: "agent", args: "<name>", summary: "start one for an agent",
				flags: []string{"org", "project", "class", "ttl", "repo", "branch", "task", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "what is running",
				flags: []string{"org", "project", "class", "purpose", "all", "json"}},
			{name: "show", aliases: []string{"get"}, args: "<name>", summary: "one sandbox in full", flags: []string{"org", "json"}},
			{name: "ssh", args: "<name> [-- <command>]", summary: "open a shell in it",
				flags: []string{"org"}},
			{name: "config", summary: "the ~/.ssh/config block, so any editor can attach",
				flags: []string{"org"}},
			{name: "proxy", args: "<name>", summary: "the attach surface, for ssh's ProxyCommand",
				flags: []string{"org", "port"}},
			{name: "extend", args: "<name>", summary: "push its expiry out again",
				flags: []string{"org", "ttl", "json"}},
			{name: "suspend", args: "<name>", summary: "release its compute, keep its disk",
				flags: []string{"org", "json"}},
			{name: "resume", args: "<name>", summary: "start it again where it left off",
				flags: []string{"org", "json"},
				prose: "A suspended sandbox starts again as it was. An expired one also gets a " +
					"new API key, a new repository credential and the class's default lifetime, " +
					"because its old ones were revoked when it expired."},
			{name: "terminate", aliases: []string{"delete", "rm", "remove", "down"}, args: "<name>", summary: "end it and take its volume with it",
				flags: []string{"org", "yes", "json"}},
			{name: "usage", summary: "what sandboxes have cost",
				flags: []string{"org", "by", "since", "json"}},
		},
	},
	{
		name:    "usage",
		summary: "usage and cost, grouped however you ask",
		args:    "[flags]",
		flags:   []string{"by", "since", "org", "json"},
		examples: []string{
			"keera usage --by project",
			"keera usage --by user --since 720h",
		},
	},
	{
		name:    "billing",
		summary: "the bill and the credit for the deployment's own provider keys",
		prose: "Calls to models on a provider key the deployment holds are billed at the " +
			"provider's list price. On a deployment that takes payments, organisations pay " +
			"for them in advance, in CHF, by card through PostFinance Checkout. Without " +
			"credit, such models are refused; the organisation's own models still work. " +
			"See docs/billing.md.",
		subs: []subcommand{
			{name: "report", aliases: []string{"list", "ls"}, summary: "one month's bill",
				flags: []string{"month", "org", "json"},
				prose: "Each currency is summed on its own; nothing is converted. Operators " +
					"also see what the provider charges and the margin. 'keera billing' on " +
					"its own is this.",
				examples: []string{"keera billing", "keera billing --month 2026-09 --json"}},
			{name: "credit", aliases: []string{"balance", "account"},
				summary: "the balance, the saved card, the automatic top-up and the latest payments",
				flags:   []string{"org", "json"}},
			{name: "topup", aliases: []string{"pay"},
				summary: "pay for credit: prints the payment page to open",
				flags:   []string{"amount", "save-card", "org", "json"},
				prose: "VAT is added on top, if the deployment charges it. The credit is added " +
					"once PostFinance says the payment went through. --save-card keeps the card " +
					"for automatic top-ups, and replaces a card saved before.",
				examples: []string{"keera billing topup --amount 100 --save-card"}},
			{name: "auto-topup", summary: "charge the saved card when the balance runs low",
				flags:    []string{"below", "amount", "off", "org", "json"},
				examples: []string{"keera billing auto-topup --below 50 --amount 200", "keera billing auto-topup --off"},
				prose: "When a charge fails, no other is tried until the setting is saved " +
					"again or someone tops up by hand."},
			{name: "forget-card", aliases: []string{"remove-card"},
				summary: "remove the saved card, which stops the automatic top-up",
				flags:   []string{"org"}},
			{name: "payments", summary: "the payments, newest first",
				flags: []string{"limit", "org", "json"}},
			{name: "grant", summary: "add credit without a payment, or take some away (operators)",
				flags: []string{"amount", "note", "org", "json"},
				prose: "For a trial, a refund, or money that came by bank transfer. A negative " +
					"--amount takes credit away. --note is required: it says why, and is shown to " +
					"the organisation.",
				examples: []string{`keera billing grant --org org_123 --amount 50 --note "trial"`}},
			{name: "invoiced", args: "<on|off>",
				summary: "let an organisation use the keys without credit (operators)",
				flags:   []string{"org", "json"},
				prose:   "For an organisation billed by invoice instead. Its use is still metered."},
		},
	},
	{
		name:    "failures",
		aliases: []string{"failure"},
		summary: "the calls that did not deliver, and what the backend said",
		args:    "[flags]",
		flags:   []string{"kind", "model", "project", "key", "user", "status", "since", "limit", "org", "json"},
		prose: "Each row shows the backend's own message beside the model and the key. The " +
			"status says a call failed; the message says whose problem it is.",
	},
	{
		name:    "session",
		aliases: []string{"sessions"},
		summary: "what agents have been carrying out, one task at a time",
		prose: "A session is the requests one coding agent made working through one task. Nobody " +
			"decides how many requests a task takes, so a cost per request is a number nobody " +
			"can act on and a cost per task is the one that goes into a budget conversation.\n\n" +
			"The grouping is computed when the report is read, from a hash of the conversation " +
			"the gateway records on each request - no prompt is stored - and cut wherever the " +
			"agent went quiet for longer than KEERA_SESSION_GAP.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "one row per task, the costliest first",
				flags: []string{"sort", "unhappy", "model", "project", "key", "user", "since", "limit",
					"org", "json"},
				prose: "The first column is the id of the task's first request. 'keera session " +
					"show' takes it. --user takes an email or an id."},
			{name: "show", aliases: []string{"get"}, args: "<request-id>",
				summary: "one task from beginning to end, named by any request in it",
				flags:   []string{"org", "json"},
				prose: "The id is any request in the task: the first column of 'keera session " +
					"list' or 'keera failures'. 'keera session <id>' is short for it."},
		},
	},
	{
		name:    "connect",
		summary: "the finished configuration for an editor",
		args:    "[<client>] [flags]",
		flags:   []string{"key", "org", "subscription", "model", "project", "json"},
		prose: "Prints an editor's configuration on stdout and the instructions on stderr, so " +
			"it can be redirected into the file it names. With no client it lists the ones " +
			"this deployment can configure. --subscription sets up Claude Code on a Claude " +
			"plan; see docs/subscriptions.md.",
		examples: []string{
			"keera connect",
			"keera connect opencode --key laptop > ~/.config/opencode/opencode.json",
			"keera connect claude-code --subscription",
		},
	},
	{name: "version", summary: "the build version this command line reports"},
	{name: "help", summary: "help for one command", args: "[<command> [<subcommand>]]", hidden: true},
}

// find resolves a name, including a plural or a habit spelling, to its command.
func find(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name || slices.Contains(c.aliases, name) {
			return c, true
		}
	}
	return command{}, false
}

func (c command) sub(name string) (subcommand, bool) {
	for _, s := range c.subs {
		if s.name == name || slices.Contains(s.aliases, name) {
			return s, true
		}
	}
	return subcommand{}, false
}

// overview is what `keera` on its own prints: one line per command, short
// enough to read without scrolling. p is the painter of the stream it goes to.
func overview(p painter) string {
	var b strings.Builder
	b.WriteString(p.head(tagline) + "\n\n" + p.head("Usage:") +
		"\n  keera <command> [subcommand] [flags]\n\n" + p.head("Commands:") + "\n")
	width := 0
	for _, c := range commands {
		if !c.hidden && len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range commands {
		if c.hidden {
			continue
		}
		fmt.Fprintf(&b, "  %s  %s\n", padTo(p.cmd(c.name), width), c.summary)
	}
	base, from := resolveBase(defaultGateway())
	b.WriteString("\nRun 'keera help <command>', or 'keera help <command> <subcommand>'.\n")
	b.WriteString("Every listing command also takes --json, and every command --color=never\n" +
		"(or --no-color).\n")
	// Which deployment the commands act on cannot be seen anywhere else.
	fmt.Fprintf(&b, "\nThese commands talk to %s (%s).\n", p.cmd(base), from)
	b.WriteString("Point them elsewhere with --url, or sign in: keera login --url <your deployment>\n")
	return b.String()
}

// helpText renders one command, or one of its subcommands. fs is the
// FlagSet the command parses with, or nil for a command with no flags.
func helpText(fs *flag.FlagSet, name, subName string) string {
	c, ok := find(name)
	if !ok {
		return overview(style)
	}
	if sub, found := c.sub(subName); found {
		return subHelpText(fs, c, sub)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s - %s\n", style.head("keera "+c.name), c.summary)
	if len(c.subs) > 0 {
		writeSubList(&b, c)
	} else {
		fmt.Fprintf(&b, "\n%s\n  keera %s", style.head("Usage:"), c.name)
		if c.args != "" {
			fmt.Fprintf(&b, " %s", c.args)
		}
		b.WriteString("\n")
	}
	writeProse(&b, c.prose)

	// With subcommands, each verb lists its own flags. A flag no verb claims
	// is still printed here, so it can always be found.
	if len(c.subs) == 0 {
		writeFlags(&b, fs, c.flags)
	} else if extra := unclaimed(fs, c); len(extra) > 0 {
		b.WriteString("\n" + style.head("Other flags:") + "\n")
		writeFlagLines(&b, fs, extra)
	}
	writeExamples(&b, c.examples)
	if len(c.subs) > 0 {
		fmt.Fprintf(&b, "\nFlags for one of them: %s\n",
			style.cmd("keera help "+c.name+" "+c.subs[0].name))
	}
	return b.String()
}

// subHelpText renders one subcommand of c.
func subHelpText(fs *flag.FlagSet, c command, sub subcommand) string {
	var b strings.Builder
	title := "keera " + c.name + " " + sub.name
	if sub.args != "" {
		title += " " + sub.args
	}
	fmt.Fprintf(&b, "%s - %s\n", style.head(title), sub.summary)
	writeProse(&b, sub.prose)
	writeFlags(&b, fs, sub.flags)
	writeExamples(&b, sub.examples)
	// The command's prose explains the idea, so point back at it.
	fmt.Fprintf(&b, "\nWhat a %s is: %s\n", c.name, style.cmd("keera help "+c.name))
	return b.String()
}

// writeSubList writes a command's verbs, one per line.
func writeSubList(b *strings.Builder, c command) {
	b.WriteString("\n" + style.head("Usage:") + "\n")
	width := 0
	for _, s := range c.subs {
		if n := len(s.name) + len(s.args) + 1; n > width {
			width = n
		}
	}
	for _, s := range c.subs {
		line := s.name
		if s.args != "" {
			line += " " + s.args
		}
		// Painted first and padded after, so escapes stay out of the widths.
		fmt.Fprintf(b, "  %s  %s\n",
			padTo(style.cmd("keera "+c.name+" "+line), len("keera ")+len(c.name)+1+width),
			s.summary)
	}
	if also := subAliases(c); len(also) > 0 {
		fmt.Fprintf(b, "\nAlso accepted: %s\n", strings.Join(also, ", "))
	}
}

// unclaimed is every flag the FlagSet declares that no subcommand in the
// registry names. It should always be empty; help_test says so.
func unclaimed(fs *flag.FlagSet, c command) []string {
	if fs == nil {
		return nil
	}
	claimed := map[string]bool{}
	for _, s := range c.subs {
		for _, name := range s.flags {
			claimed[name] = true
		}
	}
	var out []string
	fs.VisitAll(func(f *flag.Flag) {
		if !claimed[f.Name] {
			out = append(out, f.Name)
		}
	})
	sort.Strings(out)
	return out
}

func writeProse(b *strings.Builder, prose string) {
	if prose == "" {
		return
	}
	b.WriteString("\n")
	for para := range strings.SplitSeq(prose, "\n\n") {
		// A paragraph with its own line breaks, such as the table of router
		// modes, is laid out by hand: indent it, do not rewrap it.
		if strings.Contains(para, "\n") {
			b.WriteString(indent(para, "  ") + "\n\n")
			continue
		}
		b.WriteString("  " + wrapAt(para, 2, 2, proseWidth) + "\n\n")
	}
	// The caller's next section opens with its own blank line.
	trimTrailingBlank(b)
}

// proseWidth is the width help and reports are laid out for.
const proseWidth = 78

// wrapAt breaks text to width. The first line starts at column first,
// because something is already printed in front of it, and every later line
// is indented to column indent. It counts runes, not bytes, because the prose
// holds em-dashes.
func wrapAt(text string, first, indent, width int) string {
	var b strings.Builder
	column := first
	for i, word := range strings.Fields(text) {
		n := len([]rune(word))
		switch {
		case i == 0:
			column = first + n
		case column+1+n > width:
			b.WriteString("\n" + strings.Repeat(" ", indent))
			column = indent + n
		default:
			b.WriteString(" ")
			column += 1 + n
		}
		b.WriteString(word)
	}
	return b.String()
}

func writeFlags(b *strings.Builder, fs *flag.FlagSet, names []string) {
	if fs == nil || len(names) == 0 {
		return
	}
	var present []string
	for _, n := range names {
		if fs.Lookup(n) != nil {
			present = append(present, n)
		}
	}
	if len(present) == 0 {
		return
	}
	b.WriteString("\n" + style.head("Flags:") + "\n")
	writeFlagLines(b, fs, present)
}

// writeFlagLines prints each flag with two dashes, its value's placeholder
// and its description, wrapped to the column it starts in.
func writeFlagLines(b *strings.Builder, fs *flag.FlagSet, names []string) {
	type row struct{ left, usage string }
	rows := make([]row, 0, len(names))
	width := 0
	for _, n := range names {
		f := fs.Lookup(n)
		if f == nil {
			continue
		}
		name, usage := flag.UnquoteUsage(f)
		left := "--" + f.Name
		if name != "" {
			left += " " + name
		}
		if shownDefault(f.DefValue) {
			usage += " (default " + f.DefValue + ")"
		}
		if len(left) > width {
			width = len(left)
		}
		rows = append(rows, row{left, usage})
	}
	for _, r := range rows {
		fmt.Fprintf(b, "  %s  %s\n", padTo(style.cmd(r.left), width),
			wrapAt(r.usage, width+4, width+4, proseWidth))
	}
}

// shownDefault reports whether a flag's default is worth printing. Empty,
// false, zero and negative values mean "not given" here, whether a number or a
// duration, so printing "(default -1ns)" would only mislead.
func shownDefault(def string) bool {
	if def == "" || def == "false" {
		return false
	}
	if n, err := strconv.ParseFloat(def, 64); err == nil {
		return n > 0
	}
	if d, err := time.ParseDuration(def); err == nil {
		return d > 0
	}
	return true
}

func writeExamples(b *strings.Builder, examples []string) {
	if len(examples) == 0 {
		return
	}
	b.WriteString("\n" + style.head("Examples:") + "\n")
	for _, e := range examples {
		for line := range strings.SplitSeq(e, "\n") {
			if line == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("  " + example(line) + "\n")
		}
	}
}

// example paints a line to copy: the command in one colour, and a trailing
// # comment, which is not typed, in another.
func example(line string) string {
	i := strings.Index(line, "#")
	if i < 0 {
		return style.cmd(line)
	}
	code := strings.TrimRight(line[:i], " ")
	return style.cmd(code) + line[len(code):i] + style.muted(line[i:])
}

func indent(text, with string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = with + l
		}
	}
	return strings.Join(lines, "\n")
}

func trimTrailingBlank(b *strings.Builder) {
	s := strings.TrimRight(b.String(), "\n")
	b.Reset()
	b.WriteString(s + "\n")
}

// printHelp writes one command's help to stdout. It returns an error so a
// command can end with `return printHelp(...)`.
func printHelp(fs *flag.FlagSet, name, sub string) error {
	fmt.Print(helpText(fs, name, sub))
	return nil
}

// wantsHelp spots a request for help in any of the ways people write it:
// `keera filter --help`, `keera filter help`, `keera filter add --help` or
// `keera filter help add`. It returns the subcommand asked about, empty for
// the command itself, and false when this is not a request for help.
//
// The word "help" counts only where a verb goes, so a project or a prompt can be
// called "help". It stops at "--", because what follows belongs to another
// program, as in `keera sandbox ssh box -- git --help`.
func wantsHelp(args []string) (string, bool) {
	asked := false
	var named []string
	for i, a := range args {
		if a == "--" {
			break
		}
		switch {
		case a == "-h" || a == "--help" || a == "-help" || (a == "help" && i == 0):
			asked = true
		case !strings.HasPrefix(a, "-"):
			named = append(named, a)
		}
	}
	if !asked {
		return "", false
	}
	if len(named) > 0 {
		return named[0], true
	}
	return "", true
}

// suggest is the command whose name is nearest to what was typed. Empty when
// nothing is near enough, because a wild guess does not help.
func suggest(typed string) string {
	best, bestDistance := "", 3
	for _, c := range commands {
		if c.hidden {
			continue
		}
		for _, name := range append([]string{c.name}, c.aliases...) {
			if d := distance(typed, name); d < bestDistance {
				best, bestDistance = c.name, d
			}
		}
	}
	return best
}

// suggestSub is the same thing one level down, for `keera filter lst`.
func suggestSub(name, typed string) string {
	c, ok := find(name)
	if !ok {
		return ""
	}
	best, bestDistance := "", 3
	for _, s := range c.subs {
		// A near miss on an alias suggests the verb itself.
		for _, spelling := range append([]string{s.name}, s.aliases...) {
			if d := distance(typed, spelling); d < bestDistance {
				best, bestDistance = s.name, d
			}
		}
	}
	return best
}

// distance is the Levenshtein distance: enough to catch a missing, doubled
// or swapped letter.
func distance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		copy(prev, cur)
	}
	return prev[len(b)]
}

// subAliases is every other spelling this command's verbs answer to, as
// "verb (alias, alias)", printed under the list of verbs.
func subAliases(c command) []string {
	out := make([]string, 0, len(c.subs))
	for _, s := range c.subs {
		if len(s.aliases) > 0 {
			out = append(out, s.name+" ("+strings.Join(s.aliases, ", ")+")")
		}
	}
	return out
}

// unknownSub is what a command says to a verb it does not have: the nearest
// verb, or else the list of verbs.
func unknownSub(name, typed string) error {
	c, ok := find(name)
	if !ok {
		return fmt.Errorf("unknown command: keera %s", name)
	}
	// No verb at all is a question about the command, so print its help.
	// Commands with a listing never get here.
	if typed == "" {
		fmt.Print(helpText(nil, name, ""))
		return fmt.Errorf("%s needs a subcommand", name)
	}
	verbs := make([]string, 0, len(c.subs))
	for _, s := range c.subs {
		verbs = append(verbs, s.name)
	}
	msg := fmt.Sprintf("keera %s has no subcommand %q", name, typed)
	if near := suggestSub(name, typed); near != "" {
		msg += fmt.Sprintf("; did you mean 'keera %s %s'?", name, near)
	} else if len(verbs) > 0 {
		msg += fmt.Sprintf("; it has: %s", strings.Join(verbs, ", "))
	}
	return fmt.Errorf("%s\nRun 'keera help %s'", msg, name)
}

// helpCmd is `keera help [<command> [<subcommand>]]`. A command's flags live
// on the FlagSet it builds, so this runs the command with --help.
func helpCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(overview(style))
		return nil
	}
	name := args[0]
	if _, ok := find(name); !ok {
		if near := suggest(name); near != "" {
			return fmt.Errorf("no command %q; did you mean 'keera %s'?", name, near)
		}
		fmt.Fprint(os.Stderr, overview(styleErr))
		return fmt.Errorf("no command %q", name)
	}
	rest := append(slices.Clone(args[1:]), "--help")
	return Run(ctx, append([]string{name}, rest...))
}
