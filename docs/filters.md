# Filters

A filter reads the text of each request a guardrail applies it to, before the
gateway forwards it. It is the only guardrail that looks at what is _inside_ a
request.

Its main use is a model outside the cluster. A team that needs one should still
not send it credentials, customer names or account numbers.

## The three modes

`rewrite` and `gate` read the request with a small model and an instruction.
`pattern` applies a list of rules and runs no model.

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

- Use `rewrite` when the request should still go, with something removed that
  only a reader can find.
- Use `gate` when the request should be stopped, not edited: its whole purpose
  is the thing that may not leave. A gate costs a verdict instead of a
  regenerated conversation, and it is the more reliable of the two at its job.
- Use `pattern` when what must not leave has a fixed **shape**: an API key, a
  connection string, an IBAN, a card number, a national ID. Here an expression
  is faster, cheaper and more reliable than a model. See
  [Pattern filters](#pattern-filters).

### Which one

Ask whether finding the thing needs reading. A credential has a shape. A
customer's name in an ordinary sentence does not: no expression finds
`the Meier account is overdrawn`, and no model beats `\bsk-[a-z0-9]{20,}\b` at
what that matches.

The modes combine. A common setup is an organisation-wide `pattern` filter for
credentials and account numbers, a `rewrite` filter on the teams whose prose
names clients, and a `gate` on the one team with a hosted model. See
[Ordering and combination](#ordering-and-combination).

A model filter adds a second generation to every request, so deployments
usually put it on one team rather than on everything.

You can change a filter's mode later, and its fields change with it: `rewrite`
and `gate` take a model and an instruction, `pattern` takes rules, and neither
accepts the other's. An instruction also means something different to `rewrite`
and to `gate`. Change both together, and run `keera filter check`. A gate whose
instruction is a list of things to redact refuses every request that mentions
one of them.

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
| `/api/v1/responses`        | the `instructions`, and the text of every input message, text part and `function_call_output`       |
| `/api/v1/completions`      | the `prompt`, or each string in a `prompt` array                                                    |
| `/api/v1/embeddings`       | the `input`, or each string in an `input` array                                                     |

A Messages or Responses request forwarded untranslated to its own provider (see
[gateway.md](gateway.md#three-request-shapes)) is filtered in its own shape.
Thinking blocks are never shown to a filter: Anthropic signs them and refuses
one that was changed.

Tool results are covered. For a coding agent, that is where file contents
travel.

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
with `filter_refused` and the filter's sentence.

On the wire, a refusal is one line in place of the mode's normal answer:

```
REFUSED: the prompt asks for a customer's account details
```

A gate answers either that or the single word `ALLOW`. If neither the word nor
[the token behind it](#how-a-gates-verdict-is-read) gives a verdict, that is a
fault, and the request is stopped like any filter that could not run.

A filter [in shadow](#shadow-rolling-one-out) drops nothing. It records the
refusal it would have made and forwards the request.

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
  it as "Not permitted".
- **The filter's generation is charged**, as on every outcome.

`keera filter check` shows a refusal as a refusal, not a failure. Its first two
samples carry a credential and a named client with an account number, so a
filter written to refuse those will refuse them - but then the run tells you
nothing about its rewriting. Gates are checked differently; see
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

It needs a backend that returns logprobs. vLLM and llama.cpp do; most hosted
providers do not. There is nothing to configure: a gate whose model serves none
reads the words only. Rewrite filters do not use it.

It also tells the gateway **how sure** the gate was, which
[`keera filter check`](#checking-one) shows.

## Pattern filters

A pattern filter is a list of rules. It has no model and no instruction, so it
adds no wait and no spend.

```sh
keera filter add redact-keys --mode pattern --rules @redact.rules
```

Each line is an expression, `=>`, and what each match becomes. A rule whose
right side is `REFUSE` drops the request instead, with the sentence after the
colon:

```
(?i)\b[a-z_]*(?:secret|token|password|api[_-]?key)[a-z_]*\s*[=:]\s*\S+ => [CREDENTIAL]
(?i)\b(sk|pk|ghp|gho|xox[baprs])-[a-z0-9_-]{16,}\b                     => [CREDENTIAL]
(?i)\b[a-z]+://[^\s:@]+:[^\s:@]+@\S+                                   => [CONNECTION-STRING]
\b[A-Z]{2}\d{2}(?:[ ]?[A-Z0-9]{4}){2,7}\b                              => [IBAN]
\b\d{3}\.\d{4}\.\d{4}\.\d{2}\b                                          => [AHV]
(?i)\bexport all customers\b => REFUSE: that moves the customer list out
```

Blank lines and `#` comments are ignored. The line is split at the **last**
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
every time. It is capped at 240 bytes.

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
| no rules at all                                        | it would look like it works while doing nothing                        |
| a model or an instruction                              | nothing here reads prose, and a model on it would look like it decides |

Limits: **64 rules**, each expression at most **512 bytes**. They keep the list
readable; RE2 has no catastrophic backtracking.

### What it cannot do

**It cannot read.** A customer's name in an ordinary sentence has no shape. Use
the other two modes for that.

**It sees the same text as the other modes.** Tool call arguments, images and
pre-tokenised input are out of reach - see
[What a filter can reach](#what-a-filter-can-reach-inside-a-request). The whole
chain shares one extraction, so changing that is a change to the chain, not to
this mode.

**A broad rule matches broadly.** `\b(?:\d[ -]?){13,19}\b` matches a card
number and also a timestamp in nanoseconds. `keera filter check` names the rule
that touched the sample source code, which is the one to narrow.

### What it costs

Nothing. Its runs are on the filter log and on `/metrics` like any other, with a
cost of zero and negligible latency. What this document says about cost applies
to `rewrite` and `gate` only.

## Failing closed

A filter that cannot run refuses the request. There is no setting to change
this.

| Situation                                                           | Status |
| ------------------------------------------------------------------- | ------ |
| the guardrail names a filter that no longer exists                  | 503    |
| its model is missing, disabled, not a chat model, or has no backend | 503    |
| its model was reached but the answer was unusable, or it errored    | 502    |
| a gate answered neither `ALLOW` nor `REFUSED`, in word or in token  | 502    |
| the conversation will not fit through its model's context           | 413    |
| a pattern filter's rules will not compile                           | 502    |

A pattern filter has no model, no answer and no context, so the middle rows do
not apply to it.

A filter [in shadow](#shadow-rolling-one-out) that cannot run refuses nothing:
the run is recorded as `error` and the request goes on.

Each error names the filter, and each is recorded as a usage event, so it shows
on the **My access** screen.

The gateway also enforces:

- Deleting a filter that a guardrail still names is refused, and the refusal
  lists the guardrails.
- A guardrail that names a filter the organisation does not have is refused
  when it is written.

## Shadow: rolling one out

`--shadow`, or **Acts on its answer → Shadow** in the panel, runs the filter on
every request its guardrails cover but enforces nothing. The rewrite is
discarded, the refusal does not happen, the request is forwarded as the client
sent it, and what the filter _would_ have done is recorded.

Both ways an instruction goes wrong are quiet:

- **Too eager:** it rewrites source code, reformats a stack trace or summarises
  the question. The developer sees a model that seems to have read something
  else, and blames the model.
- **Too strict:** it refuses their work, and they are only told that a guardrail
  refused them.

`keera filter check` catches the obvious cases on three sample segments. It
cannot tell you the _rate_ on real traffic: one request in a thousand, or one in
three. Shadow measures that before anyone depends on the filter.

```sh
keera filter add redact-secrets --model keera-guard --prompt @redact.txt --shadow
keera guardrail set team <team-id> --filters redact-secrets
# a week later
keera filter report redact-secrets --since 168h
keera filter set redact-secrets --enforce
```

Leaving shadow only changes whether the answer is acted on. The mode,
instruction and model stay the same, so what you measured is what goes live.

What shadow does not change:

- **The cost.** The generation still runs, is waited for before the request is
  forwarded, and is charged to the same budgets.
- **Its place in the chain.** It runs where it would when enforcing, so a
  shadow gate after a redaction filter judges the redacted text. It does not
  pass on its own rewrite.

What it does change:

- A shadow filter is **not** in `X-Keera-Filters`, which names only what acted
  on the request.
- A shadow filter that cannot run does not refuse the request. A shadow filter
  that errors on everything measures nothing, and its screen says so.

## Ordering and combination

Filters accumulate down the hierarchy like system prompts. A level may add
filters but never drop one an outer level applied.

They run outermost first - organisation, then team, then key - and each sees
what the one before it wrote. A filter named at two levels runs once.

Within one level they run in the order that level wrote them: the order of
`--filters a,b`, or the **In this order** list in the guardrails dialog, whose
arrows move a filter up or down. A filter ticked in that dialog is added last.

A refusal ends the chain. The filters after it do not run and are not charged.
A refusal by a filter in shadow ends nothing.

Modes mix freely. A gate after a rewrite filter judges the text the rewrite
left, which is what would actually be sent. That is also why the organisation's
redaction should be outermost.

**Put the pattern filter first.** It is free and removes what has a shape, so
the model filters after it never see those credentials or account numbers. It
does not save tokens, since the segments stay the same length. For example:

```sh
keera filter add redact-keys --mode pattern --rules @redact.rules
keera filter add redact-names --model keera-guard --prompt @names.txt
keera guardrail set org <org-id> --filters redact-keys
keera guardrail set team <team-id> --filters redact-names
```

The organisation pays nothing for the first, and only the team that needs the
second pays for it.

Within one request, the order is:

1. authenticate, rate-limit, budget
2. the [router](routers.md), if the client named one
3. **filters**, in the order above
4. the standing system prompt is prepended
5. the output ceiling is clamped
6. forward

Filters run before step 4 so the administrator's system prompt is never handed
to a small model that is allowed to edit it.

## Choosing and sizing the model

This section does not apply to a [pattern filter](#pattern-filters), which has
no model.

Point a `rewrite` or `gate` filter at a chat model, ideally a fast local one.
Its generation is added to every request the guardrail covers.

The context it needs depends on the mode:

- A `rewrite` filter reads the whole request and writes it back, so **its
  context must be at least as large as the models it guards, with room for
  both**.
- A `gate` only reads, and answers a few tokens, so **its context only has to
  hold the request once**. A gate can guard conversations that are too long for
  a rewrite filter on the same model.

The gateway estimates the request's size against the filter model's
`max_context` and refuses with 413 rather than send something that would be
truncated. The message says which of the two rules applied. Frequent 413s mean
the filter's model is too small for the traffic, or clients send very large
contexts.

The instruction is capped at 16 KiB.

## Cost and latency

**This section is about `rewrite` and `gate`.** A
[pattern filter](#pattern-filters) generates nothing.

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

Failed runs and shadow runs are charged too. A pattern filter is never charged.

`keera filter check` reports what one run cost and how long it took. The
filter's own screen reports the latency it added at p50 and p95, and its share
of the bill.

## Writing an instruction

This is for `rewrite` and `gate`. A [pattern filter](#pattern-filters) has rules
instead.

Write about the text, not about the gateway. In both modes, half of the
instruction is about what _not_ to do.

### For a rewrite filter

Say what to remove, what to replace it with, and, explicitly, to leave
everything else exactly as it is. If it should also
[refuse some requests](#refusing-a-request), say which.

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

Say what makes a request one that may not go at all, then say explicitly that
everything else may. A gate cannot remove anything, so an instruction written
as a list of things to redact refuses every request that mentions one of them.

Say that being unusual, badly written or about an awkward subject is no reason
to refuse. The panel's form offers this:

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

`keera filter check <alias>`, or the **Check** button on the panel's Filters
screen, runs the filter on a fixed sample and shows the result.

The sample has three parts: a credential, a named client with an account
number, and an ordinary Go function that nothing should touch.

**A rewrite filter** gets all three in one request, and both sides of each are
shown. If the first two come back unchanged, the filter does not do its job. If
the function changes, the filter will change real code.

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
wrong result. The function is also run on its own, so a rule that matches it is
reported even when an earlier rule refused the sample. The result is exact: the
rules do the same thing to the same text every time.

Nothing is scored as a pass. The check does point out the two clear mistakes: a
filter that left the planted secret alone, and a filter that touched the code.

A check of a model filter goes straight to the backend, with the credentials the
data plane uses. It is not rate-limited, budgeted or billed and writes no usage
row, but it does put one real request on the GPUs. A check of a pattern filter
touches nothing and costs nothing.

A check of a filter in shadow still runs it, and says so.

## What it is doing

Each filter has its own screen: `/filters/<alias>` in the panel,
`keera filter report <alias>` at a terminal. Over a time window, it shows:

| Figure                                             | What it answers                                                                                  |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| runs, and the share of the organisation's requests | Is it firing on everything, or on nothing? The other rates mean little without it                |
| the pass / rewrite / refuse / could-not-run split  | What does it do when it fires?                                                                   |
| segments shown, segments changed                   | Is the instruction too eager? Two changed out of forty is normal; thirty-eight is rewriting work |
| added latency, p50 and p95                         | How long does each request wait for it?                                                          |
| its spend, against the organisation's              | What share of the bill is it? Zero for a pattern filter                                          |
| the refusal rate **by team**                       | Who is affected?                                                                                 |

The last row matters most. Four percent across an organisation can be a whole
working day for the one team it lands on.

The same figures are on `/metrics`, labelled by filter, organisation, outcome
and shadow: `keera_filter_runs_total`, `keera_filter_cost_micros_total` and
`keera_filter_duration_seconds`. Alert on a filter that starts refusing.

**Nothing a filter read or wrote is stored.** Each run keeps only the mode,
whether it was enforcing, the outcome, how long it took, what it spent, and how
many segments it was shown and changed. Which rule fired is not kept either; it
appears only in `keera filter check`, against the sample.

The filter log is kept as long as the usage log and purged by the same cutoff
(`KEERA_USAGE_RETENTION`).

## Who owns what

The model catalogue is shared by every tenant, and only an operator changes it.
A filter belongs to one organisation, and its administrators write it. Two
organisations may each have a filter with the same alias; they are different
filters.

A pattern filter uses nothing from the catalogue, so an administrator can write
one without an operator setting anything up first.

A filter's alias is its identity, like a model's: lowercase letters, digits and
interior hyphens. It cannot change. For a different alias, create a new filter
and move the guardrails.

Members cannot create or change filters, but they can read them, instruction
included. The Filters screen, each filter's screen, `GET /control/v1/filters` and
`GET /control/v1/filters/{alias}/report` are open to anyone signed in to the
organisation.

**My access** also shows the filters each of the member's keys passes through,
and the level that applied each one.

## What is recorded

- `filter.put`, `filter.delete` and `filter.check` in the audit log, with who
  did it and in which organisation. The `filter.put` entry includes the
  instruction or rules, the mode and whether it enforces.
- `guardrail.put` carries the filters a guardrail names.
- Every refusal a filter caused is a usage event with its status.
- One row per filter per request in the filter log: the mode, whether it was
  enforcing, the outcome, the latency, the cost, and the number of segments
  shown and changed. Nothing about the text.
- `keera_filter_runs_total`, `keera_filter_cost_micros_total` and
  `keera_filter_duration_seconds` on `/metrics`.
- Answers carry `X-Keera-Filters`, naming every filter that acted on the
  request in the order they ran, gates included. Filters in shadow are not
  named.
