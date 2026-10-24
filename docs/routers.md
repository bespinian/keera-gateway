# Routers

A router is a tree of steps that a request goes through on its way to a model:
[filters](filters.md), route steps, rate limits, budgets and models, in the
order you draw them. A client calls it by putting its alias in the `model`
field, as it would a model's. Requests that don't name it are not affected.
[What clients call, and what always applies](gateway.md#what-clients-call-and-what-always-applies)
shows how routers and guardrails fit together.

To create and manage one in the panel or on the command line, see
[Routers](https://docs.keera.ch/manage/routers/). This page explains how routers
work and why.

A **route step** picks one model from a list for each request. The simplest
router is one route step.

| Step            | What it does                                                  | Where the request goes next                  |
| --------------- | ------------------------------------------------------------- | -------------------------------------------- |
| `filter`        | runs one of the organisation's filters over the request       | `next`                                       |
| `route`         | chooses one model from its destinations                       | the branch for that model, or straight to it |
| `rate_limit`    | counts requests and tokens per minute, day or month           | `next` while within it, else `exceeded`      |
| `budget`        | counts what requests through this step cost, per day or month | `next` while within it, else `exceeded`      |
| `system_prompt` | adds a system prompt to the requests that pass it             | `next`                                       |
| `model`         | sends the request to one model                                | nowhere: it ends the branch                  |

## The tree

Every step except a model hands the request on. A filter has one `next` step. A
route step has one branch per destination, and a limit has two: within, and
exceeded. Every branch ends at a model, or at a route step that sends straight
to the model it chose. Branches never join again, so you can always tell which
way a request came.

A **route branch** is keyed by a destination of that step. Once the step has
chosen, the request goes down that branch. A destination without a branch gets
the request straight away.

A route step that tries its destinations in turn (`fallback`, `latency`,
`least-busy` or `size`) follows the branch of the destination it would try
first. Once it follows a branch, it does not try the other destinations. A
destination without a branch keeps the usual behaviour: if it fails, the step
tries the next one, but never one with a branch. That one is reached only
through its branch, so its steps, such as a redaction, are never skipped.

A **limit** without an `exceeded` branch refuses what is over it, as the key's
own limits do: a budget with a 402 `budget_exceeded`, a rate limit with a 429,
`rate_limit_exceeded` or `token_rate_limit_exceeded` per minute and
`requests_quota_exceeded` or `tokens_quota_exceeded` per day or month (see
[Errors](https://docs.keera.ch/reference/errors/)). The limit headers on an answer describe only the
key's own levels. With an `exceeded` branch, the request takes that branch
instead and is still served. Its row in the request log notes which limit sent
it there.

A router may have up to 64 steps. It works on the chat endpoints only
(`/api/v1/chat/completions`, `/api/v1/messages` and `/api/v1/responses`), and a
key for [subscription models](subscriptions.md) cannot name one.

## Writing one

The steps are written as JSON. This one takes out credentials, spends at most
CHF 50 a month on the hosted model, and redacts names only on the way there:

```json
{
  "id": "keys",
  "kind": "filter",
  "ref": "redact-keys",
  "next": {
    "id": "hosted-budget",
    "kind": "budget",
    "budget_micros": 50000000,
    "period": "month",
    "exceeded": { "id": "local", "kind": "model", "ref": "keera-speed" },
    "next": {
      "id": "route",
      "kind": "route",
      "mode": "instruction",
      "model": "keera-decider",
      "prompt": "Send anything naming a client to keera-speed.",
      "destinations": ["keera-speed", "hosted-frontier"],
      "fallback": "keera-speed",
      "branches": {
        "hosted-frontier": {
          "id": "names",
          "kind": "filter",
          "ref": "redact-names",
          "next": { "id": "hosted", "kind": "model", "ref": "hosted-frontier" }
        }
      }
    }
  }
}
```

A request the route step sends to `keera-speed` goes there directly.

How to save, show and edit one is in
[Create a router](https://docs.keera.ch/manage/routers/#create-a-router).
`keera router set <alias> --step <id>` changes one route step without the file.
Every field is in the
[API reference](https://docs.keera.ch/reference/api/schemas/#routerstep). An
`id` has at most 32 characters, a route step 1 to 16 destinations, and
`budget_micros` is in millionths of a franc: 50000000 is CHF 50.

The command line gives a step without an `id` one made from its kind, such as
`filter-1`. Give each limit its own id, though: see below.

A model step or destination must be a chat model that is not a subscription
model. Every filter and model a step names must exist when the router is saved.

## The key's guardrails still apply

A router works inside the key's guardrails, never around them:

1. The key's rate limits and budgets are checked first.
2. The router runs, and chooses the model.
3. The key's own filters run, then its system prompt is added, followed by the
   prompts of the router's system prompt steps the request passed.

So a router cannot loosen what an administrator set, and the key's filters
always run, whatever the router did before them.
[What happens to one request](gateway.md#what-happens-to-one-request) has the
whole order.

## System prompts

A `system_prompt` step adds its text to the requests whose branch passes it.
Put one on a hosted branch, for example, to tell only the hosted model what it
must not do.

Where the step sits decides which requests get the prompt, not when it is
added. Like the key's own system prompt, it is added after every filter, so no
filter rewrites it and no route step reads it. It comes after the key's prompt,
because it is the more particular of the two, as a project's comes after its
organisation's. Several steps on one branch are added in the order the request
passed them, each separated by a blank line.

## Limits count per step

Each limit step counts by its `id`, for its own router. Two routers never share
a count, and two limits in one router don't either. If you edit the steps
around a limit and keep its id, it keeps its count. If you change the id, the
limit starts again from zero.

A rate limit's `period` is `minute` if left out, and a budget's is `month`. Both
count the way a guardrail's do; see
[The tenancy model](gateway.md#the-tenancy-model) and
[Rate limits across replicas](gateway.md#rate-limits-across-replicas).

A request is counted against a limit only when it passes it within the limit.
Its whole cost is then charged to that limit, the model's answer and its
filters and decisions included. A request that took the exceeded branch is not
counted against that limit.

## Rewriting before a route step

A route step can be placed after a filter that rewrites the request. Then it
decides on the rewritten text, not on what the client sent, and can send a
prompt to a hosted model that the original would have kept inside. This is why
the key's own filters run after the router; see
[Why this order](gateway.md#what-happens-to-one-request).

So saving such a router works, but it comes with a warning. The warning is
about filters that rewrite with a model, and not those in shadow, before a step
in `instruction` mode, which reads the text. A `size` step measures the
rewritten text too, without a warning. A
[pattern filter](filters.md#pattern-filters) before a route step gives none: it
takes out things with a fixed shape, such as keys and card numbers, which are
not what a route step decides by. Put it first, as
[Ordering and combination](filters.md#ordering-and-combination) recommends.

Either way, a route step reads the prompt before anything has decided where it
may go. So **serve the deciding model locally**, as a filter's. It must also be
on the organisation's allow-list; see
[Models that filters and routers run on](gateway.md#models-that-filters-and-routers-run-on).

## Allowing a router

A router is allowed the way a model is: by naming its alias in an allow-list.
Allowing it allows every model it may send a request to, which is every model
step and every route destination without a branch, whatever the key's own
allow-list says. Filters are never on an allow-list.

To send everything a project does through a router, narrow the project's
allow-list to the router. The key then has nothing else in its allow-list,
which is why allowing the router must allow where it sends.

A guardrail never routes on its own: if it did, a developer who asked for the
large model could get the small one without knowing.

So a new router joins the organisation's allow-list only when that list allows
every model it reaches. Otherwise it would open a model the list leaves out,
and it stays off the list until an administrator adds it; the panel and the
command say that no key can call it yet. Changing a router on the list so that
it reaches such a model is saved with a warning.

## Who owns what

A router belongs to one organisation, and names only that organisation's
filters and models. Who may write and read it is the same as for a filter; see
[Models that filters and routers run on](gateway.md#models-that-filters-and-routers-run-on).
Router aliases share one namespace with models, because clients name both in the
same field, so a model and a router cannot share an alias.

While a router names a filter or model, deleting that one is refused with a
409, such as `filter_in_use`, which names the router. A model counts when it is
a model step, a destination or a deciding model. Change the router first. If a
model step's model stops being usable some other way, for example it is
disabled, a request that reaches that step is refused with a 503
`router_unavailable`.

Deleting a router takes it off the organisation's allow-list. Deleting one that
a project's or key's allow-list still names, or that is the only entry on the
organisation's, is refused with a 409 `router_in_use`.

## Modes

A route step chooses in one of five modes:

| Mode                        | Picks the model by                             | Extra cost                 |
| --------------------------- | ---------------------------------------------- | -------------------------- |
| `instruction` (the default) | asking a small model to read the request       | one generation per request |
| `size`                      | the length of the request                      | only failed attempts       |
| `fallback`                  | the order the destinations are written         | only failed attempts       |
| `latency`                   | each one's recent time to first token          | only failed attempts       |
| `least-busy`                | the requests the gateway has in flight to each | only failed attempts       |

Only `instruction` reads what the request says. The other four put the
destinations in an order and try them until one answers.

A route step may have a single destination. That gives clients a name that
stays the same when the model behind it changes. An `instruction` step with one
destination, or with only one that can be served right now, sends every request
there without asking its model or reading the request. When that one cannot be
used at all - missing, disabled or without a backend - the request goes to the
fallback, or is refused with a 503 if the step has none.

Most of what follows is about `instruction` steps. The other modes have their
own sections - [routing by size](#routing-by-size),
[the fallback mode](#the-fallback-mode) and
[balancing between equals](#balancing-between-equals).

## The list decides, not the instruction

The deciding model's answer is looked up in `destinations`. If it names anything
else, the step has not decided and the request goes to
[the fallback](#when-it-cannot-decide). A prompt cannot talk the step into a
model outside its list.

So the destinations, in `keera router show` and in the panel, show where a
route step can send a prompt. The instruction only affects which one it picks.

Also:

- A destination that does not exist or is not a chat model is refused with a
  400 when the router is saved. One that cannot be served later - disabled, or
  no backend - is **left out of the choice**. `keera router check` reports what
  was and was not offered.
- The **fallback must be one of the destinations**.
- A subscription model cannot be the deciding model or a destination
  ([why](gateway.md#models-that-filters-and-routers-run-on)).

## Descriptions are what a route step decides on

The deciding model sees each destination's own model `description`
([how to write one](https://docs.keera.ch/manage/models/#write-a-good-description)).

The description lives on the model so it stays true when the model behind an
alias changes. Clients see it on `/api/v1/models` too.

Without descriptions it chooses between bare aliases, and `keera router check`
warns about it.

A model from a hosted provider starts with the provider's description
(`keera model catalogue` lists them). Rewrite it to say what this deployment
uses the model for.

## When it cannot decide

Choose when writing the step. Without `--fallback`, it refuses what it cannot
place.

| `--fallback <destination>`                | `--no-fallback`                    |
| ----------------------------------------- | ---------------------------------- |
| the request goes there without a decision | the request is refused with 503    |
| recorded as `fallback`                    | recorded as `error`                |
| the sender gets an answer                 | the sender gets `router_undecided` |

A request the fallback answered counts as served. Why the step could not decide
is kept as the request's note, shown on the Requests screen and by
`keera session show`.

Filters always fail closed. Route steps get a choice because there may be a
safe place to send the request.

A route step cannot decide when:

- its model [cannot run](gateway.md#models-that-filters-and-routers-run-on)
- none of its destinations can be served (with a fallback, this ends in the 503
  below)
- its model errored, timed out (after 30 seconds), or answered nothing
- its answer names none of its destinations
- its answer names two destinations at once
- the request has no text to read
- the request does not fit in the deciding model's context

A separate case is a chosen destination that cannot serve the request:
`router_destination_unavailable`, 503. This is almost always the fallback, since
only reachable destinations are offered to the model.

Either way, the reason is stored on the request's row in the request log.

## Cost and latency

**This section is about `instruction` only.** The other modes generate nothing;
a request pays only for the destinations that failed before one answered.

The decision happens before anything is forwarded, so it adds to time to first
token. What it spends is charged to the same budgets and added to the request's
`cost_micros`.

A filter costs a generation only on the requests its guardrails cover. **A
route step costs one on every request that reaches it.**

So the step reads at most **the newest 24 KiB** of the request's segments. It
reads from the end because the latest ask is what matters; a coding agent's
conversation is mostly files it has already read. A single segment larger than
the window is sent whole.

The deciding model gets a small, fixed output budget. `keera router check`
shows what its runs cost, which is what every request through the step pays on
top.

### How the answer is read

The destinations are offered as letters - `A) keera-small`, `B) keera-large` -
and the model is asked for the letter only. The gateway reads the probabilities
for that one token and takes the most likely letter.

This costs **one** output token instead of up to 64, and the answer is always a
destination.

It needs a backend that returns logprobs. A model that serves none is asked for
a name instead
([Logprobs](gateway.md#models-that-filters-and-routers-run-on)).

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
  the destination list and every description on every request that reaches the
  step.
- At most 16 destinations. With longer lists, a small model tends to pick
  whichever end it read last.

The deciding model can also be a
[decision model](gateway.md#decision-models), a classifier that picks in one
pass and writes nothing. It needs its instruction written differently.

## Routing by size

Request length often shows how much model a request needs, and it costs nothing
to measure. A request for a variable name is a paragraph; a request for a
migration plan carries a conversation, files and errors.

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
  instruction step, a filter or an allow-list. A size step is no data boundary.
- **Count anything but text.** It measures the same text the filters read, with
  the same pessimistic estimate. A request that is mostly a base64 image counts
  as small, although the image does take up context.

### The one thing only it gets right

It is the only mode that chooses by whether a request fits. A destination whose
`max_context_tokens` cannot hold the request is ranked **behind every
destination that can**, whatever the ceilings say. The other modes may send a
long conversation to a small model, which then refuses it itself. The gateway
refuses a request only when
[no destination can hold it](gateway.md#requests-too-long-for-the-model).

The three tiers, in the order they are tried:

1. it can hold the request, and the request is under its ceiling
2. it can hold the request, and the request is over its ceiling - a smaller
   model than wanted, used when every one meant for this size is down
3. it cannot hold the request at all

Within a tier: smallest ceiling first, the destination without a ceiling last,
written order breaks ties. `keera router check` prints the whole table.

### Ceilings

Written as `alias:tokens` in `--destinations`, or in `ceilings` in the JSON. A
bare number is tokens and `k` means a thousand, so `keera-speed:4k` and
`keera-speed:4000` are the same.

**The number is an estimate**: three bytes of text per token. It overestimates
on purpose, so a borderline request goes to the larger model. The same estimate
is behind the 413 on an oversized filter input.

**Exactly one destination must have no ceiling.** With none, a request larger
than every ceiling has nowhere to go. With more than one, only the first of them
is ever first choice. Both are refused when the router is saved.

A ceiling no higher than the one before it makes its destination first choice
for nothing. This is allowed, but `keera router check` warns about it.

### When a destination is down

It fails over like [the fallback mode](#what-counts-as-a-failure). So if the
preferred destination is down, a larger one serves the request. It costs more
but is not refused, and shows as `fallback`
([What is recorded](#what-is-recorded)).

## The fallback mode

Use it when two or more models can serve the same request and one may be
unavailable: a local cluster with a hosted endpoint behind it, two providers of
the same model, a GPU pool that is drained for maintenance.

**Destinations are tried in the order they are written**, until one answers or
the list runs out. There is no deciding model, no instruction and no fallback
destination - the list _is_ the fallback.

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

A destination the organisation cannot serve is skipped. Only when _none_ can be
served is the request refused, with `router_destination_unavailable`. When every
destination waits for the organisation's first payment, it is `org_limited`
instead, as for the models themselves.

A request a later destination answered counts as served. The destinations that
failed before it are kept as the request's note.

### What a request costs

Only time: each failed destination adds wait before the next one is tried. It
is recorded in `router_ms`, the same column as a deciding step's generation.

### The state to watch for

**A chain quietly running on its second destination.** Every request succeeds,
but pays the second destination's price and waits for the first to fail. No
error shows up anywhere.

It shows in the split on the router's screen, and in:

```sh
keera router check ha
```

which asks each destination, in order, whether it is up, and warns when the
first one is not answering or only one is. The result only holds for that
moment. Alert on the fallback rate.

## Balancing between equals

A fallback step always prefers its first destination. For equal destinations,
such as identical vLLM replicas, that leaves the others mostly idle. These two
modes put the same list in a new order on every request.

Otherwise they work like the fallback mode, and
[fail over the same way](#what-counts-as-a-failure). There is no deciding model,
instruction or fallback destination.

### Which of the two

- **`least-busy` counts requests in flight**, from dispatch until the last token
  reaches the client. Use it for identical replicas. It handles uneven requests
  well: one long generation keeps a model as busy as fifty short ones. It is
  also the only one of the two that works in the first seconds after a restart.
- **`latency` averages time to first token**, weighted so the last half minute
  counts about twice as much as the half minute before. Use it when the
  destinations differ - regions, providers, hardware - because queue depths
  cannot be compared across machines of different speed. The first token is
  the first one the model wrote - text, reasoning or a tool call - not an
  event that only opens the stream, which some providers send at once. It is
  timed from when the destination was sent the request, so filters and failed
  attempts before it do not count. An answer that is not streamed counts until
  its end.

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
- **In `latency` mode, a destination with no recent measurement is ranked
  first.** After five minutes without traffic, its reading is dropped and it
  moves to the front. This costs one request and is the only way the step
  notices that a destination got faster. A least-busy step only counts requests
  in flight.
- **Ties go to the written order**, so a new step behaves like a fallback step
  until it has measured something.

Their check is the fallback mode's check, with what the step measured for each
destination in its note, in the order that gives. The panel shows it in a
column of its own. Two checks a minute apart may show a different order; that is normal.

## There is no shadow mode

A route step has nothing to compare its choice with: the client did not name a
model, so the only other candidate is the fallback. Use the check and the
router's screen instead.

## Checking one

A check shows what each route step would do
([Check and watch it](https://docs.keera.ch/manage/routers/#check-and-watch-it)).
`--step <id>` checks one of them.

A step that tries its destinations has no decision to sample, so its check
[asks each destination in turn whether it is there](#the-state-to-watch-for).

A [size step's](#routing-by-size) check does the same, and prints **the sizes
each destination is first choice for**. Unlike everything else a check shows,
this is not a sample: it is the step's whole behaviour. It also warns when a
ceiling is larger than its model's context, when a destination is first choice
for nothing, and when every request goes to the same destination whatever its
size.

On a step that decides, the check sends three sample prompts and shows where
each went:

| The sample                                         | What it asks                                 |
| -------------------------------------------------- | -------------------------------------------- |
| rename a variable                                  | a trivial edit - the cheapest model that can |
| an online migration of a 400 GB table              | a request that wants a model that can reason |
| a named client with an account and contract number | a request carrying data that may not leave   |

For each, it shows how sure the model was: the share of the probability its
letter got against the others. A sample decided at 95% is a clear choice. Three
samples at 35% mean the step cannot tell its destinations apart, and small
changes to the instruction or model will change its answers. If the backend
returns no probabilities, nothing is shown.

There is no pass or fail. But the check does warn when **every sample went to
the same model**: that step pays a generation per request to do what naming the
model directly would do. It also warns when some samples could not be placed.

It also lists destinations that were not offered, and destinations with no
description.

A check
[calls the backend directly](gateway.md#models-that-filters-and-routers-run-on):
at least three requests for a step that decides, since a sample whose lettered
answer cannot be read is asked again for a name, and one per destination for
the other modes (two when a backend refuses tools).

## What a client can see

`GET /api/v1/models` lists routers next to the models. Their `destinations` are
every model they may send to.

The answer's `model` field names **the model that answered**, not the router.

Answers carry `X-Keera-Router` with the router's alias. Once a route step has
chosen, it also says where it sent the request, so a developer can see which
model answered:

```
X-Keera-Router: guarded
X-Keera-Router: auto -> keera-frontier
X-Keera-Router: auto -> keera-speed (fallback)
X-Keera-Router: ha -> hosted-frontier (every destination failed)
```

When every destination fails, the client gets the last destination's own
response. If that one could not be reached at all, or did not answer in time,
the client gets the gateway's 502 `upstream_unavailable` instead.

A filter step that refuses says the router refused, not a guardrail of the key.

## What is recorded

Each router has its own screen, and `keera router report <alias>`
([Check and watch it](https://docs.keera.ch/manage/routers/#check-and-watch-it)).

A route step's failures are silent. Every request still gets a 200:

- **Too eager**: paying the large model's price to rename variables.
- **Too shy**: prompts a guardrail meant to keep in the cluster leave it.
- **Stopped deciding**: every request goes to the fallback, and still pays for a
  generation.
- **Running on the second destination**: a fallback step whose first
  destination is down.

So the screen shows, over a time window:

| Figure                                 | What it tells you                                                    |
| -------------------------------------- | -------------------------------------------------------------------- |
| requests                               | whether anything uses it; the shares below mean nothing without it   |
| decided / fell back / could not place  | whether it still decides, or whether its first choice still answers  |
| **the split between its destinations** | whether it routes at all, or always picks the same model             |
| what deciding added, p50 and p95       | the wait it adds; in the trying modes, the time failed attempts took |

The split is the main figure. Each request a route step placed has one of three
outcomes:

| Outcome    | Instruction                    | Fallback                       | Size, latency or least-busy           |
| ---------- | ------------------------------ | ------------------------------ | ------------------------------------- |
| `chose`    | the decision was made and kept | the first destination answered | the destination ranked first answered |
| `fallback` | it could not decide            | a later destination answered   | a lower-ranked destination answered   |
| `error`    | it could not place the request | every destination failed       | every destination failed              |

A client that hangs up before any destination answers is also `error`: no
destination answered, so `chose` would claim a success. The destination is not
marked as failing for it, so a latency or least-busy step still ranks it as
before.

Outside the instruction mode, `fallback` means the ranking was wrong. In
latency or least-busy mode a few percent is re-probing at work; a quarter means
a destination is failing while measuring well.

`/metrics` has these figures too, labelled by router, step, organisation,
outcome and destination: `keera_router_decisions_total`,
`keera_router_cost_micros_total` and `keera_router_duration_seconds`. It counts
every decision of an instruction step with two or more destinations. A route
step that only tries its destinations, which includes an instruction step with
one, is counted only when it is the request's last route step, as in the
report. Alert on a rising fallback rate.

**Nothing a route step read is stored.** Also recorded:

- `router.put`, `router.delete` and `router.check` in the audit log, with who
  did it and in which organisation. The `router.put` entry holds the whole
  tree, instructions included.
- One row per request in the request log: the router, and, when a route step ran,
  whether the last one decided and what deciding added to the wait. The model
  that answered is that row's `alias`. So a router's report only reaches back as
  far as `KEERA_USAGE_RETENTION`.
- What deciding spent, added to the request's `cost_micros` and charged to the
  same budgets - also when the request fell back or was refused. Only
  `instruction` spends anything here. When a request passes several route steps,
  each one's spend is charged, but the row records the last one's outcome.
- Every destination that was tried and did not answer, on the request's row -
  also when a later one answered.
- The filter steps record their runs as they would anywhere else, so the
  [filter](filters.md#what-is-recorded) screens include their router traffic.

## Upgrading from v0.10

In v0.10, a router was only what a route step is now. The upgrade to v0.11
deletes those routers, so a client naming one is told the model does not exist
until it is written again: a router of the same alias with one route step,
added with the same flags as before.

The allow-lists that named the old router still name its alias, so the new one
is allowed in the same places, and its report includes the old one's traffic.
