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
	"strings"
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
		summary: "sign in to a deployment through its identity provider",
		args:    "[flags]",
		flags:   []string{"provider", "no-browser"},
		prose: "Opens a browser, signs you in the way the panel does, and keeps what comes " +
			"back in ~/.config/keera/credentials.json. Commands then run as you: your role " +
			"decides what they may do, and the audit log records your address rather than " +
			"\"operator key\". The sign-in lasts a month and ends sooner with 'keera logout'.\n\n" +
			"--url makes this the only line a developer needs: the gateway signed in to " +
			"becomes the one every later command talks to, with nothing to export into a " +
			"shell profile. Without it the sign-in goes to https://gateway.keera.ch, the " +
			"hosted deployment. 'keera whoami' says which gateway is in hand.\n\n" +
			"KEERA_OPERATOR_KEY still works and still wins where it is set. It is the " +
			"deployment's own credential - shared, unexpiring, every organisation - so it is " +
			"the one for automation and for the first ten minutes of a deployment, before " +
			"there is an identity provider to sign in through.",
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
		prose: "The command to type when something is refused and it is not clear which of " +
			"three credentials this shell is holding, or which of two deployments it is " +
			"pointed at.",
	},
	{
		name:    "doctor",
		summary: "check a deployment end to end and say what is missing",
		args:    "[flags]",
		flags:   []string{"probe", "org", "json"},
		prose: "One command for \"why does this not work\". It reads the deployment's " +
			"configuration and catalogue and says what is missing, with the thing to do " +
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
			"what places a sign-in in the right tenant. Until there are two organisations " +
			"nothing needs it: everyone who signs in lands in the only one there is. From " +
			"the moment there are two, an address matching no organisation's domain is " +
			"refused rather than put in one - so it is set on every organisation before the " +
			"second one exists, including the one that was there first.",
		subs: []subcommand{
			{name: "create", aliases: []string{"add", "new"}, args: "<name>", summary: "create an organisation",
				flags: []string{"domain", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "every organisation", flags: []string{"json"}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<org-id>", summary: "set the email domain whose sign-ins land in it",
				flags: []string{"domain", "no-domain", "json"}},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<org-id>", summary: "delete an organisation and everything in it",
				flags: []string{"yes", "json"}},
		},
		examples: []string{`keera org create "Example Bank" --domain example.ch`},
	},
	{
		name:    "team",
		aliases: []string{"teams"},
		summary: "teams inside an organisation",
		prose: "Usually one per department, so that budgets and reports line up with how the " +
			"organisation is actually run. The guardrails on a team apply to every key in it.",
		subs: []subcommand{
			{name: "create", aliases: []string{"add", "new"}, args: "<name>", summary: "create a team", flags: []string{"org", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "every team", flags: []string{"org", "json"}},
			{name: "rename", aliases: []string{"set", "edit", "update"}, args: "<team> <new-name>", summary: "give a team a different name",
				flags: []string{"org", "json"},
				prose: "The name is a label. Keys, guardrails, spend and every usage row ever " +
					"written hold the team by its id, so a rename changes what reports are " +
					"headed and nothing else. The old name stays in the audit log."},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<team>", summary: "delete a team that has no working keys",
				flags: []string{"org", "yes", "json"},
				prose: "Refused while any key in the team still works: deleting it would take " +
					"those credentials with it. Revoke them, or move them to another team, " +
					"first. Revoked keys stay as history, under no team."},
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
				flags: []string{"org", "role", "external-id", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "everyone in the organisation", flags: []string{"org", "json"}},
			{name: "role", aliases: []string{"set-role"}, args: "<email> <role>", summary: "member or admin; signs them out",
				flags: []string{"org", "json"}},
			{name: "disable", aliases: []string{"offboard"}, args: "<email>",
				summary: "turn a person off, for when they leave",
				flags:   []string{"org", "yes", "json"},
				prose: "They cannot sign in and are signed out everywhere. Every key attributed " +
					"to them is revoked. Where the deployment lends out sandboxes, their agent " +
					"sandboxes are terminated and their own are suspended, with the volume " +
					"kept. Usage and the audit log are kept.\n\n" +
					"Leaving the directory is not enough on its own: it stops new sign-ins, " +
					"but not the keys a person already has."},
			{name: "enable", args: "<email>", summary: "let a disabled person sign in again",
				flags: []string{"org", "json"},
				prose: "Their old keys stay revoked, so they start with none."},
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
			{name: "create", aliases: []string{"add", "new"}, args: "[<alias>]", summary: "issue an API key",
				flags: []string{"org", "team", "user", "alias", "expires", "json"},
				prose: "The alias is what the key is for, in one label - it is what the panel, " +
					"the reports and the audit log call this key, so a key issued without one " +
					"is a row nobody can identify later.",
				examples: []string{
					`keera key create --team team_1 --user ada@example.ch --alias "Ada's laptop"`,
				}},
			{name: "list", aliases: []string{"ls"}, summary: "every key", flags: []string{"org", "team", "json"}},
			{name: "revoke", aliases: []string{"delete", "rm", "remove"}, args: "<alias>", summary: "stop a key working",
				flags: []string{"org", "yes", "json"},
				prose: "The alias names the key still working under it in this organisation - " +
					"where more than one does, the ids are listed rather than one of them " +
					"picked. An id out of 'keera key list' names a key outright, and with " +
					"--yes needs nothing else.\n\n" +
					"Revoking is immediate and cannot be undone: whatever holds the key is " +
					"refused from its next call, and nothing can print it again. To replace " +
					"a key without an outage, rotate it instead.",
			},
			{name: "rotate", args: "<alias>",
				summary: "replace a key with an identical one and revoke it",
				flags:   []string{"org", "alias", "expires", "json"},
				prose: "For a key that has leaked, and for the ordinary rotation a policy asks " +
					"for. The alias names the key still working under it; an id out of " +
					"'keera key list' names a key outright. --alias and --expires override " +
					"what it had; everything else, including its team and its guardrails, " +
					"is carried over.",
			},
		},
	},
	{
		name:    "model",
		aliases: []string{"models"},
		summary: "the model catalogue clients name",
		prose: "Clients name an alias like 'keera-speed', never a backend model id and never an " +
			"inference URL. That is what lets the model behind an alias be swapped without any " +
			"developer changing anything.\n\n" +
			"A model a catalogue file declares belongs to the file: it is applied again on " +
			"every start, so 'set', 'enable', 'disable' and 'delete' refuse it and 'model " +
			"list' shows it as sourced from the catalogue file. Change the file, then restart " +
			"the gateway or run 'keera model apply'. Its stored API key is the exception - no " +
			"catalogue file carries a credential.\n\n" +
			"--description is a sentence saying what a model is for. Clients see it on " +
			"/v1/models, and it is what a router is told about the model when the router is " +
			"choosing between destinations - so a router whose models have no descriptions is " +
			"choosing between bare aliases.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "the model catalogue", flags: []string{"json"}},
			{name: "add", aliases: []string{"create", "new"}, args: "<alias>", summary: "add a model",
				flags: []string{"provider", "backend", "product-id", "backend-model", "kind",
					"description", "max-context", "price-in", "price-out", "price-cached",
					"api-key", "api-key-env", "disabled", "json"},
				examples: []string{
					"keera model add keera-speed --backend http://vllm:8000/v1 \\\n" +
						"    --backend-model Qwen/Qwen2.5-Coder-7B-Instruct --max-context 32768",
					"keera model add keera-swiss --provider infomaniak --product-id 100234 \\\n" +
						"    --backend-model swiss-ai/Apertus-v1.5-70B --api-key @-",
				}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<alias>", summary: "change any of those on an existing model",
				flags: []string{"provider", "backend", "product-id", "backend-model", "kind",
					"description", "max-context", "price-in", "price-out", "price-cached",
					"api-key", "api-key-env", "no-api-key", "json"}},
			{name: "enable", args: "<alias>", summary: "serve this model", flags: []string{"json"}},
			{name: "disable", args: "<alias>", summary: "stop serving it, keeping its declaration",
				flags: []string{"json"}},
			{name: "check", aliases: []string{"probe", "test"}, args: "<alias>", summary: "probe the live backend end to end",
				flags: []string{"json"},
				prose: "The test for the failure that quietly breaks coding agents: a backend " +
					"answering 200 with prose in 'content' where a tool call was asked for, " +
					"which is what a vLLM parser that does not match its model produces.",
			},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<alias>", summary: "remove a model", flags: []string{"yes", "json"}},
			{name: "apply", args: "<file>", summary: "upsert every model a catalogue file declares",
				flags: []string{"json"}},
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
			"Which tools a scope may call is a guardrail: 'keera guardrail set team t_1 " +
			"--tools github/search_code,jira'. A server's alias allows all its tools; " +
			"alias/tool allows one.\n\n" +
			"Like a model, a server a catalogue file declares belongs to the file.",
		subs: []subcommand{
			{name: "list", aliases: []string{"ls"}, summary: "the MCP servers", flags: []string{"json"}},
			{name: "add", aliases: []string{"create", "new"}, args: "<alias>", summary: "add an MCP server",
				flags: []string{"endpoint", "description", "auth-header", "api-key", "api-key-env",
					"disabled", "json"},
				examples: []string{
					"keera mcp add github --endpoint https://api.githubcopilot.com/mcp/ \\\n" +
						"    --description \"Issues and pull requests\" --api-key @-",
				}},
			{name: "set", aliases: []string{"edit", "update"}, args: "<alias>",
				summary: "change any of those on an existing server",
				flags: []string{"endpoint", "description", "auth-header", "api-key", "api-key-env",
					"no-api-key", "json"}},
			{name: "enable", args: "<alias>", summary: "serve this server", flags: []string{"json"}},
			{name: "disable", args: "<alias>", summary: "stop serving it, keeping its declaration",
				flags: []string{"json"}},
			{name: "delete", aliases: []string{"rm", "remove"}, args: "<alias>", summary: "remove a server",
				flags: []string{"yes", "json"}},
			{name: "calls", summary: "the tool-call log: which tool, what came of it, how much went each way",
				flags:    []string{"org", "server", "tool", "team", "key", "since", "limit", "summary", "json"},
				examples: []string{"keera mcp calls --since 1h", "keera mcp calls --summary --since 168h"}},
			{name: "connect", args: "<alias>", summary: "how to point Claude Code, Codex and others at a server",
				flags: []string{}},
		},
	},
	{
		name:    "guardrail",
		aliases: []string{"guardrails"},
		summary: "what a scope may do: models, rates, spend, machines",
		prose: "A guardrail is what one scope may do: which models it may reach, how fast, how " +
			"much it may spend, what it passes through on the way and how much machine it may " +
			"hold.\n\n" +
			"They nest: an organisation's is the ceiling a team's fits inside, and a team's " +
			"is the ceiling a key's fits inside. So a scope that sets nothing is not " +
			"unlimited - it is whatever holds it. 'guardrail effective' says which level " +
			"each number came from, and is the thing to read before changing one.\n\n" +
			"'keera limit' and 'keera budget' set one part of a guardrail each.",
		subs: []subcommand{
			{name: "get", aliases: []string{"show"}, args: "<scope> <id>", summary: "what this one scope sets; scope is org, team or key",
				flags: []string{"json"}},
			{name: "effective", args: "<scope> <id>",
				summary: "what a request actually meets, and which level decided each part",
				flags:   []string{"json"},
				prose: "The chain collapsed the way the gateway collapses it, with the level " +
					"that decided each value named beside it.\n\n" +
					"Allow-lists intersect; ceilings take the minimum. Two things do not " +
					"merge: budgets, which are checked per level, and system prompts, which " +
					"are joined outermost first.",
				examples: []string{"keera guardrail effective key key_06g9…"},
			},
			{name: "set", aliases: []string{"edit", "update"}, args: "<scope> <id>", summary: "set any of them on a scope",
				flags: []string{"models", "rpm", "tpm", "max-output-tokens", "budget", "period",
					"system-prompt", "no-system-prompt", "filters", "no-filters", "tools",
					"block-hosted-tools", "allow-hosted-tools", "max-sandboxes",
					"max-sandbox-ttl", "sandbox-classes", "max-sandbox-cpu", "max-sandbox-memory", "json"},
				examples: []string{
					"keera guardrail set team team_1 --models keera-speed --rpm 120 \\\n" +
						"    --budget 500 --period month",
				}},
		},
	},
	{
		name:    "limit",
		summary: "a scope's rate limits, on their own",
		args:    "<scope> <id> [flags]",
		flags:   []string{"rpm", "tpm", "max-output-tokens", "json"},
		prose: "The rate-limiting part of 'guardrail set', and nothing else. Same object, " +
			"same scopes, same nesting - fewer flags to read.\n\n" +
			"With no flags it prints what is in force, and which level set it.",
		examples: []string{
			"keera limit team team_1 --rpm 120",
			"keera limit key key_06g9…",
		},
	},
	{
		name:    "budget",
		summary: "a scope's budget, on its own",
		args:    "<scope> <id> [flags]",
		flags:   []string{"budget", "period", "json"},
		prose: "The spending part of 'guardrail set', and nothing else. Spend past a budget " +
			"is refused with 402, not 429: a client reading it as \"slow down\" would retry " +
			"against a limit that only moves when the period rolls over.\n\n" +
			"Budgets do not merge. An organisation's and a team's are two limits, and both " +
			"have to hold. With no flags this prints every budget above the scope.",
		examples: []string{
			"keera budget team team_1 --budget 500 --period month",
			"keera budget org org_06g9…",
		},
	},
	{
		name:    "filter",
		aliases: []string{"filters"},
		summary: "what a request may contain, before it is forwarded",
		prose: "A filter is what every request a guardrail applies it to passes through before " +
			"it is forwarded. With --mode rewrite, the default, a small model takes credentials " +
			"or client data out of a prompt that is about to leave the cluster and lets the " +
			"prompt go. With --mode gate the model edits nothing and answers only whether the " +
			"request may be sent at all, which costs a verdict instead of the whole " +
			"conversation written out again.\n\n" +
			"Both of those cost a second generation on every request the guardrail covers. " +
			"--mode pattern costs nothing: instead of a model and an instruction it takes " +
			"--rules, a list of expressions and what each match becomes, applied in the " +
			"gateway.\n\n" +
			"Reach for pattern where what must not leave has a shape: an API key, a connection " +
			"string, an IBAN, a card number. A rule is faster and more reliable than a model at " +
			"those, gives the same answer every time, and cannot decide to improve somebody's " +
			"stack trace on the way past. Reach for rewrite or gate where it does not - a " +
			"customer's name in an ordinary sentence has no shape.\n\n" +
			"They compose, and the usual shape is one of each with the rules outermost, so the " +
			"model is never shown what the rules took out.\n\n" +
			"A filter that cannot run refuses the request rather than forwarding it unfiltered, " +
			"so 'delete' refuses one a guardrail still names, and says which guardrails to " +
			"change first.",
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
		prose: "A filter reads a request and changes it; a router chooses which model answers " +
			"it - so that the requests that need the large model get it and the rest do not, " +
			"so that a prompt carrying client data is not the one that leaves the cluster, and " +
			"so that a model being down is not a department being down.\n\n" +
			"It is reached by a client naming it in place of a model: 'keera connect --model " +
			"auto' configures an editor for the router 'auto' exactly as it would for an alias. " +
			"That is deliberate - a hook that rerouted a request which had asked for a " +
			"particular model would answer a developer out of a model they did not choose. To " +
			"route a scope whatever it asks for, narrow its allow-list to the router.\n\n" +
			"  keera guardrail set team <team-id> --models auto\n\n" +
			"What a router is told about each destination is that model's --description, so " +
			"write those first. Allowing a router allows its destinations: a key that may use " +
			"'auto' may be sent to anything in 'auto's list.\n\n" +
			"The modes:\n" +
			"  instruction   a small local model reads the request. The default, and\n" +
			"                the only one that costs a generation.\n" +
			"  size          the smallest destination this request's size was meant\n" +
			"                for. Each --destination carries its ceiling in estimated\n" +
			"                tokens; the one without a ceiling takes what is larger.\n" +
			"  fallback      the order they are written, fixed. The first is the one\n" +
			"                you want and the rest are what you want when it is not.\n" +
			"  latency       fastest first, by what each has lately taken to begin.\n" +
			"  least-busy    emptiest first, by requests this gateway has in flight.\n\n" +
			"Reach for fallback when the destinations are ranked, and for latency or " +
			"least-busy when they are equals: two identical vLLM deployments are not a first " +
			"choice and a second choice, and a fallback router over them runs the deployment " +
			"on half its GPUs while looking like one that works. Both are measured inside each " +
			"gateway process - nothing is shared between replicas and nothing survives a " +
			"restart - so a tie falls back to the order you wrote.\n\n" +
			"All of them fail over the same way. A destination that cannot be reached, or that " +
			"answers with a failure of its own, is passed over for the next one. A 4xx is not: " +
			"a request that is too long or over a quota will be all of those things at the next " +
			"destination too, so it is answered rather than repeated. Nothing fails over once " +
			"an answer has started arriving.\n\n" +
			"A router's failures are silent, because every request it places is answered - so " +
			"the number to read is 'router report', the split between its destinations.",
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
		prose: "A sandbox is a machine this gateway lends out for the length of one task or one " +
			"working day: the toolchain already in it, a home directory that survives being " +
			"suspended, an API key of its own that never reaches your laptop, and no way out to " +
			"anywhere the deployment did not name. It expires by itself, which is the point - " +
			"'extend' is how one lives longer, and nothing lives for ever.\n\n" +
			"--team scopes the key the sandbox is given. A sandbox on a team is charged to that " +
			"team's budget, held to its rate limit, counted against its sandbox quota, and its " +
			"agent sees only the models the team allows. Without it the sandbox works inside " +
			"the organisation's own guardrails.\n\n" +
			"'sandbox ssh' runs your own ssh over the gateway's single published port, so there " +
			"is no second address and no jump host. That also means VS Code's Remote-SSH and " +
			"JetBrains Gateway work against a sandbox unmodified: they want an ssh transport " +
			"and nothing else. 'keera sandbox config >> ~/.ssh/config' is the whole of the " +
			"setup.\n\n" +
			"With --purpose agent - or 'sandbox agent', which is the same thing with the flags " +
			"already set - the sandbox is one task's machine instead. Nothing attaches to it, " +
			"it is terminated rather than suspended when its time runs out, and it names its " +
			"own session, so 'keera sessions' reports what it did as one task rather than " +
			"inferring the grouping. The --task is passed to it and never stored.\n\n" +
			"'up', 'ls' and 'rm' are accepted for create, list and terminate, for hands that " +
			"have typed them at a container runtime for years.",
		subs: []subcommand{
			{name: "classes", summary: "the machines this deployment offers", flags: []string{"org", "json"}},
			{name: "create", aliases: []string{"add", "up", "new"}, args: "<name>", summary: "start one",
				flags: []string{"org", "team", "class", "purpose", "ttl", "repo", "branch",
					"ssh-key", "json"}},
			{name: "agent", args: "<name>", summary: "start one for an agent",
				flags: []string{"org", "team", "class", "ttl", "repo", "branch", "task", "json"}},
			{name: "list", aliases: []string{"ls"}, summary: "what is running",
				flags: []string{"org", "team", "class", "all", "json"}},
			{name: "show", aliases: []string{"get"}, args: "<name>", summary: "one sandbox in full", flags: []string{"org", "json"}},
			{name: "ssh", args: "<name> [-- <command>]", summary: "open a shell in it",
				flags: []string{"org", "port"}},
			{name: "config", summary: "the ~/.ssh/config block, so any editor can attach",
				flags: []string{"org"}},
			{name: "proxy", args: "<name>", summary: "the attach surface, for ssh's ProxyCommand",
				flags: []string{"org", "port"}},
			{name: "extend", args: "<name>", summary: "push its expiry out again",
				flags: []string{"org", "ttl", "json"}},
			{name: "suspend", args: "<name>", summary: "release its compute, keep its disk",
				flags: []string{"org", "json"}},
			{name: "resume", args: "<name>", summary: "start it again where it left off",
				flags: []string{"org", "json"}},
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
			"keera usage --by team",
			"keera usage --by user --since 720h",
		},
	},
	{
		name:    "failures",
		aliases: []string{"failure"},
		summary: "the calls that did not deliver, and what the backend said",
		args:    "[flags]",
		flags:   []string{"kind", "model", "team", "key", "status", "since", "limit", "org", "json"},
		prose: "The message is printed beside the model and the key because the message is the " +
			"whole point: a status says a call failed, and only the backend's own wording says " +
			"whether that is the deployment's problem, the developer's, or the endpoint " +
			"operator's.",
	},
	{
		name:    "sessions",
		summary: "what agents have been carrying out, one row per task",
		args:    "[flags]",
		flags: []string{"sort", "unhappy", "model", "team", "key", "user", "since", "limit",
			"org", "json"},
		prose: "A session is the calls one coding agent made working through one task. Nobody " +
			"decides how many calls a task takes, so a cost per call is a number nobody can act " +
			"on and a cost per task is the one that goes into a budget conversation.\n\n" +
			"The grouping is computed when the report is read, from a hash of the conversation " +
			"the gateway records on each request - no prompt is stored - and cut wherever the " +
			"agent went quiet for longer than KEERA_SESSION_GAP.",
	},
	{
		name:    "session",
		summary: "one task from beginning to end, named by any request in it",
		args:    "<request-id> [flags]",
		flags:   []string{"org", "json"},
	},
	{
		name:    "connect",
		summary: "the finished configuration for an editor",
		args:    "[<client>] [flags]",
		flags:   []string{"model", "org", "json"},
		prose: "The panel's \"Connect a client\" screen for whoever does not have the panel. It " +
			"prints the configuration block on stdout and everything around it on stderr, so it " +
			"can be redirected straight into the file it names.\n\n" +
			"With no client it lists the ones this deployment can configure.",
		examples: []string{
			"keera connect",
			"keera connect opencode --model keera-speed > ~/.config/opencode/opencode.json",
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
// enough to read without scrolling.
func overview() string {
	var b strings.Builder
	b.WriteString(style.head(tagline) + "\n\n" + style.head("Usage:") +
		"\n  keera <command> [subcommand] [flags]\n\n" + style.head("Commands:") + "\n")
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
		fmt.Fprintf(&b, "  %s  %s\n", padTo(style.cmd(c.name), width), c.summary)
	}
	base, from := resolveBase()
	b.WriteString("\nRun 'keera help <command>', or 'keera help <command> <subcommand>'.\n")
	b.WriteString("Every listing command also takes --json, and every command --color=never.\n")
	// Which deployment the commands act on cannot be seen anywhere else.
	fmt.Fprintf(&b, "\nThese commands talk to %s (%s).\n", style.cmd(base), from)
	b.WriteString("Point them elsewhere with --url, or sign in: keera login --url <your deployment>\n")
	return b.String()
}

// helpText renders one command, or one of its subcommands. fs is the
// FlagSet the command parses with, or nil for a command with no flags.
func helpText(fs *flag.FlagSet, name, subName string) string {
	c, ok := find(name)
	if !ok {
		return overview()
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
		b.WriteString("  " + wrapAt(para, 2) + "\n\n")
	}
	// The caller's next section opens with its own blank line.
	trimTrailingBlank(b)
}

// proseWidth is the width help and reports are laid out for.
const proseWidth = 78

// wrapAt breaks text to proseWidth, for text that starts at column start
// because something is already printed in front of it. Every line after the
// first is indented to that column.
func wrapAt(text string, start int) string {
	var b strings.Builder
	column := start
	for i, word := range strings.Fields(text) {
		n := len([]rune(word))
		switch {
		case i == 0:
			column = start + n
		case column+1+n > proseWidth:
			b.WriteString("\n" + strings.Repeat(" ", start))
			column = start + n
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
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "-1" {
			usage += " (default " + f.DefValue + ")"
		}
		if len(left) > width {
			width = len(left)
		}
		rows = append(rows, row{left, usage})
	}
	for _, r := range rows {
		fmt.Fprintf(b, "  %s  %s\n", padTo(style.cmd(r.left), width),
			wrapAt(r.usage, width+4))
	}
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
func wantsHelp(args []string) (string, bool) {
	asked := false
	var named []string
	for _, a := range args {
		switch a {
		case "-h", "--help", "-help", "help":
			asked = true
		default:
			if !strings.HasPrefix(a, "-") {
				named = append(named, a)
			}
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
		return fmt.Errorf("keera %s needs a subcommand", name)
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
		fmt.Print(overview())
		return nil
	}
	name := args[0]
	if _, ok := find(name); !ok {
		if near := suggest(name); near != "" {
			return fmt.Errorf("no command %q; did you mean 'keera %s'?", name, near)
		}
		fmt.Fprint(os.Stderr, overview())
		return fmt.Errorf("no command %q", name)
	}
	rest := append(slices.Clone(args[1:]), "--help")
	return Run(ctx, append([]string{name}, rest...))
}
