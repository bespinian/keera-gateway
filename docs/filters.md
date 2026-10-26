# Filters

A filter reads the text of each request a guardrail applies it to, before the
gateway forwards it. It is the only guardrail that looks at what is _inside_ a
request.

Its main use is a model outside the cluster. A project that needs one should still
not send it credentials, customer names or account numbers.

How to create, check and use one is in
[Filters](https://docs.keera.ch/manage/filters/). This page explains how filters
work and why.

## The three modes

`rewrite` and `gate` read the request with a small model and an instruction.
`pattern` applies a list of rules and runs no model. `--mode` picks one; left
out, a filter is a `rewrite`.

|                        | `rewrite`                           | `gate`                           | `pattern`                         |
| ---------------------- | ----------------------------------- | -------------------------------- | --------------------------------- |
| What decides           | a model reading prose               | a model reading prose            | a list of expressions             |
| What it answers        | the request's text, rewritten       | one word                         | nothing - the rules are applied   |
| Can it redact?         | yes                                 | no                               | yes, what its rules match         |
| Can it stop a request? | yes, if its instruction says so     | yes                              | yes, with a `REFUSE` rule         |
| Cost per request       | a second generation, request-sized  | a few tokens                     | **nothing**                       |
| Context it needs       | the request twice: read and written | the request once                 | none                              |
| Same answer twice?     | usually, at temperature zero        | usually, at temperature zero     | always                            |
| What its check shows   | the sample before and after         | a verdict on two sample requests | the sample, and which rules fired |

How to choose is in
[Three kinds of filter](https://docs.keera.ch/manage/filters/#three-kinds-of-filter).
When a request should be stopped, not edited, a gate beats a rewrite filter: it
costs a verdict instead of a regenerated conversation, and it is the more
reliable of the two at its job.

### Which one

Ask whether finding the thing needs reading. A credential has a shape. A
customer's name in an ordinary sentence does not: no expression finds
`the Meier account is overdrawn`, and no model beats `\bsk-[a-z0-9]{20,}\b` at
what that matches.

The modes combine; see [Ordering and combination](#ordering-and-combination).

You can change a filter's mode later, and its fields change with it: `rewrite`
and `gate` take a model and an instruction, `pattern` takes rules, and neither
accepts the other's. An instruction also means something different to `rewrite`
and to `gate` ([For a gate](#for-a-gate)). Change both together, and run
`keera filter check`.

Whether a filter _enforces_ is separate from its mode - see
[Shadow](#shadow-rolling-one-out).

## What a filter can reach inside a request

Every mode sees the same thing: the text of one request, as an ordered array of
segments.

- A `rewrite` filter returns the same number of segments, rewritten, or
  [refuses](#refusing-a-request).
- A `gate` returns a verdict and no text.
- A `pattern` filter sends nothing anywhere. Its rules run on each segment in
  turn.

| Surface                    | Segments                                                                                            |
| -------------------------- | --------------------------------------------------------------------------------------------------- |
| `/api/v1/chat/completions` | each message's `content` when it is a string, and the `text` of each `{"type":"text"}` content part |
| `/api/v1/messages`         | the `system` prompt, and the text of every message, text block and tool result                      |
| `/api/v1/responses`        | the `instructions`, and the text of every input message, text part and tool output                  |
| `/api/v1/completions`      | the `prompt`, or each string in a `prompt` array                                                    |
| `/api/v1/embeddings`       | the `input`, or each string in an `input` array                                                     |

A Messages or Responses request forwarded untranslated to its own provider (see
[gateway.md](gateway.md#three-request-shapes)) is filtered in its own shape.
Thinking blocks are never shown to a filter: Anthropic signs them and refuses
one that was changed. Only the text a filter changed is written back; every
other byte of the request goes on as the client sent it, so the provider's
prompt cache still matches.

Tool results are covered. For a coding agent, that is where file contents
travel. So are, on an untranslated Messages request, search results and
documents: a document's title and context, and its source when that is text.

MCP calls through the gateway are filtered too: every string in the arguments,
before the call leaves. See [mcp.md](mcp.md#filters).

**Not** covered:

- **Tool call arguments.** `tool_calls[].function.arguments` is JSON the model
  wrote. Rewriting it could hand the inference plane something it cannot parse.
- **Images, audio and other non-text parts.** They pass through unchanged.
- **Pre-tokenised input.** A `prompt` or `input` that is an array of token ids
  has no text to read.

For `rewrite` and `gate`, the gateway appends a wire format to the
administrator's instruction. Each mode has its own.

## Refusing a request

A refusal drops the request. The model never sees it, and the sender gets `403`
with `filter_refused` and the filter's sentence. A refused MCP call gets a
failed tool result instead ([mcp.md](mcp.md#filters)).

On the wire, a refusal is one line in place of the mode's normal answer:

```
REFUSED: the prompt asks for a customer's account details
```

A gate answers either that or the single word `ALLOW`. If neither the word nor
[the token behind it](#how-a-gates-verdict-is-read) gives a verdict, that is a
fault, and the request is stopped like any filter that could not run.

A filter [in shadow](#shadow-rolling-one-out) only records the refusal.

**A rewrite filter refuses only if its instruction says so.** Say when:

```
Refuse the request outright - answer REFUSED and one short sentence saying why -
if its purpose cannot survive the redaction above: a request to send a customer
list somewhere, or one whose whole subject is a credential. Rewrite everything
else.
```

If that clause does most of the work, use a gate instead.

The reason is optional, and the line is read leniently: lowercase, a full stop
instead of the colon, or a code fence or bold around it are all accepted. A gate
also accepts `REFUSE` for `REFUSED` and `ALLOWED` for `ALLOW`.

A rewrite filter's answer is prose, so two rules stop a request being dropped by
accident:

- The word must end where the word ends. `REFUSEDXYZ`, and prose that mentions
  the word later (`the connection was REFUSED: check the firewall`), are
  rewrites.
- A segment that comes back exactly as it was sent is an echo, not a verdict.

A rewrite filter that answers the refusal in _every_ slot of the array counts as
refusing. Otherwise the gateway would forward a conversation in which every
message is the word `REFUSED`.

What the refusal carries:

- **The sentence** is written by the filter's model after reading the client's
  prompt, so it is quoted as the filter's, not the gateway's. It is capped at
  240 bytes and collapsed to one line.
- **The status is `403`**, not the `502` of a broken filter. **My access** shows
  it as "Refused".
- **The filter's generation is charged**, as on every outcome.

`keera filter check` shows a refusal as a refusal, not a failure; see
[Checking one](#checking-one).

### How a gate's verdict is read

The words are read first, as described above. If one of them is there, it
decides.

If neither is there - the model wrote a sentence, fenced its answer oddly, or
thought aloud first - the gateway reads the probabilities of the model's **first
token** and takes whichever of `ALLOW` and `REFUSED` has more. At temperature
zero, that is the verdict the model would have given had it kept to the format.
Without this, a formatting slip by the small model would be a 502 on ordinary
work. If the verdict is a refusal and no sentence was written in the format, the
model's prose becomes the sentence the sender sees.

This does not loosen [failing closed](#failing-closed). A request goes only if
one of the two readings says so.

It needs a backend that returns logprobs. A gate whose model serves none reads
the words only ([Logprobs](gateway.md#models-that-filters-and-routers-run-on)).
Rewrite filters do not use it.

It also tells the gateway **how sure** the gate was, which
[`keera filter check`](#checking-one) shows.

## Pattern filters

A pattern filter is a list of rules. It has no model and no instruction, so it
adds no wait and no spend. To create one, see
[Create a pattern filter](https://docs.keera.ch/manage/filters/#create-a-pattern-filter).

Blank lines and `#` comments are ignored. A line is split at the **last**
`=>`, because the left side is an expression and the right side is literal.

### What a rule is

**The expression is [RE2](https://github.com/google/re2/wiki/Syntax)**, Go's
regexp syntax: no backreferences, no lookaround, and linear time in the input.

**The replacement is literal.** `$1` is those two characters, not the first
capture group, so the same text always gets the same result. It also means a
rule cannot keep part of its match: one that redacts a card number cannot keep
the last four digits. Write two rules, or use a `rewrite` filter.

**A rule either replaces or refuses, never both.**

**A refusing rule's sentence is the administrator's own**, so it is the same
every time. A reason longer than 240 bytes is refused.

**Rules run in the order written, and each sees what the one before it left.**
So a specific rule placed above a broad one wins:

```
\bCH93 0076 2011 6238 5295 7\b            => [HOUSE-ACCOUNT]
\b[A-Z]{2}\d{2}(?:[ ]?[A-Z0-9]{4}){2,7}\b => [IBAN]
```

A refusing rule ends the sweep where it matches.

### What is refused when you write one

Every expression is compiled when the filter is saved, not when it is first
used. A filter fails closed, so a broken expression would take a department
offline.

| What                                                   | Why                                                                    |
| ------------------------------------------------------ | ---------------------------------------------------------------------- |
| an expression that will not compile                    | it would refuse every request the guardrail covers                     |
| an expression that matches the empty string, like `x*` | it would insert its replacement between every character                |
| a rule that both replaces and refuses                  | a rule does one or the other                                           |
| a reason on a rule that does not refuse                | a replacing rule gives the sender no sentence                          |
| an empty expression                                    | a rule needs something to match                                        |
| a reason longer than 240 bytes                         | the sender reads it in an error message                                |
| no rules at all                                        | it would look like it works while doing nothing                        |
| a model or an instruction                              | nothing here reads prose, and a model on it would look like it decides |

Limits: **64 rules**, each expression at most **512 bytes**. They keep the list
readable; RE2 has no catastrophic backtracking.

### What it cannot do

**A broad rule matches broadly.** `\b(?:\d[ -]?){13,19}\b` matches a card
number and also a timestamp in nanoseconds. `keera filter check` names the rule
that touched the sample source code, which is the one to narrow.

Its runs are logged like any other, at zero cost, and a check of it touches no
backend. It fails closed only when its rules will not compile.

## Failing closed

A filter that cannot run refuses the request. There is no setting to change
this.

| Situation                                                                 | Status |
| ------------------------------------------------------------------------- | ------ |
| the guardrail names a filter that no longer exists                        | 503    |
| its model [cannot run](gateway.md#models-that-filters-and-routers-run-on) | 503    |
| its model could not be reached, timed out, errored, or answered unusably  | 502    |
| a gate answered neither `ALLOW` nor `REFUSED`, in word or in token        | 502    |
| the conversation will not fit through its model's context                 | 413    |
| a pattern filter's rules will not compile                                 | 502    |

A filter run is cut off after two minutes, so a stuck backend cannot hold a
request open. That counts as timed out.

A request with no text at all, such as a chat of only images, runs no filter,
so a filter that is missing or broken does not refuse it either.

A filter [in shadow](#shadow-rolling-one-out) that cannot run refuses nothing.

Each error names the filter, and each is recorded as a usage event, so it shows
on the **My access** screen.

The gateway also enforces:

- Deleting a filter that a router or a guardrail still uses is refused. The
  refusal names the routers, or the guardrails once no router uses it.
- A guardrail that names a filter the organisation does not have is refused
  when it is written.
- A filter's model must be on the organisation's allow-list and cannot be a
  subscription model. See
  [Models that filters and routers run on](gateway.md#models-that-filters-and-routers-run-on).

## Shadow: rolling one out

A new filter enforces, from the panel, from `keera filter add` and from the
control API. To start it in shadow, choose Shadow in the panel, pass `--shadow`
to `keera filter add`, or send `"shadow": true`. In shadow, the filter runs on
every request its guardrails and routers send through it, but enforces nothing.
The rewrite is discarded, the refusal does not happen, the request is forwarded
as the client sent it, and what the filter _would_ have done is recorded.

Both ways an instruction goes wrong are quiet:

- **Too eager:** it rewrites source code, reformats a stack trace or summarises
  the question. The developer sees a model that seems to have read something
  else, and blames the model.
- **Too strict:** it refuses their work, and they are only told that a guardrail
  refused them.

`keera filter check` catches the obvious cases on three sample segments. It
cannot tell you the _rate_ on real traffic: one request in a thousand, or one in
three. Shadow measures that before anyone depends on the filter. The commands
are in
[Try it in shadow first](https://docs.keera.ch/manage/filters/#try-it-in-shadow-first).

Leaving shadow only changes whether the answer is acted on. The mode,
instruction and model stay the same, so what you measured is what goes live.

What shadow does not change:

- **The cost.** The generation still runs, is waited for before the request is
  forwarded, and is charged to the same budgets.
- **Its place in the chain.** It runs where it would when enforcing, so a
  shadow gate after a redaction filter judges the redacted text. It does not
  pass on its own rewrite, and its refusal ends nothing.

What it does change:

- A shadow filter is **not** in `X-Keera-Filters`, which names only what acted
  on the request.
- A shadow filter that cannot run does not refuse the request: the run is
  recorded as `error` and the request goes on. One that errors on everything
  measures nothing, and its screen says so.

## Ordering and combination

Filters accumulate down the hierarchy like system prompts. A level may add
filters but never drop one an outer level applied.

They run outermost first - organisation, then project, then key - and each sees
what the one before it wrote. A filter named at two levels runs once.

A [router's](routers.md) filter steps run before all of them. A filter that both
a router step and a guardrail name runs once, at the router's step.

Within one level they run in the order that level wrote them: the order of
`--filters a,b`, or of the filter blocks in the guardrails dialog, whose
arrows move a filter earlier or later. A filter added there runs last.

A refusal ends the chain. The filters after it do not run and are not charged.

Modes mix freely. A gate after a rewrite filter judges the text the rewrite
left, which is what would actually be sent. That is also why the organisation's
redaction should be outermost.

**Put the pattern filter first.** It is free and removes what has a shape, so
the model filters after it never see those credentials or account numbers. It
does not save tokens, since the segments stay the same length.

A guardrail's filters run after the router, if the client named one, and
before the standing system prompt is added.
[What happens to one request](gateway.md#what-happens-to-one-request) has the
whole order, and why.

A guardrail's filters run on every request of its scope. To run a filter only
on some requests, such as those a route step sends to a hosted model, put it in
a [router](routers.md) as a step. See
[What clients call, and what always applies](gateway.md#what-clients-call-and-what-always-applies).

## Choosing and sizing the model

Point a `rewrite` or `gate` filter at a chat model, ideally a fast local one.
Its generation is added to every request the guardrail covers.

A `gate` can also run on a [decision model](gateway.md#decision-models), a
classifier that answers the verdict in one pass and writes nothing. Its
instruction is then a yes-or-no question, where yes refuses.

The context it needs depends on the mode:

- A `rewrite` filter reads the whole request and writes it back, so **its
  context must be at least as large as the models it guards, with room for
  both**.
- A `gate` only reads, and answers a few tokens, so **its context only has to
  hold the request once**. A gate can guard conversations that are too long for
  a rewrite filter on the same model.

The gateway estimates the request's size against the filter model's
`max_context_tokens` and refuses with 413 rather than send something that would be
truncated. The message says which of the two rules applied. Frequent 413s mean
the filter's model is too small for the traffic, or clients send very large
contexts.

The instruction is capped at 16 KiB.

## Cost and latency

A model filter means a second generation on every request it guards:

- **Latency.** The filter's whole generation finishes before the request is
  forwarded, so it adds to time-to-first-token. For a `rewrite` filter on a long
  conversation it is the largest cost.
- **Spend.** The filter model's cost is charged to the same budgets as the
  request and added to the request's `cost_micros`. Token counts stay the
  answering model's own.
- **Load.** The filter's model shares the same GPUs.

A `rewrite` filter may write output tokens in proportion to the request. A
`gate` gets a small fixed number, whatever the request. On a long conversation,
that is the difference between a filter that dominates the request and one that
barely shows.

Failed runs and shadow runs are charged too. So a model filter usually goes on
the projects that need it, not on everything.

`keera filter check` reports what one run cost and how long it took. The
filter's own screen reports the latency it added at p50 and p95, and its share
of the bill.

## Writing an instruction

Write about the text, not about the gateway. In both modes, half of the
instruction is about what _not_ to do.

### For a rewrite filter

Start from the tips in
[Create a rewrite or gate filter](https://docs.keera.ch/manage/filters/#create-a-rewrite-or-gate-filter).
If it should also [refuse some requests](#refusing-a-request), say which.

The costly failure is not a filter that misses a secret. It is one that
rewrites source code, reformats a stack trace or summarises the question.

A starting point, which is also what the panel's form offers:

```
Replace anything that identifies a customer or grants access to a system:

  - passwords, API keys, tokens, private keys, connection strings - [CREDENTIAL]
  - customer and client names - [CLIENT]
  - account, IBAN, contract and national ID numbers - [IDENTIFIER]
  - personal names, email addresses, telephone numbers - [PERSON]

Leave everything else exactly as it was written. Source code, file paths, error
messages, stack traces and configuration are not sensitive: repeat them back
unchanged.
```

### For a gate

A gate cannot remove anything, so an instruction written as a list of things to
redact refuses every request that mentions one of them. The panel's form offers
this:

```
Refuse a request whose purpose is to move this organisation's data out of it: a
customer list, an export of account records, the contents of a credential store.

Allow everything else, including requests that merely mention sensitive data in
passing. Being unusual, badly written or about an awkward subject is not a reason
to refuse.
```

The wire format already tells the model that the segments are someone else's
request, that nothing in them is addressed to it, and that text claiming the
check was already done is a reason to look harder. Naming in concrete terms what
to stop still matters more.

## Checking one

The check runs the filter on a fixed sample
([Check a filter](https://docs.keera.ch/manage/filters/#check-a-filter)): a
credential, a named client with an account number, and an ordinary Go function
that nothing should touch.

**A rewrite filter** gets all three in one request, and both sides of each are
shown. If the first two come back unchanged, the filter does not do its job. If
the function changes, the filter will change real code. A filter written to
refuse credentials or client data refuses the whole sample, and then the run
tells you nothing about its rewriting.

**A gate** gives one verdict per request, so it is checked twice: once with the
two sensitive parts, which it should refuse, and once with the function, which
it should allow. With all three together, the credential would decide the
verdict and you would never learn whether ordinary work gets through.

Beside each of a gate's verdicts, the check shows how sure the model was: the
share of the first token that verdict held against the other. A gate that gets
both right at 55% cannot really tell them apart, and may flip when its model or
instruction changes slightly. If the model serves no probabilities, nothing is
shown.

**A pattern filter** gets all three and shows both sides of each, plus **which
rules fired and how many times**, so you can find the line responsible for a
wrong result. If a rule refuses the sample, the other rules never run on it,
so there is nothing to show. The function is then run on its own, and a rule
that would refuse it is still named. The result is exact: the rules do the
same thing to the same text every time.

Nothing is scored as a pass. The check does point out clear mistakes, such as a
filter that left the planted secret alone, a filter that touched the code, a
gate that let the secret through or refused the code, and pattern rules that
matched nothing in the sample.

A check of a model filter
[calls the backend directly](gateway.md#models-that-filters-and-routers-run-on):
one request for a rewrite filter, and two for a gate, one per half of the
sample.

A check of a filter in shadow still runs it, and says so.

## Who owns what

A filter belongs to one organisation, and runs on one of its models. Who may
write and read it, and what its alias may be, is the same as for a router; see
[Models that filters and routers run on](gateway.md#models-that-filters-and-routers-run-on).

**My access** also shows the filters each of the member's keys passes through,
and the level that applied each one.

## What is recorded

Each filter has its own screen: `/filters/<alias>` in the panel,
`keera filter report <alias>` at a terminal. Over a time window, it shows:

| Figure                                             | What it answers                                                                                  |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| runs, and the share of the organisation's requests | Is it firing on everything, or on nothing? The other rates mean little without it                |
| the pass / rewrite / refuse / could-not-run split  | What does it do when it fires?                                                                   |
| segments shown, segments changed                   | Is the instruction too eager? Two changed out of forty is normal; thirty-eight is rewriting work |
| added latency, p50 and p95                         | How long does each request wait for it?                                                          |
| its spend, against the organisation's              | What share of the bill is it?                                                                    |
| the refusal rate **by project**                    | Who is affected?                                                                                 |

The last row matters most. Four percent across an organisation can be a whole
working day for the one project it lands on.

The same figures are on `/metrics`, labelled by filter, organisation, outcome
and shadow: `keera_filter_runs_total`, `keera_filter_cost_micros_total` and
`keera_filter_duration_seconds`. Alert on a filter that starts refusing.

**Nothing a filter read or wrote is stored**, apart from the one-line reason a
refusal gives, which is kept as the request's error. The filter log keeps one
row per filter per request: the mode, whether it was enforcing, the outcome, how
long it took, what it spent, and how many segments it was shown and changed.
Which rule fired is not kept either; it appears only in `keera filter check`,
against the sample. The log is kept as long as the request log
(`KEERA_USAGE_RETENTION`).

Also recorded:

- `filter.put`, `filter.delete` and `filter.check` in the audit log, with who
  did it and in which organisation. The `filter.put` entry includes the
  instruction or rules, the mode and whether it enforces.
- `guardrail.put` carries the filters a guardrail names.
- Every refusal a filter caused is a usage event with its status.
- Answers to model requests carry `X-Keera-Filters`, naming every filter that
  acted on the request in the order they ran, gates included. Filters in
  shadow are not named. Tool calls through MCP do not carry it.
