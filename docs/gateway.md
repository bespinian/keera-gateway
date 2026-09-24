# The gateway

Keera Gateway is one static Go binary. It holds the inference data plane, the
control plane and the control panel, and it talks to Postgres.

## One binary

`keera-gateway` is the server. `keera` is the administration command line. It
talks to the server's control API over HTTP, so the same commands work against
a gateway on a customer's cluster and against one on localhost.

Both are static (`CGO_ENABLED=0`) and ship in one `FROM scratch` image with no
shell.

On start, the gateway applies its own schema under a Postgres advisory lock, so
several replicas can start at once and no migration job is needed.
`keera-gateway migrate` runs the migrations as a separate step if you want one.

## One listener, four surfaces

Port 8080 carries everything, told apart by path:

| Path       | What it is                                                          |
| ---------- | ------------------------------------------------------------------- |
| `/api`     | The inference API and the MCP servers. Authenticated by an API key. |
| `/control` | The control API. Authenticated by the operator key or a session.    |
| `/sandbox` | Attaching to a sandbox. Only live when sandboxes are switched on.   |
| `/`        | The control panel, and `/metrics`, `/healthz` and `/readyz`.        |

`/sandbox` is not JSON. It carries a byte stream between an authenticated
caller and a port inside a sandbox, so attaching needs no second port and no
second certificate. See [sandboxes.md](sandboxes.md).

A deployment publishes one address and one certificate. The downside is that no
port separates the planes, only the operator key and the session cookie. To
separate them, put a proxy in front that publishes `/api` and nothing else.

`/healthz` checks only the process. `/readyz` also pings Postgres, so a replica
that has lost the database leaves the inference Service instead of being
restarted.

## The alias is the API contract

A client names an alias such as `keera-speed`, never a model id or a vLLM URL.
So you can swap the model behind an alias, change quantization, or point it at a
remote endpoint without any developer changing anything.

- The catalogue can be a file (`KEERA_MODELS_FILE`). It is applied on every
  start and is idempotent: the same file and an empty database give the same
  gateway. A model declared in the file belongs to the file, and the control API
  refuses to change or delete it.
- `backend_model` is the name the backend serves the model under - vLLM's
  `--served-model-name`, llama.cpp's `--alias`. If it is wrong, every request
  returns 404.
- `max_context` is shown to clients on `/v1/models`, sizes a filter's or a
  router's own model, and bounds every request. See
  [Requests too long for the model](#requests-too-long-for-the-model).
- An alias is lowercase letters, digits and interior hyphens, because it appears
  in the client configurations `keera connect` prints.

## Three request shapes

The data plane serves the OpenAI API at `/api/v1/chat/completions`,
`/api/v1/completions` and `/api/v1/embeddings`, the Anthropic Messages API at
`/api/v1/messages`, and the OpenAI Responses API at `/api/v1/responses`. Claude
Code uses the Messages API and Codex the Responses API. All of them go through
the same guardrails, accounting and audit.

The inference plane speaks chat completions, so the other two are translated on
the way in and back on the way out. Fields that chat completions has no place
for, such as thinking blocks, reasoning items and OpenAI's built-in tools, are
dropped.

The exception: a Messages request to a model declared with Anthropic, or a
Responses request to a model declared with OpenAI, is forwarded as it is.
Translating it would lose prompt caching and thinking at Anthropic, and
reasoning items at OpenAI. In a coding agent's session, most of the bill is the
cache. The guardrails still apply: filters read and rewrite the request in its
own shape, and the system prompt and the output ceiling are written into it. If
a router falls back from such a model to a local one, the local one gets the
translation.

- `POST /api/v1/messages/count_tokens` is answered by the gateway with an
  estimate that errs high. It is never forwarded, because that would send the
  prompt to Anthropic without passing its filters.
- A Responses request with `previous_response_id` or `conversation` continues a
  conversation stored at OpenAI. Only OpenAI's own models can read it, so any
  other model refuses it with a 400.

The hosted providers in `internal/catalog/providers.go` are **not** adapters.
They are a table of defaults - endpoint, credential variable, context window,
prices, description - so that pointing a model at Anthropic or OpenAI takes
three lines instead of six. The provider's name also decides the exception
above.

## The tenancy model

Three levels: organisation, team, key. A guardrail can attach at any of them.

The combining rule is **restrict-only**. A level can narrow what it inherits but
never widen it. Allow-lists intersect; numeric ceilings take the minimum.

Two exceptions:

- **Budgets are kept per level and checked separately**, not merged. An
  organisation cap of 10,000 and a team cap of 1,000 are two limits that must
  both hold.
- **System prompts and filters accumulate**, outermost first. A level may add to
  what it inherits but may not drop it.

Budgets reset on UTC boundaries in every deployment.

A level that sets nothing is not unlimited: it gets whatever it inherits. To see
the combined result, run `keera guardrail effective <scope> <id>` (or
`GET /control/v1/guardrails/{scope}/{id}/effective`). It returns every level's
guardrails, the combined answer, and the level that decided each value.

## What happens to one request

1. Authenticate the key, and resolve the guardrails attached to its org, team and
   itself.
2. Read the body, find the model, check the rate limits and the budgets.
3. **Router**, if the client named one: choose the destination.
4. Refuse the request if it cannot fit in the destination's context.
5. **Filters**, outermost level first: each may rewrite the request or refuse it,
   with a model and an instruction or with a list of expressions.
6. Prepend the standing system prompt and clamp the output ceiling.
7. Forward, stream the answer back, and record what it cost.

Why this order:

- The router (3) runs before the filters (5) so it reads what the client sent.
  Once a redaction filter has removed a client's name, the prompt looks safe for
  any model, and a router placed after it would send it outside the cluster.
- The context check (4) runs before the filters, so a request that cannot
  succeed costs nothing.
- The system prompt (6) is added after the filters, so an administrator's own
  wording is never handed to a small model that is allowed to edit it.
- Router and filters both run after the budget check, because both spend money.

## Requests too long for the model

A request that cannot fit in the `max_context` of any model it may go to is
refused with a 400 before anything is spent on it.

The gateway has no tokeniser, so it counts the fewest tokens a request could
be: its text at five bytes per token. Real text uses fewer bytes per token, so
the check never refuses a request that fits. A request just over the limit may
get through, and then the backend refuses it.

The refusal uses the wording clients act on. The code is
`context_length_exceeded`, which OpenAI clients read, and the message begins
`prompt is too long: N tokens > M maximum`, which makes Claude Code compact the
conversation. A backend's own refusal says neither, and the agent stops.

A model with no `max_context` is not checked. Embedding and completion inputs
are checked one by one, since each has to fit on its own.

## Streaming

A coding agent keeps one response open for minutes. So the gateway limits only
how long the inference plane may take to _start_ responding
(`KEERA_UPSTREAM_HEADER_TIMEOUT`, two minutes by default), never the response
itself. A router uses the same timeout to decide that a destination did not
answer.

Usage is recorded once per request, when it ends, from the token counts the
inference plane reported. If the client disconnects before those arrive, the
request is charged on an estimate and marked as estimated.

If you put an ingress in front, turn off response buffering and set a read
timeout longer than the longest completion. Most controllers buffer by default.

## Caching

The gateway keeps an in-memory view of the control plane (`internal/registry`),
refreshed every `KEERA_CACHE_TTL` (30 seconds). Spend is refreshed separately
and more often (`KEERA_SPEND_REFRESH`, 10 seconds).

So a revoked key can keep working, and a new guardrail may not apply yet, for up
to one TTL. In return, no request waits on a database round trip.

## Rate limits across replicas

The token buckets behind `rpm` and `tpm` live in each process. With N replicas
behind a load balancer, a limit of 600 requests per minute is 600 per replica,
and up to 600N in total. Spend is reconciled through Postgres, so a budget means
the same thing however many gateways are running.

Set `KEERA_REDIS_URL` to keep the buckets in Redis. One script then decides all
of a request's levels atomically, and every replica spends the same allowance -
see `internal/ratelimit/redis.go`.

- **Redis holds buckets and nothing else.** No guardrails, spend, sessions or
  prompts. Nothing in it has to survive a restart.
- **If Redis cannot be reached, traffic is not refused.** The gateway falls back
  to the in-memory buckets, and says so in the log and in
  `keera_ratelimit_fallback_total`. After a failed round trip it waits 5 seconds
  before trying Redis again, so an outage does not add the timeout to every
  request.
- **The headers come from the same decision**, not from a second round trip.

The control plane's sign-in throttle stays per replica.

Every answer carries what is left of the allowance - `X-Keera-Budget-Remaining`,
`X-Keera-Budget-Reset` and the `X-RateLimit-*` headers - so a client can see a
limit coming. When a limit is hit, the message says which limit, its value, and
when it lifts.

## What it deliberately does not do

- **It does not store prompts or completions.** What a filter read, what a
  router read, what a session was about - all of it is reduced to counts and
  hashes. See [filters.md](filters.md) and [sessions.md](sessions.md).
- **It is not a distributed trace.** The per-request breakdown in
  `internal/store/spans.go` has no sampling, no collector and no propagation,
  and every step it records happens inside one handler.
- **It does not translate request parameters between providers**, with one
  exception: OpenAI's reasoning models refuse `max_tokens` and a temperature, so
  the gateway sends `max_completion_tokens` and drops the temperature. Any other
  field a hosted provider rejects is an error the client sees.
- **It does not retry.** A router moves an unanswered request to its next
  destination, but nothing sends the same request to the same model twice.

## Failure modes worth knowing

**A passing chat test proves little.** The failures that break coding agents are
silent: the endpoint answers 200, the model writes prose, and the agent never
edits a file. This happens when the vLLM tool-call parser does not match the
model: the call comes back as text in `content` and `tool_calls` stays empty.
`keera model check` catches it. Run it on every change of model, quantization or
vLLM version.

**The inference backend has no authentication.** Neither vLLM's nor llama.cpp's
OpenAI server has any. Anything that reaches port 8000 directly gets a model
with no key, no budget, no rate limit and no audit entry. Keep that port
unpublished in every deployment; the Helm chart can enforce this with a
NetworkPolicy.

**Usage events are the billing record.** If the recorder starts dropping them,
it logs an error saying so.
