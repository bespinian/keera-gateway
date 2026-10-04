# Claude subscriptions

People on a Claude Team or Enterprise plan can use Claude Code through Keera
Gateway, and their plan pays. Keera still applies the guardrails, records every
request, and shows how much of the plan each person uses.

This works for Claude Code only. Anthropic allows a Claude sign-in in Claude
Code and Claude.ai, and nowhere else. Claude.ai in the browser and the desktop
app do not go through Keera.

## How it works

Claude Code is signed in to the person's Claude plan, and its address is set to
the gateway. Each request then carries two credentials:

| Header          | Holds                           | What Keera does with it                                                 |
| --------------- | ------------------------------- | ----------------------------------------------------------------------- |
| `X-Keera-Key`   | The person's subscription key   | Finds who is asking, applies their guardrails, and does not send it on. |
| `Authorization` | The person's own Claude sign-in | Sends it to Anthropic unchanged. It is never stored and never logged.   |

Claude Code sends `X-Keera-Key` from `ANTHROPIC_CUSTOM_HEADERS`. The sign-in
token says nothing about who it belongs to, so the key is what tells Keera.

## 1. Add a subscription model

An administrator adds one model per Claude model the plan should reach:

```sh
keera model add claude-opus --provider anthropic \
    --backend-model claude-opus-5-5 --subscription
keera model add claude-haiku --provider anthropic \
    --backend-model claude-haiku-4-5 --subscription
```

In a catalogue file:

```yaml
models:
  - alias: claude-opus
    provider: anthropic
    backend_model: claude-opus-5-5
    subscription: true
```

In the panel, choose **Anthropic** as where the model runs, then tick **Paid by
each person's Claude subscription**.

A subscription model:

- must name the provider `anthropic`, and its only backend is Anthropic's own
  API. Every person's sign-in goes there, so no other address is allowed. To
  go through an egress proxy, set `HTTPS_PROXY` on the gateway.
- stores no API key.
- answers only the Anthropic Messages API, which Claude Code speaks.
- cannot be a filter's model, a router's model, or a router destination.

Claude Code asks for some background work by Anthropic's own model name,
whatever its settings say. A subscription key that names one, such as
`claude-haiku-4-5-20251001`, reaches the subscription model with that backend
model. The date at the end does not matter.

If the organisation's guardrails list allowed models, add the new ones there.

## 2. Send everyone's Claude Code through Keera

An Owner of the Claude organisation sets the address for everyone, in the
claude.ai admin console under **Admin Settings > Claude Code > Managed
settings**:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://keera.example.ch/api"
  }
}
```

Claude Code asks each person to approve the change once, and quits if they
refuse. A person cannot override it. From then on, every request goes to Keera,
and Keera refuses those without a subscription key, saying what to run.

On laptops the company manages, `managed-settings.json` does the same, and
`forceLoginOrgUUID` stops people signing in with a personal account. See
Anthropic's [managed settings](https://code.claude.com/docs/en/managed-settings).

From Claude Code v2.1.285, `allowedProviders` also refuses a session that is
pointed anywhere else, Anthropic included:

```json
{
  "allowedProviders": ["customEndpoint"],
  "env": {
    "ANTHROPIC_BASE_URL": "https://keera.example.ch/api"
  }
}
```

The address must be in the same managed settings as the list.

These are controls on the person's machine, not a security boundary. Someone
on an unmanaged laptop can get around them. Test the setup with one account
before rolling it out.

## 3. Each person, once per machine

When the address comes from the claude.ai admin console, run `claude` and
`/login` first, and approve the managed settings. Claude Code then has a copy
of them, so the command below sees the address. Then run:

```sh
keera login
keera connect claude-code --subscription --model claude-opus
```

This issues the machine its own subscription key, named
`claude-code on <hostname> (<id>)`. The id is random and kept in keera's
configuration directory, so machines that share a hostname keep their own
keys. It writes the key into `~/.claude/settings.json` (or
`$CLAUDE_CONFIG_DIR/settings.json`), so nobody has to paste it:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://keera.example.ch/api",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Keera-Key: keera_sk_…",
    "ANTHROPIC_MODEL": "claude-opus[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-opus[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku",
    "ENABLE_TOOL_SEARCH": "true"
  },
  "modelOverrides": {
    "claude-opus-5-5": "claude-opus",
    "claude-haiku-4-5": "claude-haiku",
    "claude-haiku-4-5-20251001": "claude-haiku"
  }
}
```

Claude Code asks for Opus, Sonnet and Haiku models by Anthropic's names. Each is
pointed at the subscription model of that family, or at the main model when the
organisation has none. `--model` picks the main model. Without it, the main
model is the first subscription model by alias.

Claude Code does not know the gateway's aliases. `modelOverrides` tells it
which Claude model each alias is, so it knows what the model supports and how
large its context is. Without it, Claude Code would send Haiku adaptive
thinking, which Haiku refuses. Claude Code knows a dated model, such as Haiku
4.5, only by its dated id, so that one is written too. Entries for other models
are kept.

A model newer than the person's Claude Code is not known to it, and is taken
to have a 200K context. So a model with a 1M context also gets `[1m]` after its
alias. Claude Code takes it off before it sends the request.

Claude Code also turns tool search off when its address is not Anthropic's.
Then it sends every tool of every MCP server with each request, which can fill
the context before the first answer. The gateway sends a subscription request
on as it is, so `ENABLE_TOOL_SEARCH` turns it back on.

`ANTHROPIC_BASE_URL` is left out, or taken out, when managed settings set it
already. Claude Code stops fetching the settings from the admin console when the person's own
settings name an address, so the organisation's settings would no longer
apply. The command reads Claude Code's copy of the admin console's settings and
the `managed-settings.json` files. It does not read MDM profiles or the Windows
registry. `claude doctor` shows whether the fetch works, on its
`Managed settings (remote)` line.

The rest of the file is kept. Running the command again replaces the machine's
key. A member's key uses the organisation's guardrails. An administrator can
put it in a team with `--team`.

Then run `claude`, and `/login` if it is not signed in to the plan yet.
`ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY` and `apiKeyHelper` must not be set:
Claude Code would send them instead of the Claude sign-in. The command warns
when it finds one.

## What a subscription key can do

- It reaches subscription models only. It cannot spend the organisation's money,
  because it sits in a settings file in plain text.
- It reaches no router, no MCP server and not the playground.
- It must be sent in `X-Keera-Key`, next to a Claude sign-in.
- It always belongs to a person. Disabling the person revokes it.
- Nothing ties it to one Claude account. The token cannot be checked without
  asking Anthropic, so a key sent with someone else's sign-in is recorded as
  the key holder's.

`keera key create --subscription` issues one by hand. The panel marks these
keys **Claude plan**.

## What it costs

The plan pays a fixed fee, so a subscription request costs the organisation
nothing:

- **Spend** is 0, and no budget is charged or checked. Filters still spend the
  organisation's money, so a key with filters still meets its budgets.
- **What the plan paid for** is what the same requests would have cost on
  Anthropic's API, from the model's prices. It is never charged. It shows
  whether the plan is worth it. The dashboard shows it next to spend as
  "on Claude plans, at API prices", and the keys screen per key as "on the
  plan".
- **How much of the plan is used** is the share of the plan's usage limit, as
  Anthropic reports it on each answer: the last five hours and the last seven
  days. The keys screen shows it as "plan 5h 40% · 7d 12%", and `keera key list`
  shows it too.

Token counts and rate limits work as for any other model.

## What is sent to Anthropic

The request goes as Claude Code sent it: its `Authorization`, `User-Agent`,
`X-App`, every `anthropic-*` and every `x-claude-code-*` header. A guardrail's
system prompt goes after Claude Code's own, not before: Anthropic removes
Claude Code's first block only when it comes first, and the request should
reach Anthropic as Claude Code built it.

Anthropic's plan headers (`anthropic-ratelimit-unified-*`, `retry-after` and
`x-should-retry`) are passed back, so Claude Code shows the plan's limits.

## What changes for Claude Code

- Remote Control is off while the address is not Anthropic's.
- Some checks still go straight to `api.anthropic.com`, not through Keera: the
  one for fast mode, and the one WebFetch runs before it fetches a page. The
  network must let them through, or those features fail.
- A web application firewall in front of Keera must not inspect the bodies
  sent to `/api/v1/messages`. Prompts carry code and tags that look like
  attacks, so real sessions get a 403 while short tests pass.

See Anthropic's [gateway guide](https://code.claude.com/docs/en/llm-gateway)
for the rest.

## Errors

| Status | Says                                                 | Means                                                               |
| ------ | ---------------------------------------------------- | ------------------------------------------------------------------- |
| 401    | carries a Claude sign-in but no Keera key            | Run `keera login`, then `keera connect claude-code --subscription`. |
| 401    | this is a subscription key                           | The key went in `Authorization`. It belongs in `X-Keera-Key`.       |
| 401    | carries no Claude sign-in (`missing_claude_sign_in`) | Run `/login` in Claude Code, and unset `ANTHROPIC_AUTH_TOKEN`.      |
| 400    | `subscription_model`                                 | Only the Messages API reaches a subscription model.                 |
| 404    | `model_not_found`                                    | A subscription key named an organisation model, or the reverse.     |
| 404    | `model_not_found`                                    | Claude Code named a Claude model no subscription model serves.      |

`keera model check` cannot check a subscription model, because the gateway
holds no credential for it. Send it a message from Claude Code instead.
