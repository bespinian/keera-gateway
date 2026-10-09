# Hosted models

A hosted model runs outside your infrastructure: at Anthropic, OpenAI,
Infomaniak, stepping stone, Phoeniqs, CSCS, or any other endpoint that speaks
the OpenAI API.

**Prompts sent to a hosted model leave your infrastructure.** Treat it as a
deliberate exception. This page covers what to weigh before you offer one and
how to keep it contained.

Infomaniak, stepping stone, Phoeniqs and CSCS serve open-weight models from
their own data centres in Switzerland, so those prompts stay under Swiss law. Prompts to
Anthropic and OpenAI do not. Everything below applies to all of them.

## Why offer one at all

**Work a small local model cannot do.** A 7B coding model handles renames, short
edits and everyday questions. It is weak at design, multi-step reasoning and
whole-system questions. If a project may only use the local model, it will stop
using the platform or go around it.

**Evaluating before there are GPUs.** You can set up a deployment, issue keys,
write guardrails and read reports against a hosted model before any hardware
arrives.

## How to declare one

### In the catalogue file

Each new organisation gets a copy of the models in the file. See
[gateway.md](gateway.md#models-belong-to-an-organisation).

```yaml
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
    description: >-
      Hosted outside this cluster and expensive. Design, multi-step reasoning
      and whole-system questions. Must not be sent client data.
```

`provider` fills in every field the entry leaves out: the endpoint, the context
window, the prices (input, output, cached input, cache write, and the dearer
ones for a long prompt where the provider charges more), a description, the release date and
the location. The providers are `anthropic`, `openai`,
`infomaniak`, `stepping-stone`, `phoeniqs` and `cscs`; `keera model providers`
lists them with their models. For a `backend_model` not in that table, the entry
must set `input_micros_per_mtok` and `output_micros_per_mtok` itself. A model
the table names without a date, such as `claude-haiku-4-5`, may also be named by
its dated id, `claude-haiku-4-5-20251001`.

The location is where the provider serves its models: `ch` for Infomaniak,
stepping stone, Phoeniqs and CSCS, `usa` for Anthropic and OpenAI. A model with
no provider is `onprem` when its backend is inside your network: a service name,
a private address or the loopback. A backend on the internet does not say which country
it is in, so an entry for one must set `location` itself.

The entry always wins over the provider table. So you can point
`provider: anthropic` at a corporate egress proxy or a sovereign endpoint, or
set your internal price. If that endpoint runs somewhere else, set `location`
too.

#### One provider needs a product id

Infomaniak's URL contains the product id of your own AI Service, so an entry for
it needs one more field.

```yaml
models:
  - alias: keera-swiss
    provider: infomaniak
    # From the Infomaniak console, under AI Tools. It is part of the address
    # rather than a secret - the key is separate.
    product_id: "100234"
    backend_model: swiss-ai/Apertus-v1.5-70B
```

An entry with its own `backends` needs no `product_id`. If that address has the
same shape (for example an egress proxy that mirrors the path), write
`{product_id}` in it and the id is filled in.

### In the panel

**Models → Add model** first asks where the model runs: one tile per provider,
and one for your own inference plane. The rest of the form appears after you
pick one.

For a provider, you enter the model names and the API key. The endpoint,
context window and prices are filled in and folded away
under **Advanced options**. The model remembers its provider, so editing it
later opens the same tile with the same values.

Infomaniak also asks for a product id and builds the endpoint from it.

**Models → Model catalogue** lists every model the providers offer, with its
release date, location, context window and list prices, as `keera model
providers` does. You can search it, filter it by provider, and sort it by any
column. **Add** opens the same form with the provider and the model already
picked. Everyone can see the list. Operators and
administrators can add a model from it to an organisation. See
[Models belong to an organisation](gateway.md#models-belong-to-an-organisation).

### On the command line

```sh
keera model add keera-frontier --provider anthropic --backend-model claude-opus-5
keera model set keera-frontier --api-key @-      # reads the key from stdin

# Infomaniak, which also needs the product id its address carries.
keera model add keera-swiss --provider infomaniak --product-id 100234 \
  --backend-model swiss-ai/Apertus-v1.5-70B --api-key @-
```

Use `@-`: a key passed as a flag value ends up in the shell history.

An administrator adds the model to their own organisation. An operator passes
`--org <id>`, unless there is only one organisation.

## Where the credential lives

If the deployment holds its own key for the provider, a model takes none: it
uses that key and is billed at list price. See [billing.md](billing.md). The
rest of this section is about keys an organisation enters itself.

The key is stored on the model, encrypted with AES-GCM under
`KEERA_SECRET_KEY`, so a Postgres dump holds only ciphertext. Set it in the
panel or with `keera model set <alias> --api-key @-`. Each model holds its
organisation's own key. Operators and the organisation's administrators can set
it.

If you change `KEERA_SECRET_KEY` later, every stored credential becomes
unreadable and must be entered again.

A catalogue file never holds a key. When an organisation gets a model from the
file, store its key with the command above. `keera model apply` keeps a key
that is already stored.

A stored key is only sent to the hosts it was entered for. A change that adds a
backend on another host is refused unless it gives the key again, or removes it
with `--no-api-key`. Otherwise an administrator could point a model someone
else set up at a host of their own and read its key.

A model cannot name one of the gateway's environment variables as its key. It
would let an administrator send any of the gateway's secrets, such as
`KEERA_SECRET_KEY`, to a server of their choice. The deployment's own provider
keys are the one exception, and they only ever go to the provider's own
endpoint ([billing.md](billing.md)).

An Infomaniak product id is not a secret. It goes in the catalogue entry next to
the model id.

## Put a guardrail on it

Without a guardrail, every key in the organisation can reach a hosted model. At
minimum:

```sh
# Only this project may use it, and only up to a monthly budget.
keera guardrail set project <project-id> --models keera-speed,keera-frontier \
  --budget 500 --period month
```

Then consider:

- A [filter](filters.md) removes credentials, client names and account numbers
  from the request, or refuses it.
- A [router](routers.md) decides which requests go to the hosted model at all,
  so everyday work stays local.

Read both before you offer a hosted model to anyone.

## What the prices are, and are not

The provider table holds each provider's published list price, in its own
currency, as micro-units per million tokens. Use it as a starting point for
showback, not as a contract.

Each model has four prices: input, output, cached input (an input token the
provider served from its prompt cache) and cache write (an input token the
provider wrote to it).

- **The currency may not be yours.** Anthropic and OpenAI are in USD, Infomaniak,
  stepping stone, Phoeniqs and CSCS in CHF. Nothing is converted. Override the prices on the
  entry.
- **List price is not your price.** Put a negotiated rate, a discount or an
  internal cross-charge on the entry. A model on the deployment's own key is
  the exception: it is always billed at list price ([billing.md](billing.md)).

The prices go stale. They are a table in a Go file, not a live feed. The comment
above the table says when they were last checked.

### Cached input

Anthropic and OpenAI charge less for a prompt they have already read, usually a
tenth of the input price or less, and report how much of the prompt that was.
OpenAI counts it as part of the prompt. Anthropic counts it separately, and the
gateway adds it back, so a row's input is always the whole prompt. The gateway
charges the cached part at the cached rate, so a row's cost matches the
provider's console.

stepping stone publishes a cache rate too. The gateway charges it for whatever
the response reports as cached, although stepping stone bills it only from
about the end of October 2026 (`keera model providers` says so).

Infomaniak, Phoeniqs and CSCS publish no cache discount. Their cached column is empty and every
input token is charged at the input price. That is correct, not a gap.

This price matters most. A coding agent resends its whole context on every
turn, so most of a long session is cached input. Charging it at the full input
price overstates the session several times.

A cached price of zero does not mean free. It means no price was set, and those
tokens are charged at the full input price, so a forgotten price never drops
the cached half of every prompt from a budget.

```sh
# A negotiated rate, and the discount that goes with it.
keera model set keera-frontier --price-in 4 --price-cached 0.4
```

A model in your own infrastructure reports no cache and needs no cached price.

### Cache writes

Anthropic, and OpenAI from `gpt-5.6` on, charge 25% more than the input price
for the part of the prompt they write to their cache. Both report how much that
was, as part of the prompt like a cache read. The gateway charges it at the
cache-write rate. The table has the rate for every model that charges one.

Like the cached price, a cache-write price of zero means none was set, and
those tokens are charged at the input price. That is right for the providers
that charge nothing extra.

```sh
keera model set keera-frontier --price-cache-write 5
```

### Long prompts

Some models cost more per token when the prompt is long. `claude-haiku-5-5`
costs five times as much once the prompt is over 100,000 tokens, and most of
OpenAI's models cost more over 272,000. The prompt
counts every input token, cached or not, as the provider counts it. The table
holds both sets of prices for such a model, and the gateway charges the whole
request at the dearer ones past the threshold: input, cached input, cache write
and output.

A catalogue entry can set its own:

```yaml
    long_prompt:
      above_tokens: 100000
      input_micros_per_mtok: 500000
      output_micros_per_mtok: 2500000
      cached_input_micros_per_mtok: 50000
      cache_write_micros_per_mtok: 625000
```

`keera model list --json` (as `long_prompt`) and the control panel show them. The CLI has no flags for
them: `--price-in` and the others change only the normal prices.

## Descriptions

A model declared through a provider starts with that provider's description. It
is written for the comparison a [router](routers.md) makes: speed, cost,
thoroughness.

It cannot say where the model runs, because an entry may point
`provider: anthropic` at an egress proxy or a sovereign endpoint. So rewrite it:

```sh
keera model set keera-frontier --description \
  "hosted outside this cluster and expensive; design and multi-step reasoning. \
Must not be sent client data."
```

## Known rough edges

`keera model providers`, and the panel next to each provider, show what to
know about each one: pricing quirks, model ids, beta models and what the table
leaves out. Below is what those notes do not say.

### Anthropic and OpenAI requests are forwarded as they are

**Messages clients reach Anthropic's own API.** A request to `/api/v1/messages`
(what Claude Code sends) is forwarded to Anthropic's `/v1/messages` without
being translated. The guardrails still apply to it in its own shape: filter
rewrites, the system prompt, the output ceiling and blocked hosted tools.
Prompt caching and thinking work, and cached input is charged at the cached
rate. The key is sent as `x-api-key`, and the client's `anthropic-version` and
`anthropic-beta` headers are passed on. An egress proxy in front of Anthropic
must pass `/v1/messages` as well as `/v1/chat/completions`. A Claude Team or
Enterprise plan can pay instead; see [subscriptions.md](subscriptions.md).

**Responses clients reach OpenAI's own API.** A request to `/api/v1/responses`
(what Codex sends) is forwarded to OpenAI's `/v1/responses` with its reasoning
items. Besides the guardrails, as for Anthropic, the only change is that a
reasoning model gets no `temperature` or `top_p`. An egress proxy in front of
OpenAI must pass `/v1/responses` as well as `/v1/chat/completions`.

**The OpenAI reasoning models take no temperature.** Every OpenAI model whose
id does not start with `gpt-4`, `gpt-3` or `chatgpt-` refuses `max_tokens`, and
any `temperature` or `top_p` other than the default. So the gateway sends
`max_completion_tokens` instead of `max_tokens` to every OpenAI chat model, and
drops `temperature` and `top_p` for the reasoning models. A client that asked
for a temperature gets the default.

### Also worth knowing

- **A promotional price ends.** After `gpt-5.6-sol`'s promotion, the table's
  price is too low until a new build, or until you set the price yourself.
- **Upstream model ids are not aliases.** Infomaniak, stepping stone and CSCS
  use names with capitals, such as `swiss-ai/Apertus-v1.5-70B`. Choose your own
  alias; the panel suggests a lowercase one.
- **Embedding models need their own entry**, with no `provider`,
  `kind: embedding`, the full address in `backends` (for Infomaniak, with your
  product id in it), `location: ch` and their own prices.
- **Where to read more:** Phoeniqs on
  [documentation.kvant.cloud](https://documentation.kvant.cloud/maas/active-models/),
  CSCS on [docs.cscs.ch](https://docs.cscs.ch/services/inference/api/), its
  [prices](https://ui.inference.cscs.ch/pricing) and its
  [status page](https://inference.status.cscs.ch).

## What is recorded

A hosted model gets the same allow-lists, budgets, rate limits, filters,
routers, request log, sessions and audit entries as a local one.

Two places show the difference on purpose:

- **Live map** draws a line between models served from your own network (above)
  and hosted providers (below). The line shows the share of the window's tokens
  that crossed it and what they cost.
- `keera usage --by model` shows one row per model in a terminal. It has no
  column for where a model runs, so tell hosted from local by the alias.
