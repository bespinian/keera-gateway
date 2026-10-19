# The gateway

Keera Gateway is one static Go binary. It holds the inference data plane, the
control plane and the control panel, and it talks to Postgres. The `keera`
command administers it over the control API, so the same commands work against
any gateway, on a cluster or on localhost.

## One listener, four surfaces

Port 8080 carries everything, told apart by path:

| Path       | What it is                                                                                                           |
| ---------- | -------------------------------------------------------------------------------------------------------------------- |
| `/api`     | The inference API and the MCP servers. Authenticated by an API key, as a bearer token, `x-api-key` or `X-Keera-Key`. |
| `/control` | The control API. Authenticated by the operator key, a panel session or a `keera login` token.                        |
| `/sandbox` | Attaching to a sandbox, and the Git credential a sandbox asks for.                                                   |
| `/`        | The control panel, and `/metrics`, `/healthz` and `/readyz`.                                                         |

`/sandbox` is not JSON. It carries a byte stream between an authenticated
caller and a port inside a sandbox, so attaching needs no second port and no
second certificate. With sandboxes switched off, it answers 501
`sandboxes_disabled`. See [sandboxes.md](sandboxes.md).

A deployment publishes one address and one certificate. The downside is that no
port separates the planes, only the credentials. See
[Publishing it](install.md#publishing-it).

`/healthz` checks only the process. `/readyz` also pings Postgres, so a replica
that has lost the database leaves the inference Service instead of being
restarted. `keera-gateway health` asks the gateway's own `/readyz`, for a
container healthcheck in an image that has no curl.

## The alias is the API contract

A client names an alias such as `keera-speed`, never a model id or a vLLM URL.
So you can swap the model behind an alias, change quantisation, or point it at a
remote endpoint without any developer changing anything.

- `backend_model` is the name the backend serves the model under - vLLM's
  `--served-model-name`, llama.cpp's `--alias`. If it is wrong, every request
  returns 404.
- `max_context` is shown to clients on `/api/v1/models`, sizes a filter's or a
  router's own model, and bounds every request. See
  [Requests too long for the model](#requests-too-long-for-the-model).
- `location` says where the model runs, and so where prompts go: `ch` or `usa`
  for a hosted provider, `onprem` for your own inference plane. A provider fills
  it in. Without one, a backend inside your network - a service name, a private
  address or the loopback - is `onprem`, and a backend on the internet must say
  where it is. Clients see it on `/api/v1/models`.
- `release_date` is the day the model came out, as `YYYY-MM-DD`. A provider
  fills it in for the models it knows. `/api/v1/models` gives it as `created`.
- `display_name` is the model's name for people, such as `Claude Haiku 5.5`.
  A provider fills it in for the models it knows. The panel shows it, and
  `/api/v1/models` gives it as `display_name`. Clients still name the alias.
- An alias is lowercase letters, digits and interior hyphens, because it appears
  in the client configurations `keera connect` prints. A model's alias may also
  hold slashes, to name the provider, as in `anthropic/claude-sonnet-4-5`.

## Models belong to an organisation

Every model belongs to one organisation. Only that organisation can call it,
and its administrators add, change and remove it. The backend can be any URL:
your own inference plane, or a provider reached with the organisation's own
key or the deployment's ([billing.md](billing.md)).

An administrator adds one in the panel (**Models → Add model**) or with
`keera model add <alias>`. Both act on the organisation the person works in.
An operator picks one in the panel, or with `keera org switch <org>`
([Several organisations](#several-organisations)).

Each organisation has its own aliases, so two organisations can each have a
`fast`. The gateway looks up the `model` field in the organisation's models
first, then in its routers. A model and a router in the same organisation
cannot share an alias.

To stop a project or a key using a model, leave it out of that guardrail:
`keera guardrail set project <id> --models a,b`. To stop the whole organisation
using it, disable the model in its edit dialog.

Deleting a model does not change the filters and routers that use it. An
enforcing filter whose model is gone refuses every request it covers; a shadow
one records an error and lets them through. A router leaves the model out of its
choice, or falls back or refuses when it was the model that decides.
The delete confirmation lists them first.

### Template files

A new organisation starts with a copy of each model in the template file
(`KEERA_MODELS_FILE`). That includes the one called "Keera" that the first
operator sign-in creates on an empty deployment. The copies are the
organisation's own: its administrators change, disable or remove them like any
other model. Changing the file later does not touch organisations that already
exist. To add the file's models to one, switch to it and run
`keera model apply <file>`. `keera model validate <file>` checks a file without
a server.

`KEERA_SANDBOXES_FILE` works the same way for
[sandbox classes](sandboxes.md#classes), with `keera sandbox apply <file>`.

The file holds no API keys. After a new organisation is created, its
administrator stores the key for each hosted model
([Where the credential lives](providers.md#where-the-credential-lives)), unless
the deployment holds that provider's key. `keera model apply` keeps a key that
is already stored.

## Three request shapes

The data plane serves the OpenAI API at `/api/v1/chat/completions`,
`/api/v1/completions` and `/api/v1/embeddings`, the Anthropic Messages API at
`/api/v1/messages`, and the OpenAI Responses API at `/api/v1/responses`. Claude
Code uses the Messages API and Codex the Responses API. All of them go through
the same guardrails, accounting and audit.

`GET /api/v1/models` lists the models a key may call, and
`GET /api/v1/models/{alias}` describes one. The list carries the fields of both
OpenAI's and Anthropic's model lists, so either SDK reads it. `HEAD
/api/api/hello` answers 200, for the warm-up probe some Anthropic clients send.
The path is doubled because they append `/api/hello` to a base URL that already
ends in `/api`.

A client given a base URL without `/api` gets a 404 `wrong_base_url` that says
where the API is, rather than the panel's page.

A request body may be sent plain, or compressed with gzip or deflate. The
size limit holds for what it unpacks to. Any other `Content-Encoding` is refused
with a 415.

The inference plane speaks chat completions, so the other two are translated on
the way in and back on the way out. Text that a local model can read is kept:
plain-text documents and files, and search results. Fields that chat
completions has no place for, such as thinking blocks, PDF documents,
reasoning items, OpenAI's built-in tools and Anthropic's server tools, are
dropped. So is any block or item of a type the gateway does not know. The tool
role carries only text, so images that tools return follow the tool results as
a user message. Every answer names the alias as its model, never the backend's
name for it.

A model that writes tool arguments that are not JSON has them answered as `{}`,
and the client reports the tool call as failed. On a Messages stream, a tool
call's arguments are held until the call is whole and then sent at once, since
a fragment cannot be taken back.

Chat completions has no tool namespaces and no custom tools, which Codex uses
for its MCP tools, its subagents and `apply_patch`. A tool in a namespace is
declared on its own, under both names joined by `__`, such as
`mcp__demo__get_weather`. A custom tool becomes a function that takes its text
as `input`, with its grammar in the description. The answer names each call
the way the client declared it.

The exception: a Messages request to a model declared with Anthropic, or a
Responses request to a model declared with OpenAI, is forwarded as it is.
Translating it would lose prompt caching and thinking at Anthropic, and
reasoning items at OpenAI. In a coding agent's session, most of the bill is the
cache. The guardrails still apply: filters read and rewrite the request in its
own shape, and the system prompt and the output ceiling are written into it. If
a router falls back from such a model to a local one, the local one gets the
translation. Without a local fallback or a router that reads the request, it
is not translated at all, so the provider checks it rather than the gateway.

`POST /api/v1/messages/count_tokens` is answered by the gateway with an
estimate that errs high. It is never forwarded, because that would send the
prompt to Anthropic without passing its filters.

**The gateway stores no Responses state.** The client sends the whole
conversation in `input` every time. There is no `/api/v1/conversations`, and no
route to read, cancel or delete a stored response. A request with
`previous_response_id` or `conversation` continues a conversation stored at
OpenAI, so only an OpenAI model on the organisation's own key accepts it. Any
other model refuses it with a 400.

On the deployment's own OpenAI key ([billing.md](billing.md)), every
organisation shares one OpenAI account, so nothing may be stored there. A
Responses request is sent with `store` off, and one with
`previous_response_id`, `conversation` or `background` is refused with a 400.

A model is one of OpenAI's own when its [provider](providers.md) is `openai`.

## Which client sent a request

Every request is recorded with the client that sent it, such as Claude Code,
Codex or curl. The gateway reads it from the `User-Agent`. A client can name
itself instead in `X-Keera-Client`, which wins, so a tool built on a common
HTTP library is not filed under that library. The name is what the client
says, so it tells what is connected, not who may connect.

`keera usage --by client` adds up usage by client. In the panel, **My
clients** lists the clients that called with your own keys, and which key each
one used. Its **Connect client** button shows how to set up a new one. The
dialog that shows a new key's secret has the same button, with the secret
already filled in. For Claude Code, `keera connect claude-code` does all of it
([Connecting Claude Code](#connecting-claude-code)).

## Connecting Claude Code

```sh
keera login
keera connect claude-code --model claude-opus
```

This gives the machine a key of its own and writes it into
`~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR/settings.json`), so nobody
copies it. The rest of the file is kept. The key always belongs to a person, so
the command needs a sign-in and refuses the operator key.

The key is named `claude-code on <hostname> (<id>)`. The id is random and kept
in keera's configuration directory, so machines that share a hostname keep
their own keys. The command finds the key by this name, so keep it. In order,
the command:

- rotates the machine's key if it has one. The key keeps its project,
  guardrails and lifetime.
- activates a key left for the person with `--pending`, and names it after the
  machine.
- for an administrator, issues a new key, which lasts 90 days like one from
  `keera key create`. `--project` puts it in a project.

A member with none of these is told what to ask an administrator for.

The file then holds:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://keera.example.ch/api",
    "ANTHROPIC_AUTH_TOKEN": "keera_sk_…",
    "ANTHROPIC_MODEL": "claude-opus[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-opus[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "claude-opus[1m]",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  },
  "modelOverrides": {
    "claude-opus-5-5": "claude-opus",
    "claude-haiku-4-5": "claude-haiku",
    "claude-haiku-4-5-20251001": "claude-haiku"
  }
}
```

- **Models.** Every enabled chat model and router the key may use. `--model`
  picks the one Claude Code starts with; without it, the first. Claude Code
  asks for Opus, Sonnet, Haiku and Fable by Anthropic's names, for background
  work too, so each is pointed at the model of that family, or at the main one
  when there is none. Left unset, those requests fail with 404.
- **`modelOverrides`** tells Claude Code which Claude model each Anthropic alias
  is, so it knows what the model supports. Without it, Claude Code would send
  Haiku adaptive thinking, which Haiku refuses. A dated model, such as Haiku
  4.5, is known only by its dated id, so that one is written too.
- **`[1m]`** marks a model with a 1M context. Claude Code takes a model it does
  not know to have 200K, and takes the suffix off before it sends the request.
- **`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`** keeps Claude Code's telemetry
  from leaving through anything but the gateway.
- **`ANTHROPIC_BASE_URL`** is left out, or taken out, when the organisation's
  managed settings set it already. Claude Code stops fetching the settings from
  the claude.ai admin console when the person's own settings name an address.
  The command reads Claude Code's copy of those settings and the
  `managed-settings.json` files, not MDM profiles or the Windows registry, and
  warns when they point elsewhere. `claude doctor` shows whether the fetch
  works, on its `Managed settings (remote)` line.

An `apiKeyHelper` setting makes Claude Code send its key instead, so the
command warns about one. `--print` prints the same settings instead of writing
them, for a machine the command does not run on, with the key named in `--key`
shown by its prefix; put the key itself in its place. The panel's **Connect
client** shows them too. With a Claude plan, see
[subscriptions.md](subscriptions.md#4-each-person-once-per-machine).

## The tenancy model

Three levels: organisation, project, key. A guardrail can attach at any of them.

The combining rule is **restrict-only**. A level can narrow what it inherits but
never widen it. Allow-lists intersect; the output-token ceiling takes the
minimum.

The exceptions:

- **Rate limits and budgets are kept per level and checked separately**, not
  merged. An organisation cap of 10,000 and a project cap of 1,000 are two limits
  that must both hold, and each level's `rpm` and `tpm` has its own bucket.
- **System prompts and filters accumulate**, outermost first. A level may add to
  what it inherits but may not drop it.
- **Only the organisation grants repositories** to sandboxes. A project can only
  narrow the list; a key cannot set it.

When an organisation's guardrail lists allowed models, a model the organisation
adds is put on that list, whether it was added in the panel, with
`keera model add` or with `keera model apply`. New routers are added
[only when the list allows their destinations](routers.md#allowing-a-router-allows-its-destinations).
Projects and keys that list models keep their own lists. Deleting a model takes
it off every list in the organisation, so a model added later under the same
alias is not allowed by accident. A list that named only it allows nothing.

A budget with no period is a monthly one. Budgets reset on UTC boundaries in
every deployment.

`keera guardrail show <scope> <id>` shows what one level sets on its own. A
level that sets nothing is not unlimited: it gets whatever it inherits. To see
the combined result, run `keera guardrail effective <scope> <id>` (or
`GET /control/v1/guardrails/{scope}/{id}/effective`). The endpoint returns every
level's guardrails and the combined answer; the command also shows which level
decided each value.

The panel's **Playground** has no way around this. You pick one of your own
active keys, and every message is sent with it: it meets that key's guardrails,
counts against its rate limits and budgets, and shows in its usage.

## Keys and projects

Organisation names are unique, ignoring case. An operator renames one with
`keera org set <org> --name "Example Bank AG"`, or the pencil on the
organisations screen. Everything refers to it by id, so nothing else changes.

Every key is in a project. A new organisation starts with one project, called
`default`, which is a project like any other. A key created without `--project`
goes in the organisation's oldest project, and an organisation with no projects
cannot have keys. A project with keys that are not revoked, expired ones
included, cannot be deleted. Its revoked keys stay, without a project.

A project has a name, unique in its organisation, and an optional description.
An administrator changes either with
`keera project set <project> --name <name> --description <text>`, or the pencil
on the projects screen.

A key belongs to a person: whoever issues it, unless `--user` names someone
else or `--no-user` nobody, as for a pipeline. A key issued with the operator
key belongs to nobody, since the operator key is not a person. The panel does
the same.

A key's secret is printed once and never stored. Nobody, not even an operator,
can read it later. Screens, reports and the audit log show its name instead. A
key lasts 90 days. `--expires 720h` gives it another lifetime,
`--expires 2026-12-31` makes that the last day it works, and `--expires never`
makes a key that does not expire. The panel offers the same choices.

A key's name is only a label: `keera key set <key> --name <name>`, or the
pencil on the **API keys** screen. A member can rename their own keys, an
administrator any key in their organisation, and an operator any key. The old
name stays in the audit log.

Only an administrator issues keys, so every key has the project and guardrails
an administrator chose for it. A key for someone else can be left for them to
activate: `--pending`, or "They activate it" in the panel, which is the panel's
default. It has no secret and does not work until its holder activates it on
their dashboard, on **My access**, with `keera key activate`, or with
[`keera connect claude-code`](#connecting-claude-code). So only they ever see
it.

```sh
keera key create --user ada@example.ch --name "Ada's laptop" --pending
```

`keera key rotate`, or **Rotate** on the **API keys** screen, replaces a key in
one step. The new key has the same project, owner, lifetime and guardrails, and
the old one is revoked at once. An expired key can be rotated too. Anyone who
may revoke a key may rotate it, so members rotate their own. Only an administrator can give the new
key another lifetime with `--expires`. The command asks you to type the key's
name first; `--yes` skips that in a script.

```sh
KEY=$(keera key rotate "a developer's laptop")
```

A rotated key stops working everywhere it is used, so give each machine its own
key.

## Several organisations

A person can be in several organisations, with a role in each. Each organisation
adds them, in the panel or with `keera user add`. That is an invitation until
they first land there.

In the panel, someone in more than one sees a menu instead of the organisation's
name at the top. Choosing an organisation moves the session there, and choosing
an invitation joins it. On the command line:

```sh
keera org list                 # yours, and the ones that invited you
keera org switch "Example Bank"
KEERA_ORG=org_123 keera usage  # one shell or pipeline
```

A browser remembers the organisation it was last in, and a new sign-in lands
there. Elsewhere it lands in the one used last ([Which organisation a sign-in
lands in](sso.md#which-organisation-a-sign-in-lands-in)). `keera login` goes
back to the one that machine was last in. Every command acts on that
organisation, so none takes `--org`. The exceptions are `keera billing grant`
and `keera billing invoiced`, where an operator names it each time, and `keera
sandbox proxy`, which `keera sandbox config` writes into the ssh config.

An operator works in every organisation, so `keera org switch` only chooses the
one commands act on, and `keera org switch --all` goes back to all of them. The
operator role goes wherever the person goes.

Disabling a person or changing their role acts in one organisation. Leaving the
directory ([Asking the directory again](sso.md#asking-the-directory-again)) or
`keera logout --all` ends their sign-ins in all of them. Each switch is in the
audit log as `auth.switch_org`, in the organisation switched to.

## What happens to one request

1. Authenticate the key, and resolve the guardrails attached to its
   organisation, its project and itself.
2. Read the body, find the model, check the rate limits and the budgets. A
   [subscription model](subscriptions.md#what-it-costs) meets the budgets only
   when filters apply, because only they spend money.
3. **Router**, if the client named one: choose the destination.
4. Refuse the request if it cannot fit in the destination's context.
5. **Filters**, outermost level first: each may rewrite the request or refuse it,
   with a model and an instruction or with a list of expressions.
6. Prepend the standing system prompt (chat requests only; a completion has no
   place for one), take out hosted tools if any model the request may reach is
   on the deployment's key ([billing.md](billing.md)), and clamp the output
   ceiling.
   For a subscription model the prompt goes after Claude Code's own system
   blocks instead; see [subscriptions.md](subscriptions.md#what-is-sent-to-anthropic).
7. Forward, stream the answer back, and record what it cost.

Why this order:

- The router (3) runs before the filters (5) so it reads what the client sent.
  Once a redaction filter has removed a client's name, the prompt looks safe for
  any model, and a router placed after it would send it outside the cluster.
- The context check (4) runs before the filters, so a request that cannot
  succeed costs nothing more than the router's decision.
- The system prompt (6) is added after the filters, so an administrator's own
  wording is never handed to a small model that is allowed to edit it.
- Router and filters both run after the budget check, because both spend money.

## Models that filters and routers run on

[Filters](filters.md) and [routers](routers.md) share these rules. Most are
about the model that a `rewrite` or `gate` filter, or an `instruction` router,
calls to decide.

**Which model.** A chat model, or a
[decision model](#decision-models) for a gate or a router. It must be
on the organisation's allow-list, if it has one. Otherwise the filter or router
is refused with a 400 `model_not_allowed`. Narrowing the allow-list so that the
model drops off is refused with a 409 `filter_model_not_allowed` or
`router_model_not_allowed`. Move the filter or router to an allowed model
first.

**Not a subscription model.** A
[subscription model](subscriptions.md#what-a-subscription-key-can-do) cannot be
a filter's model, a router's model or a router's destination. Saving one is
refused with a 400, and turning on `subscription` for a model a filter or
router uses is refused with a 409.

**When the model cannot run.** The model is missing, disabled, the wrong kind,
a subscription model, has no backend, or is
[locked until the organisation pays](sso.md#what-a-new-organisation-can-use).
A filter then [fails closed](filters.md#failing-closed), and a router
[falls back or refuses](routers.md#when-it-cannot-decide).

**Logprobs.** A gate and an instruction router read the probabilities of the
model's first token. vLLM and llama.cpp return them; most hosted providers do
not. There is nothing to configure. If a backend refuses a request for them
with a 4xx, the gateway stops asking that model for them from then on. So does a
router whose model answers without them. A gate then reads only the words of the answer, and a router asks for
a destination's name instead of a letter. So a hosted model costs at most one
retried call per gateway process.

**Checks.** `keera filter check` and `keera router check` call the backend
directly, with the credentials the data plane uses. A check is not rate-limited
or budgeted and writes no usage row. On the deployment's own provider key it
needs credit and is billed ([billing.md](billing.md#what-is-billed)). It does
put real requests on the GPUs.

**Who owns what.** A filter or a router belongs to one organisation, and its
administrators write it. Two organisations can each have one with the same
alias; they are different filters or routers. The alias is its identity, like a
model's: lowercase letters, digits and interior hyphens. It cannot change: for
a different alias, create a new one and move its guardrails or clients over.
Members cannot create or change filters and routers, but they can read them,
instruction included. Their screens, `GET /control/v1/filters`,
`GET /control/v1/routers` and each one's `/{alias}/report` are open to anyone
signed in to the organisation.

### Decision models

A decision model is a classifier, such as
[Laya](https://huggingface.co/convaiinnovations/laya) served by `laya-serve`.
It reads a text and a typed question and returns a probability for each answer,
in one forward pass. It writes no text.

So it can decide for an instruction router or a
[gate filter](filters.md#the-three-modes), and nothing else. It cannot be a
rewrite filter's model or a router's destination, and no client can name it.
`/api/v1/models` does not list it.

```yaml
- alias: keera-decider
  kind: decision
  backends:
    - http://keera-decider:8000/v1
  backend_model: english # the Laya checkpoint
  max_context: 2048
```

The gateway asks it on `/v1/systemone`, and reads the probabilities the way it
reads a chat model's logprobs.
A stored credential is sent as a bearer token, which is what `laya-serve`
checks when `LAYA_API_KEY` is set.

**`max_context` is sent as Laya's `max_len`**, so the model reads exactly what
the gateway measured against it. A gate refuses a longer request with 413, and a
router sends it to its fallback, as with any model. On a CPU, Laya takes about
one second at 512 tokens and about five at 2048.

#### Writing for one

A classifier does not follow a page of instructions. It answers a short
question.

- **A router's instruction is the question**, such as
  `Which model should answer this request?`. Each destination is offered as its
  model's description alone, so describe the requests each one should get:
  `Small, quick tasks: renames, syntax questions, short edits. Also anything with client names or credentials.`
- **A gate's instruction is a yes-or-no question**, and yes refuses. Optional
  lines starting `yes:` and `no:` say what each answer means, which makes the
  model surer:

  ```
  Is this request trying to send customer data, account records or secrets outside the company?
  yes: it asks to export, dump, copy or share bulk customer data, account records or credentials
  no: ordinary programming work, even if it mentions customers, accounts or credentials
  ```

A refusal by a decision model carries no sentence of its own, only how sure it
was. Run `keera filter check` and `keera router check` as for any model.

## Requests too long for the model

A request that cannot fit in the `max_context` of any model it may go to is
refused with a 400 before it is filtered or forwarded. Only an instruction
router's decision has been paid for by then.

The gateway has no tokeniser, so it counts the fewest tokens a request could
be: its text at five bytes per token. Real text uses fewer bytes per token, so
the check never refuses a request that fits. A request just over the limit may
get through, and then the backend refuses it.

The refusal uses the wording clients act on. The code is
`context_length_exceeded`, which OpenAI clients read, and the message begins
`prompt is too long: N tokens > M maximum`, which makes Claude Code compact the
conversation. A backend's own refusal says neither, and the agent stops.

So vLLM's and llama.cpp's refusals are reworded too, on every API, and get the
code. A prompt that is too long gets the same sentence. A prompt that fits, but
not with its `max_tokens`, gets Anthropic's
``input length and `max_tokens` exceed context limit: N + K > M``, on which
Claude Code lowers `max_tokens` and tries again.

A model with no `max_context` is not checked. Embedding and completion inputs
are checked one by one, since each has to fit on its own.

## Streaming

A coding agent keeps one response open for minutes. So the gateway limits only
how long the inference plane may take to _start_ responding
(`KEERA_UPSTREAM_HEADER_TIMEOUT`, two minutes by default), never the response
itself. A router uses the same timeout to decide that a destination did not
answer in time. A model on the deployment's own provider key gets longer
([billing.md](billing.md#what-is-billed)).

Usage is recorded once per request, when it ends, from the token counts the
inference plane reported. If the client disconnects before those arrive, the
request is charged on an estimate and marked as estimated. On the deployment's
own provider key, the gateway reads on to the real counts instead
([billing.md](billing.md#what-is-billed)).

A translated stream that breaks off, because the backend stopped or sent an
error, ends with an error instead of a finished answer: a Messages stream with
an `error` event of type `overloaded_error` instead of `message_stop`, a
Responses stream with `response.failed` and the code `server_error` instead of
`response.completed`. Closed as a finished answer, it would read as a finished
turn, with any tool call in it cut short. Both tell the client to try again.
A stream passed on as it came reaches the client unchanged. Either way, the
request's row keeps the 200 already sent and says in its error why the answer
was not whole.

A client that hangs up before any answer still gets a row, with status 499 and
marked as cancelled, in the field `canceled`. It carries what the router and the
filters had spent by then, so hanging up never makes them free.

An ingress in front has to stream too; see
[Publishing it](install.md#publishing-it).

## Caching

The gateway keeps an in-memory view of the control plane (`internal/registry`),
so a request waits on the database only to check a key it has not seen
recently. A change made through the control API is pushed to every replica at
once, over Postgres LISTEN/NOTIFY, so a revoked key stops working right away.
A change to one key, such as a sandbox starting or stopping, drops only that
key. Any other change, such as a model or a guardrail, drops the keys of
its organisation and reloads that organisation alone. In case a
notification is lost, a checked key is reused for at most
`KEERA_CACHE_TTL` (30 seconds), and models, filters, routers and MCP servers are
reloaded every minute. Spend is refreshed separately (`KEERA_SPEND_REFRESH`, 10
seconds).

If Postgres is down, the models and the rest stay as last loaded, and a key
keeps working until its `KEERA_CACHE_TTL` runs out. After that, every request
with it gets 503 `control_plane_unavailable` until the database is back.

## Rate limits across replicas

The token buckets behind `rpm` and `tpm` live in each process. With N replicas
behind a load balancer, a limit of 600 requests per minute is 600 per replica,
and up to 600N in total. Spend is reconciled through Postgres, so a budget means
the same thing however many gateways are running. Each replica reads spend again
every `KEERA_SPEND_REFRESH` (10 s), so a budget can be overshot by about that
much traffic.

Set `KEERA_REDIS_URL` (`redis://host:6379/0`, or `rediss://` for TLS) to keep
the buckets in Redis. One script then decides all of a request's levels
atomically, and every replica spends the same allowance - see
`internal/ratelimit/redis.go`. Use a single Redis endpoint, not Redis Cluster,
which refuses a script whose keys are in different slots.

- **Redis holds buckets and nothing else.** No guardrails, spend, sessions or
  prompts. Nothing in it has to survive a restart.
- **If Redis cannot be reached, traffic is not refused.** The gateway falls back
  to the in-memory buckets, and says so in the log and in
  `keera_ratelimit_fallback_total`. After a failed round trip it waits 5 seconds
  before trying Redis again, so an outage does not add the timeout to every
  request.
- **The headers come from the same decision**, not from a second round trip.

The control plane's sign-in throttle stays per replica.

Answers say what is left of the allowance, so a client can see a limit coming.
The budget headers are sent when some level has a budget, and the request
headers when some level has an `rpm`. They describe only the tightest level:

| Header                           | What it holds                        |
| -------------------------------- | ------------------------------------ |
| `X-Keera-Budget-Remaining`       | what is left to spend, in CHF        |
| `X-Keera-Budget-Limit`           | the budget                           |
| `X-Keera-Budget-Reset`           | when it resets, in RFC 3339          |
| `X-Keera-Budget-Scope`           | the level: `org`, `project` or `key` |
| `X-RateLimit-Limit-Requests`     | the `rpm`                            |
| `X-RateLimit-Remaining-Requests` | requests left in the bucket          |
| `X-Keera-RateLimit-Scope`        | the level: `org`, `project` or `key` |

A `tpm` limit has no header. When a limit is hit, the message says which limit
and its value. A rate limit answers 429 with `Retry-After`. A budget answers
402, and its message gives the day it resets. When the provider itself refuses,
its `Retry-After` and `X-Should-Retry` are passed on.

Other response headers:

| Header                   | What it holds                                                                             |
| ------------------------ | ----------------------------------------------------------------------------------------- |
| `X-Keera-Model`          | the alias that answered; behind a router, the destination that answered                   |
| `X-Keera-Router`         | the router and where it sent the request ([routers.md](routers.md#what-a-client-can-see)) |
| `X-Keera-Filters`        | the filters that acted on the request ([filters.md](filters.md#what-is-recorded))         |
| `X-Keera-Removed-Tools`  | the hosted tools taken out on the deployment's key ([mcp.md](mcp.md#hosted-tools))        |
| `X-Keera-Removed-Fields` | the fields that cost extra, taken out on the deployment's key ([billing.md](billing.md))  |
| `X-Request-Id`           | the request's id; one the client sends is kept if it is up to 64 of `A-Z a-z 0-9 . _ : -` |

### Naming a request yourself

A client can also send its own id in `X-Client-Request-Id`, as OpenAI's API
allows. Unlike `X-Request-Id`, the client knows it even when no answer comes
back. The gateway writes it to its log as `client_request_id`, next to
`request_id`, and sends it on to OpenAI models, so OpenAI's support can find the
request too. Requests that filters and routers send for a request don't carry
it. An id that is not printable ASCII or is longer than 512 characters is
ignored, since OpenAI would answer it with a 400.

Both ids are kept with the request. The Requests screen finds a request by
either one, in any period, and its details show them.

## Errors

Errors come in the shape of the API that was called. The Messages API has no
field for a code, so its errors carry only a type that follows the status, such
as `billing_error` for 402. The other APIs carry these codes:

| Status | Code                                                                                                                                                                |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | `missing_model`, `invalid_body`, `unsupported_parameter`, `context_length_exceeded`, `subscription_model`                                                           |
| 401    | `missing_api_key`, `invalid_api_key`, `expired_api_key`, `revoked_api_key`, `subscription_key`, `missing_claude_sign_in`                                            |
| 402    | `budget_exceeded`, `credit_exhausted` ([billing.md](billing.md#pay-in-advance))                                                                                     |
| 403    | `filter_refused`, `subscriptions_off`, `org_limited`                                                                                                                |
| 404    | `model_not_found`: no such model, the key may not use it, or it is served at another endpoint; `mcp_server_not_found`; `not_found`: no such route; `wrong_base_url` |
| 415    | `unsupported_encoding`: a request body compressed other than with gzip or deflate                                                                                   |
| 413    | `request_too_large` (`KEERA_MAX_BODY_BYTES`), `filter_input_too_large`                                                                                              |
| 429    | `rate_limit_exceeded` (`rpm`), `token_rate_limit_exceeded` (`tpm`)                                                                                                  |
| 502    | `upstream_unavailable`, `upstream_error`, `filter_failed`, `mcp_credential_refused`, `mcp_unavailable`                                                              |
| 503    | `control_plane_unavailable`, `no_backend`, `filter_unavailable`, `router_undecided`, `router_destination_unavailable`                                               |

## Metrics

`/metrics` is the Prometheus exposition. It needs the operator key, an
operator's session, or `KEERA_METRICS_TOKEN`, because its labels name every
organisation.

| Metric                           | Labels             | What it counts                                                                   |
| -------------------------------- | ------------------ | -------------------------------------------------------------------------------- |
| `keera_requests_total`           | model, org, status | inference requests                                                               |
| `keera_tokens_total`             | model, org, status | input plus output tokens                                                         |
| `keera_request_duration_seconds` | model, org, status | wall time of a request                                                           |
| `keera_gateway_overhead_seconds` | model, org         | time the gateway added, not counting filters and routers                         |
| `keera_upstream_errors_total`    | model, org         | each failed attempt: a backend not reached or too slow, or a 5xx a router passed |
| `keera_inflight_requests`        |                    | requests waiting for the inference plane to start answering                      |
| `keera_ratelimit_fallback_total` |                    | rate-limit admissions and token charges made without Redis                       |

Filters, routers and MCP tool calls have their own metrics, listed in
[filters.md](filters.md#what-is-recorded), [routers.md](routers.md#what-is-recorded)
and [mcp.md](mcp.md#the-tool-call-log).

## What it deliberately does not do

- **It does not store prompts or completions.** What a filter read, what a
  router read, what a session was about - all of it is reduced to counts and
  hashes. See [filters.md](filters.md) and [sessions.md](sessions.md).
- **It is not a distributed trace.** The per-request breakdown in
  `internal/store/spans.go` has no sampling, no collector and no propagation,
  and every step it records happens inside one handler.
- **It does not translate request parameters between providers**, with three
  exceptions. It sends `max_completion_tokens` instead of `max_tokens` to every
  OpenAI chat model, and drops `temperature` and `top_p` for OpenAI's reasoning
  models (every id not starting with `gpt-4`, `gpt-3` or `chatgpt-`). It drops
  `temperature`, `top_p` and `top_k` for Anthropic's models since Opus 4.7.
  Those models refuse them. A client that asked for a temperature gets the
  default. Any other field a hosted provider rejects is an error the client
  sees.
- **It does not retry.** A model with several backends takes them in turn,
  round-robin, and moves to the next one only when it cannot connect, so a
  request reaches at most one. A fallback router moves on to its next
  destination [when one fails](routers.md#what-counts-as-a-failure). Nothing
  sends the same request to the same model twice.

## Failure modes worth knowing

**A passing chat test proves little.** A model can answer 200 with prose where a
coding agent needs `tool_calls`. See
[`keera model check`](install.md#keera-model-check-is-the-acceptance-test).

**The inference backend has no authentication.** vLLM, llama.cpp and
`laya-serve` have none by default. Anything that reaches port 8000 directly gets a model
with no key, no budget, no rate limit and no audit entry. Keep that port
unpublished in every deployment; the Helm chart can enforce this with a
NetworkPolicy.

**Usage events are the billing record.** If the recorder starts dropping them,
it logs an error saying so.
