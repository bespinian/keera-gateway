# Routers

A router is an alias for a list of models. A request that names it is sent to
one model from that list. Use one to send small requests to a cheap model, or to
keep sensitive prompts away from a model outside the cluster.

There are five modes:

| Mode                        | Picks the model by                             | Extra cost                 |
| --------------------------- | ---------------------------------------------- | -------------------------- |
| `instruction` (the default) | asking a small model to read the request       | one generation per request |
| `size`                      | the length of the request                      | only failed attempts       |
| `fallback`                  | the order the destinations are written         | only failed attempts       |
| `latency`                   | each one's recent time to first token          | only failed attempts       |
| `least-busy`                | the requests the gateway has in flight to each | only failed attempts       |

Only `instruction` reads what the request says. The other four put the
destinations in an order and try them until one answers.

Most of this page is about `instruction` routers. The other modes have their own
sections - [routing by size](#routing-by-size),
[the fallback router](#the-fallback-router) and
[balancing between equals](#balancing-between-equals). Everything before them
that is not about deciding applies to all five.

## How a request reaches one

A client puts the router's alias in the `model` field:

```json
{ "model": "auto", "messages": [{ "role": "user", "content": "…" }] }
```

`keera connect opencode --model auto` writes it into an editor's configuration
like any alias. `GET /api/v1/models` lists routers next to the models, with
their destinations.

Unlike a filter, a router is not attached to a scope. If it were, a developer
who asked for the large model could get the small one without knowing. To route
everything a scope sends, narrow its allow-list to the router:

```sh
keera guardrail set team <team-id> --models auto
```

That is why guardrails have no router field.

## Allowing a router allows its destinations

A key that may use `auto` may reach every destination of `auto`, whatever its
own allow-list says. The setup above needs this: a key narrowed to the router
has nothing else in its allow-list.

The panel marks routers in an allow-list and shows where they send.

## The list decides, not the instruction

The deciding model's answer is looked up in `destinations`. If it names anything
else, the router has not decided and the request goes to
[the fallback](#when-it-cannot-decide). A prompt cannot talk the router into a
model outside its list.

So `keera router list` and the panel's **Destinations** column show where a
router can send a prompt. The instruction only affects which one it picks.

Also:

- A destination the catalogue cannot serve - missing, disabled, not a chat
  model, no backend - is **left out of the choice**. `keera router check`
  reports what was and was not offered.
- The **fallback must be one of the destinations**.

## Descriptions are what a router decides on

The deciding model sees each destination's own model `description`:

```sh
keera model set keera-speed --description \
  "fast, local; short edits, syntax questions, everyday work"
keera model set keera-frontier --description \
  "hosted and expensive; design, multi-step reasoning, whole-system questions"
```

The description lives on the model so it stays true when the model behind an
alias changes. Clients see it on `/api/v1/models` too.

Write the descriptions before the router. Without them the router chooses
between bare aliases, and `keera router check` warns about it.

A model from a hosted provider starts with the provider's description
(`keera model providers` lists them). Rewrite it to say what this deployment
uses the model for.

## Where it runs

Before the filters:

1. authenticate, rate-limit, budget
2. **the router**, choosing the destination
3. the filters, in the order the hierarchy gives them
4. the standing system prompt is prepended
5. the output ceiling is clamped
6. forward

The router reads what the client sent, before any redaction. So **the deciding
model must be served locally**, just like a filter's.

It runs after the budget check because deciding costs money. A
[size router](#routing-by-size) costs nothing but runs in the same step.

## When it cannot decide

Choose one when writing the router:

| `--fallback <destination>`                | `--no-fallback`                    |
| ----------------------------------------- | ---------------------------------- |
| the request goes there without a decision | the request is refused with 503    |
| recorded as `fallback`                    | recorded as `error`                |
| the sender gets an answer                 | the sender gets `router_undecided` |

A router that saves money usually has a safe fallback. A router that keeps
prompts inside the cluster does not: sending an unclassified prompt to a fixed
model is the failure it exists to prevent.

Filters always fail closed. Routers get a choice because there may be a safe
place to send the request.

A router cannot decide when:

- its model is missing, disabled, not a chat model, or has no backend
- its model errored, timed out, or answered nothing
- its answer names none of its destinations
- its answer names two destinations at once
- the request has no text to read
- the request does not fit in the deciding model's context

A separate case is a chosen destination that cannot serve the request:
`router_destination_unavailable`, 503. This is almost always the fallback, since
only reachable destinations are offered to the model.

Either way, the reason is stored on the request's usage row.

## Cost and latency

**This section is about `instruction` only.** The other modes generate nothing;
a request pays only for the destinations that failed before one answered.

The decision happens before anything is forwarded, so it adds to time to first
token. What it spends is charged to the same budgets and added to the request's
`cost_micros`.

A filter costs a generation only on the requests its guardrails cover. **A
router costs one on every request that names it.**

So the router reads at most **the newest 24 KiB** of the request's segments. It
reads from the end because the latest ask is what matters; a coding agent's
conversation is mostly files it has already read. A single segment larger than
the window is sent whole.

The deciding model gets a small, fixed output budget. `keera router check` shows
what its runs cost, which is what every request naming the router pays on top.

### How the answer is read

The destinations are offered as letters - `A) keera-small`, `B) keera-large` -
and the model is asked for the letter only. The gateway reads the probabilities
for that one token and takes the most likely letter.

This costs **one** output token instead of up to 64, and the answer is always a
destination.

It needs a backend that returns logprobs. vLLM and llama.cpp do; most hosted
providers do not. There is nothing to configure: the gateway tries this first,
and if a backend does not return logprobs it asks that model for a name from
then on. A hosted deciding model costs one retried decision per gateway process.

The probabilities also show **how sure** the model was. `keera router check`
shows this - see [checking one](#checking-one).

## Writing an instruction

**Describe kinds of requests, not models.** The descriptions say which model
suits which.

Say which way to err. The small model will sometimes be wrong. The instruction
should say whether a wrong choice should cost money or risk data leaving.

The panel's form offers this:

```
Choose the model that suits this request.

Send small, self-contained requests to the cheapest capable model: a rename, a
syntax question, a short edit to one file, a commit message.

Send requests needing design, several steps at once, or judgement about a system
to a model that can reason.

Send anything containing a client's name, an account number, personal details or
a credential to a model inside the cluster, whatever else it is asking for. That
takes priority.

If you are unsure, choose the more capable model rather than the cheaper one.
```

Limits:

- The instruction is capped at 4 KiB, a quarter of a filter's. It is sent with
  the destination list and every description on every request that names the
  router.
- At most 16 destinations. With longer lists, a small model tends to pick
  whichever end it read last.

## Routing by size

Request length often shows how much model a request needs, and it costs nothing
to measure. A request for a variable name is a paragraph; a request for a
migration plan carries a conversation, files and errors.

```sh
keera router add bysize --mode size \
  --destinations keera-speed:4k,keera-frontier \
  --description "the local model for short work, the hosted one for long"
```

Each destination has **a ceiling**: the largest request it should get, in
estimated tokens. The destination without a ceiling takes anything larger. A
request goes to the destination with the smallest ceiling it fits under.

### What it has that an instruction has not

- **No cost**: no generation, no GPU, no added time to first token.
- **Same answer every time**: spend and the data boundary do not shift between
  similar requests, and `keera router check` shows exactly what production will
  do.
- **It cannot be argued with**: a prompt asking for the large model is just a
  few words longer.

### What it cannot do

- **Tell a hard request from an easy one of the same length.** "Prove this
  function terminates" is shorter than most renames.
- **See what is in the text.** To keep client data inside the cluster, use an
  instruction router, a filter or an allow-list. A size router is no data
  boundary.
- **Count anything but text.** It measures the same text the filters read, with
  the same pessimistic estimate. A request that is mostly a base64 image counts
  as small, although the image does take up context.

### The one thing only it gets right

It is the only mode that checks whether a request fits. A destination whose
`max_context` cannot hold the request is ranked **behind every destination that
can**, whatever the ceilings say. Otherwise a long conversation could reach the
small model and be truncated or get a 413.

The three tiers, in the order they are tried:

1. it can hold the request, and the request is under its ceiling
2. it can hold the request, and the request is over its ceiling - more model
   than wanted, which costs more but works
3. it cannot hold the request at all

Within a tier: smallest ceiling first, the destination without a ceiling last,
written order breaks ties. `keera router check` prints the whole table.

### Ceilings

Written as `alias:tokens` in `--destinations`. A bare number is tokens and `k`
means a thousand, so `keera-speed:4k` and `keera-speed:4000` are the same.

**The number is an estimate**: three bytes of text per token. It overestimates
on purpose, so a borderline request goes to the larger model. The same estimate
is behind the 413 on an oversized filter input.

**Exactly one destination must have no ceiling.** With none, a request larger
than every ceiling has nowhere to go. With more than one, only the first of them
is ever first choice. Both are refused when the router is saved.

A ceiling no higher than the one before it makes its destination first choice
for nothing. This is allowed, but `keera router check` warns about it.

### What it shares with the other cheap modes

Destinations are tried until one answers, a 4xx is not failed over, nothing
fails over once an answer has started, and the next destination up is the
fallback. See [what counts as a failure](#what-counts-as-a-failure).

So if the preferred destination is down, a larger one serves the request. It
costs more but is not refused. This shows as `fallback` on the router's screen,
and means the same as on a [measured router](#what-it-looks-like-afterwards-1):
the ranking was wrong.

## The fallback router

Use it when two or more models can serve the same request and one may be
unavailable: a local cluster with a hosted endpoint behind it, two providers of
the same model, a GPU pool that is drained for maintenance.

```sh
keera router add ha --mode fallback \
  --destinations keera-local,hosted-frontier \
  --description "the cluster, with the hosted model behind it"
```

**Destinations are tried in the order they are written**, until one answers or
the list runs out. There is no deciding model, no instruction and no fallback
destination - the list _is_ the fallback.

Otherwise it is a router like any other: clients name it the same way, allowing
it allows its destinations, and it writes the same usage row.

### What counts as a failure

| Failed over                                            | Not failed over                     |
| ------------------------------------------------------ | ----------------------------------- |
| the destination could not be reached                   | any 4xx                             |
| it did not begin answering within the upstream timeout | anything after the answer has begun |
| it answered 5xx                                        |                                     |

**A 4xx is about the request, not the destination.** A request that is too long,
malformed or over an upstream quota would fail the same way at the next
destination.

**Nothing fails over once an answer has started.** One model cannot finish
another model's half-sent answer.

So the timeout that matters is `KEERA_UPSTREAM_HEADER_TIMEOUT`, on the response
headers. Lower it if two minutes is too long to wait before the next destination
is tried.

A destination the catalogue cannot serve is skipped. Only when _none_ can be
served is the request refused, with `router_destination_unavailable`.

### What a request costs

Nothing, until something fails. A fallback router has no model of its own, so
`keera_router_cost_micros_total` stays at zero.

The cost is **time**: each failed destination adds wait before the next one is
tried. It is recorded in `router_ms`, the same column as a deciding router's
generation.

### What it looks like afterwards

| Outcome    | On a router that decides       | On a fallback router           |
| ---------- | ------------------------------ | ------------------------------ |
| `chose`    | the decision was made and kept | the first destination answered |
| `fallback` | it could not decide            | a later destination answered   |
| `error`    | it could not place the request | every destination failed       |

When every destination fails, the client gets the last destination's own
response.

Answers carry the same header, and `(fallback)` means the same thing:

```
X-Keera-Router: ha -> keera-local
X-Keera-Router: ha -> hosted-frontier (fallback)
```

The failed attempts are stored on the request's row, even when a later
destination answered.

### The state to watch for

**A chain quietly running on its second destination.** Every request succeeds,
but pays the second destination's price and waits for the first to fail. No
error shows up anywhere.

It shows in the split on the router's screen, and in:

```sh
keera router check ha
```

which asks each destination, in order, whether it is up. The result only holds
for that moment. Alert on the fallback rate.

## Balancing between equals

A fallback router always prefers its first destination. For equal destinations,
such as identical vLLM replicas, that leaves the others mostly idle. These two
modes put the same list in a new order on every request:

```sh
keera router add pool --mode least-busy \
  --destinations llama-a,llama-b,llama-c \
  --description "the three replicas, whichever has room"

keera router add frontier --mode latency \
  --destinations frontier-eu,frontier-us \
  --description "either region, whichever is answering"
```

Otherwise they work like a fallback router: destinations are tried until one
answers, a 4xx is not failed over, nothing fails over once an answer has
started, and there is no deciding model, instruction or fallback destination.

### Which of the two

- **`least-busy` counts requests in flight**, from dispatch until the last token
  reaches the client. Use it for identical replicas. It handles uneven requests
  well: one long generation keeps a model as busy as fifty short ones. It is
  also the only one of the two that works in the first seconds after a restart.
- **`latency` averages time to first token**, weighted so the last half minute
  counts about twice as much as the half minute before. Use it when the
  destinations differ - regions, providers, hardware - because queue depths
  cannot be compared across machines of different speed.

### What it measures, and what it does not

- **Everything is measured per gateway process.** Nothing is shared between
  replicas or kept across restarts. These modes work best with one gateway
  replica. With many, they still catch one destination getting slower or busier
  than the others.
- **Only answers count as latency.** Otherwise a destination refusing in two
  milliseconds would score best. A failure counts as a failure; a 4xx counts as
  nothing.
- **A destination that just failed is ranked last** for the next half minute.
  Otherwise both modes would keep picking a down destination: nothing answers,
  so nothing is measured, and nothing is in flight.
- **A destination with no recent measurement is ranked first.** After five
  minutes without traffic, its reading is dropped and it moves to the front.
  This costs one request and is the only way the router notices that a
  destination got faster.
- **Ties go to the written order**, so a new router behaves like a fallback
  router until it has measured something.

### What it looks like afterwards

| Outcome    | On a fallback router           | On a latency or least-busy router        |
| ---------- | ------------------------------ | ---------------------------------------- |
| `chose`    | the first destination answered | the destination it ranked first answered |
| `fallback` | a later destination answered   | a lower-ranked destination answered      |
| `error`    | every destination failed       | every destination failed                 |

Here `fallback` means the ranking was wrong, not that a destination is down. A
few percent is re-probing at work. A quarter means a destination is failing
while measuring well.

`keera router check pool` is the fallback router's check with one more column:
what the router measured for each destination and the position that gives it.
Two checks a minute apart may show a different order; that is normal.

## There is no shadow mode

A filter's shadow mode compares its result with the request as sent. A router
has nothing to compare with: the client named the router, so the only other
candidate is the fallback. Use the check and the router's screen instead. For a
[size router](#routing-by-size), the check shows its whole behaviour, so a
shadow mode would add nothing.

## Checking one

`keera router check <alias>`, or the **Check** button in the panel, shows what a
router would do.

A fallback router has no decision to sample, so its check
[asks each destination in turn whether it is there](#the-state-to-watch-for).

A [size router's](#routing-by-size) check does the same, and prints **the sizes
each destination is first choice for**. Unlike everything else a check shows,
this is not a sample: it is the router's whole behaviour. It also warns when a
ceiling is larger than its model's context, and when a destination is first
choice for nothing.

On a router that decides, the check sends three sample prompts and shows where
each went:

| The sample                                         | What it asks                                 |
| -------------------------------------------------- | -------------------------------------------- |
| rename a variable                                  | a trivial edit - the cheapest model that can |
| an online migration of a 400 GB table              | a request that wants a model that can reason |
| a named client with an account and contract number | a request carrying data that may not leave   |

For each, it shows how sure the model was: the share of the probability its
letter got against the others. A sample decided at 95% is a clear choice. Three
samples at 35% mean the router cannot tell its destinations apart, and small
changes to the instruction or model will change its answers. If the backend
returns no probabilities, nothing is shown.

There is no pass or fail. But the check does warn when **every sample went to
the same model**: that router pays a generation per request to do what naming
the model directly would do.

It also lists destinations that were not offered, and destinations with no
description.

A check calls the backend directly, with the data plane's credentials. It is not
rate-limited, budgeted or billed and writes no usage row - but it does put three
real requests on the GPUs.

## What it is doing

Each router has its own screen: `/routers/<alias>` in the panel,
`keera router report <alias>` in a terminal.

A router's failures are silent. Every request still gets a 200:

- **Too eager**: paying the large model's price to rename variables.
- **Too shy**: prompts a guardrail meant to keep in the cluster leave it.
- **Stopped deciding**: every request goes to the fallback, and still pays for a
  generation.
- **Running on the second destination**: a fallback router whose first
  destination is down.

The screen shows, over a time window:

| Figure                                        | What it tells you                                                                           |
| --------------------------------------------- | ------------------------------------------------------------------------------------------- |
| requests placed                               | whether anything uses it; the shares below mean nothing without it                          |
| decided / fell back / could not place         | whether it still decides - or, on a fallback router, whether its first choice still answers |
| **the split between its destinations**        | whether it routes at all, or always picks the same model                                    |
| what each destination cost                    | whether the savings are what justified the router                                           |
| each destination's median time to first token | whether cheaper answers made developers wait longer                                         |
| what deciding added, p50 and p95              | the wait it adds; on a fallback router, the time the failed attempts took                   |

The split is the main figure.

The same figures are on `/metrics`, labelled by router, organisation, outcome
and destination: `keera_router_decisions_total`,
`keera_router_cost_micros_total` and `keera_router_duration_seconds`. Alert on a
rising fallback rate.

**Nothing the router read is stored.** Each request's usage row records which
router placed it, whether it decided and how long deciding took. So a router's
report only reaches back as far as `KEERA_USAGE_RETENTION`.

## What a client can see

The answer's `model` field names **the model that answered**, not the router.

Answers also carry `X-Keera-Router`, so a developer can see which model the
router picked:

```
X-Keera-Router: auto -> keera-frontier
X-Keera-Router: auto -> keera-speed (fallback)
```

## Who owns what

The model catalogue is shared by all tenants, and only an operator changes it. A
router belongs to one organisation, and its administrators write it. Two
organisations can each have a router with the same name; they are different
routers.

Router aliases share one namespace with catalogue aliases, because clients use
both in the same field. **The catalogue wins.** Creating a router over an
existing alias is refused. If a model is later added under a router's name, the
model serves that name and the router's screen shows it placing nothing.

The alias is the router's identity: lowercase letters, digits and interior
hyphens. It cannot change. To rename, create a new router and move the clients.

Members cannot create or change routers, but they can read them, instruction
included. The Routers screen, each router's screen, `GET /v1/routers` and
`GET /v1/routers/{alias}/report` are open to anyone signed in to the
organisation.

## Where a router is not

- **Only on the chat endpoints**: `/api/v1/chat/completions`, `/api/v1/messages`
  and `/api/v1/responses`. `/v1/completions` and `/v1/embeddings` do not know
  router names.
- **Not on a request with no text**, for a router that decides. A chat that is
  all images has nothing to read, so the fallback places it. A fallback router
  is unaffected, and a size router counts such a request as small.
- **Not a retry.** A fallback router moves an unanswered request to the next
  destination. It never sends the same request to the same model twice.
- **Not a guardrail.** A router picks between models an administrator listed. To
  stop data leaving the cluster, use a filter or an allow-list.

## What is recorded

- `router.put`, `router.delete` and `router.check` in the audit log, with who
  did it and in which organisation. The `router.put` entry holds the
  instruction, destinations, ceilings and fallback.
- One row per request in the usage log: the router, whether it decided, and what
  deciding added to the wait. The destination is that row's `alias`.
- What deciding spent, added to the request's `cost_micros` and charged to the
  same budgets - also when the request fell back or was refused. Only
  `instruction` spends anything here.
- On a fallback router, every destination that was tried and did not answer, on
  the request's row - also when a later one answered.
- `keera_router_decisions_total`, `keera_router_cost_micros_total` and
  `keera_router_duration_seconds` on `/metrics`.
- Answers carry `X-Keera-Router`.
