# Hosted models

A hosted model runs outside your infrastructure: at Anthropic, OpenAI,
Infomaniak, stepping stone, or any other endpoint that speaks the OpenAI API.

**Prompts sent to a hosted model leave your infrastructure.** Treat it as a
deliberate exception. This page covers what to weigh before you offer one and
how to keep it contained.

Infomaniak and stepping stone serve open-weight models from their own data
centres in Switzerland, so those prompts stay under Swiss law. Prompts to
Anthropic and OpenAI do not. Everything below applies to all of them.

## Why offer one at all

**Work a small local model cannot do.** A 7B coding model handles renames, short
edits and everyday questions. It is weak at design, multi-step reasoning and
whole-system questions. If a team may only use the local model, it will stop
using the platform or go around it.

**Evaluating before there are GPUs.** You can set up a deployment, issue keys,
write guardrails and read reports against a hosted model before any hardware
arrives.

## How to declare one

### In the catalogue file

```yaml
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
    description: >-
      Hosted outside this cluster and expensive. Design, multi-step reasoning
      and whole-system questions. Must not be sent client data.
```

`provider` fills in every field the entry leaves out: the endpoint, the
credential variable, the context window, the three prices (input, output and
cached input) and a description. `keera model providers` lists the providers
this build knows.

The entry always wins over the provider table. So you can point
`provider: anthropic` at a corporate egress proxy or a sovereign endpoint, or
set your internal price.

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

**Models → New model** first asks where the model runs: one tile per provider,
and one for your own inference plane. The rest of the form appears after you
pick one.

For a provider, you enter the model names and the API key. The endpoint,
credential variable, context window and prices are filled in and folded away
under **Advanced options**. The model remembers its provider, so editing it
later opens the same tile with the same values.

Infomaniak also asks for a product id and builds the endpoint from it.

### On the command line

```sh
keera model add keera-frontier --provider anthropic --backend-model claude-opus-5
keera model set keera-frontier --api-key @-      # reads the key from stdin

# Infomaniak, which also needs the product id its address carries.
keera model add keera-swiss --provider infomaniak --product-id 100234 \
  --backend-model swiss-ai/Apertus-v1.5-70B --api-key @-
```

Use `@-`: a key passed as a flag value ends up in the shell history.

## Where the credential lives

**Stored on the model, encrypted.** This is what the panel does. The key is
encrypted with AES-GCM under `KEERA_SECRET_KEY` and stored in the database, so a
Postgres dump holds only ciphertext.

Without `KEERA_SECRET_KEY`, the panel's field is disabled and the gateway
refuses to store a credential. If you change the key later, every stored
credential becomes unreadable and must be entered again.

**Named as an environment variable.** Set `api_key_env` on the model. The
defaults are `ANTHROPIC_API_KEY` for `provider: anthropic`, `OPENAI_API_KEY`
for `provider: openai`, `INFOMANIAK_API_KEY` for `provider: infomaniak` and
`STEPPING_STONE_API_KEY` for `provider: stepping-stone`. The
secret comes from the deployment (a Kubernetes Secret, a vault, a systemd
environment file) and never enters the catalogue or the database.

Use the environment variable for a GitOps or NixOS deployment that keeps its
catalogue as a file. Use the stored key otherwise, so the credential does not
end up in a shell history, a ticket or a chat message.

An Infomaniak product id is not a secret. It goes in the catalogue entry next to
the model id.

## Put a guardrail on it

Without a guardrail, every key in the organisation can reach a hosted model. At
minimum:

```sh
# Only this team may use it, and only up to a monthly budget.
keera guardrail set team <team-id> --models keera-speed,keera-frontier \
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

Each model has three prices: input, output, and cached input (an input token
the provider served from its prompt cache).

- **The currency may not be yours.** Anthropic and OpenAI are in USD, Infomaniak
  and stepping stone in CHF. Nothing is converted. Override the prices on the
  entry.
- **List price is not your price.** Put a negotiated rate, a discount or an
  internal cross-charge on the entry.

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
the response reports as cached; see [stepping stone](#stepping-stone).

Infomaniak publishes no cache discount. Its cached column is empty and every
input token is charged at the input price. That is correct, not a gap.

This price matters most. A coding agent resends its whole context on every
turn, so most of a long session is cached input. Charging it at the full input
price overstates the session several times.

A cached price of zero does not mean free. It means no price was set, and those
tokens are charged at the full input price. This is deliberate: models declared
before this field existed have a zero, and treating it as free would drop the
cached half of every prompt from every budget on upgrade.

```sh
# A negotiated rate, and the discount that goes with it.
keera model set keera-frontier --price-in 4 --price-cached 0.4
```

A model in your own infrastructure reports no cache and needs no cached price.

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

These come from the providers' own endpoints. `keera model providers` prints
them as a note next to each provider.

### Anthropic

**Messages clients reach Anthropic's own API.** A request to `/api/v1/messages`
(what Claude Code sends) is forwarded unchanged to Anthropic's `/v1/messages`.
Prompt caching and thinking work, and cached input is charged at the cached
rate. The key is sent as `x-api-key`, and the client's `anthropic-version` and
`anthropic-beta` headers are passed on. An egress proxy in front of Anthropic
must pass `/v1/messages` as well as `/v1/chat/completions`.

**A cache write is charged at the input price.** Anthropic charges 25% more for
it, and the gateway has no separate rate. A session that writes a lot to the
cache is billed slightly low.

**Server tools run on Anthropic's side.** A request can ask Anthropic to search
the web, fetch a URL or connect to an MCP server. Filters see the prompt, not
what Anthropic fetches. A guardrail with `--block-hosted-tools` removes those
tools from the request; see [mcp.md](mcp.md#hosted-tools).

**Every other client uses the OpenAI-compatible endpoint.** Anthropic calls it a
layer for evaluation, not production, and prompt caching does not work there.
An agent on it pays full price for every resent token.

### OpenAI

Prompt caching works, and the gateway charges the discounted rate for the
discounted part (see [Cached input](#cached-input)). A request to
`/api/v1/responses` (what Codex sends) is forwarded unchanged to OpenAI's
`/v1/responses`, with its reasoning items.

**Reasoning tokens are billed as output.** OpenAI counts them in
`completion_tokens`, which Keera prices at the output rate, so the cost is
right. But they cannot be told apart: the request log does not show how much of
the cost was reasoning.

**The reasoning models take no temperature.** OpenAI's reasoning models (every
model after `gpt-4`) refuse `max_tokens`, and any `temperature` or `top_p` other
than the default. So the gateway sends `max_completion_tokens` instead of
`max_tokens` to every OpenAI model, and drops `temperature` and `top_p` for the
reasoning models. A client that asked for a temperature gets the default.

**Built-in tools run on OpenAI's side.** A Responses request can ask OpenAI to
search the web or connect to an MCP server. As with Anthropic, filters do not
see what OpenAI fetches, and `--block-hosted-tools` removes these tools.

**`gpt-5.5` and `gpt-5.4` have two prices, and the table holds one.** Both cost
about twice as much per token above 272k input tokens. The table has the lower
price, so long requests are billed low. Set the price yourself if that matters;
see [What the prices are, and are not](#what-the-prices-are-and-are-not).

### Infomaniak

**The endpoint is yours, not one endpoint.** It contains your AI Service's
product id, so an entry needs `product_id` as well as a key. Both are in the
Infomaniak console under AI Tools. A wrong id gives a 404 on every request.

**Model ids are the upstream projects' own names**, with capitals:
`swiss-ai/Apertus-v1.5-70B`, not `apertus`. `keera model providers` lists them.
They are not valid aliases, so choose your own alias; the panel suggests a
lowercase one.

**No prompt cache is priced.** See [Cached input](#cached-input).

**The table covers the chat models only.** Infomaniak also serves embedding,
re-ranking, transcription and image models on the same endpoint. They work:
declare one with `kind: embedding`, its own `backends` or `product_id`, and its
own prices. This build has no defaults for them.

### stepping stone

**One key per model.** stepping stone issues a separate API key for each model.
Store each key on its model, or give each model its own `api_key_env`; the
default `STEPPING_STONE_API_KEY` can serve only one of them.

**The cache discount is not billed yet.** The table has stepping stone's cached
input rates, but stepping stone only starts to bill them at about the end of
October 2026. Until then it charges the full input price, so a cached prompt
costs more on the invoice than in Keera.

**Model ids are the upstream projects' own names**, with capitals, as for
Infomaniak. Nemotron is `NVIDIA/NVIDIA-Nemotron-3-Super-120B-A12B`.

**The table covers the chat models only**, including the three OCR models,
which read images of pages and are no use for chat or code. stepping stone also
serves embedding, re-ranking, audio and image models on the same endpoint.
Declare one with `kind: embedding` and its own prices.

## What is recorded

A hosted model gets the same allow-lists, budgets, rate limits, filters,
routers, request log, sessions and audit entries as a local one.

Two places show the difference on purpose:

- **Live map** draws a line between models served from your own network (above)
  and hosted providers (below). The line shows the share of the window's tokens
  that crossed it and what they cost.
- `keera usage --by model` shows the same split in a terminal.
