# Hosted models

A hosted model runs outside your infrastructure: at Anthropic, OpenAI,
Infomaniak, stepping stone, Phoeniqs, CSCS, or any other endpoint that speaks
the OpenAI API.

To add one in the panel or on the command line, see
[Models](https://docs.keera.ch/manage/models/). This page explains how hosted
models work and why.

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

### In a template file

Each new organisation gets a copy of the models in the file. See
[Template files](gateway.md#template-files).

```yaml
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5-5
    description: >-
      Hosted outside this cluster and expensive. Design, multi-step reasoning
      and whole-system questions. Must not be sent client data.
```

The `description` is what a
[route step decides on](routers.md#descriptions-are-what-a-route-step-decides-on).
Without one, the model gets the provider's, which cannot say where the model
runs, since an entry may point at an egress proxy or a sovereign endpoint. So
write your own, as here.

`provider` fills in every field the entry leaves out: the endpoint, the context
window, the prices (input, output, cached input, cache write, and the dearer
ones for a long prompt where the provider charges more), a display name, a
description, the release date and the location. The providers are `anthropic`, `openai`,
`infomaniak`, `stepping-stone`, `phoeniqs` and `cscs`; `keera model catalogue`
lists them with their models. For a `backend_model` not in that table, the entry
must set `input_micros_per_mtok` and `output_micros_per_mtok` itself. A model
the table names without a date, such as `claude-haiku-4-5`, may also be named by
its dated id, `claude-haiku-4-5-20251001`.

The [location](gateway.md#the-alias-is-the-api-contract) is where the provider
serves its models: `ch` for Infomaniak, stepping stone, Phoeniqs and CSCS, `usa`
for Anthropic and OpenAI.

The entry always wins over the catalogue. So you can point
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

### In the panel and on the command line

[Models](https://docs.keera.ch/manage/models/) shows both. What it leaves out:

When the deployment holds a key for some providers ([Billing](billing.md)), the
catalogue shows only their models; **Show bring your own token** adds the ones
that need your own provider key. Operators and administrators add a model from
it to an organisation. Everyone else opens it with **Model catalogue** and can
only browse it. See
[Models belong to an organisation](gateway.md#models-belong-to-an-organisation).

The panel offers only the models in the table; add a newer one with
`keera model add` or a template file. The model remembers its provider, and
editing it later opens the same values. The provider cannot be changed there;
add a new model instead.

Infomaniak also asks for a product id and builds the endpoint from it:

```sh
keera model add keera-swiss --provider infomaniak --product-id 100234 \
  --backend-model swiss-ai/Apertus-v1.5-70B --api-key @-
```

For a **self-hosted model**, the fields under **Advanced options** default when
left empty: the kind is chat, the model is enabled, and the location is
`onprem` for a backend inside your network. A backend outside it needs a
location.

## Where the credential lives

If the deployment holds its own key for the provider, a model takes none: it
uses that key and is billed at list price. See [billing.md](billing.md). The
rest of this section is about keys an organisation enters itself.

The key is stored on the model, encrypted with AES-GCM under
[`KEERA_SECRET_KEY`](install.md#required), so a Postgres dump holds only
ciphertext. Each model holds its organisation's own key, and so does a model
from a [template file](gateway.md#template-files) once someone stores it.
Operators and the organisation's administrators can set it.

A stored key is only sent to the hosts it was entered for. A change that adds a
backend on another host is refused unless it gives the key again, or removes it
with `--no-api-key`. Otherwise an administrator could point a model someone
else set up at a host of their own and read its key.

The deployment's own provider keys only ever go to the provider's own endpoint
([billing.md](billing.md)).

## Put a guardrail on it

Without a guardrail, every key in the organisation can reach a hosted model;
see [Guardrails](https://docs.keera.ch/manage/guardrails/). A
[filter](filters.md) cleans or refuses what goes out, and a
[router](routers.md) keeps everyday work local.

## What the prices are, and are not

The catalogue holds each provider's published list price, in CHF, as
micro-units per million tokens. Use it as a starting point for showback, not as
a contract.

Each model has four prices: input, output, cached input (an input token the
provider served from its prompt cache) and cache write (an input token the
provider wrote to it).

List price is not your price, so put a negotiated rate, a discount or an
internal cross-charge on the entry. A model on the deployment's own key is the
exception: it is always billed at list price ([billing.md](billing.md)). The
prices go stale; see [billing.md](billing.md#keep-in-mind).

### Cached input

Anthropic and OpenAI charge less for a prompt they have already read, from a
fortieth to half of the input price, and report how much of the prompt that was.
OpenAI counts it as part of the prompt. Anthropic counts it separately, and the
gateway adds it back, so a row's input is always the whole prompt. The gateway
charges the cached part at the cached rate, so a row's cost matches the
provider's console.

Infomaniak, Phoeniqs and CSCS publish no cache discount, and stepping stone's
cache is not ready yet. Their cached column is empty and every input token is
charged at the input price. That is correct, not a gap.

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
was, each the way it reports a cache read. The gateway charges it at the
cache-write rate. The table has the rate for every model that charges one.
Anthropic also reports writes kept for an hour instead of five minutes; those
are charged at twice the input price.

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

A model entry can set its own:

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

## Known rough edges

### Anthropic and OpenAI requests are forwarded as they are

A Messages request to an Anthropic model, and a Responses request to an OpenAI
model, are forwarded without being translated
([gateway.md](gateway.md#three-request-shapes)). What that means per provider:

- **Anthropic.** The key is sent as `x-api-key`, and the client's
  `anthropic-version` and `anthropic-beta` headers are passed on. An egress
  proxy in front of Anthropic must pass `/v1/messages` as well as
  `/v1/chat/completions`. A Claude Team or Enterprise plan can pay instead; see
  [subscriptions.md](subscriptions.md).
- **OpenAI.** An egress proxy must pass `/v1/responses` as well as
  `/v1/chat/completions`. The deployment's own key changes more
  ([billing.md](billing.md#what-changes-for-a-model-of-that-provider)). The
  reasoning models take no `temperature`
  ([gateway.md](gateway.md#what-it-deliberately-does-not-do)).

### Also worth knowing

- **Embedding models need their own entry**, with no `provider`,
  `kind: embedding`, the full address in `backends` (for Infomaniak, with your
  product id in it), `location: ch` and their own prices.
- **Where to read more:** Phoeniqs on
  [documentation.phoeniqs.com](https://documentation.phoeniqs.com/maas/active-models/),
  CSCS on [docs.cscs.ch](https://docs.cscs.ch/services/inference/api/), its
  [prices](https://ui.inference.cscs.ch/pricing) and its
  [status page](https://inference.status.cscs.ch).

## What is recorded

A hosted model gets the same allow-lists, budgets, rate limits, filters,
routers, request log, sessions and audit entries as a local one.

Two places show the difference on purpose:

- [Live map](https://docs.keera.ch/monitor/live-map/) draws the edge of your
  network between local and hosted models.
- `keera usage --by model` shows one row per model in a terminal. It has no
  column for where a model runs, so tell hosted from local by the alias.
