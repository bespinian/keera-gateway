# The gateway

Keera Gateway is one static Go binary. It holds the inference data plane, the
control plane and the control panel, and it talks to Postgres.

## One binary

`keera-gateway` is the server. `keera` is the administration command line. It
talks to the server's control API over HTTP, so the same commands work against
a gateway on a customer's cluster and against one on localhost.

Both are static (`CGO_ENABLED=0`). The server ships in a `FROM scratch` image
with no shell. `keera` ships as release archives (`make dist`).

On start, the gateway applies its own schema under a Postgres advisory lock, so
several replicas can start at once and no migration job is needed.

## One listener, four surfaces

Port 8080 carries everything, told apart by path:

| Path       | What it is                                                                                            |
| ---------- | ----------------------------------------------------------------------------------------------------- |
| `/api`     | The inference API and the MCP servers. Authenticated by an API key, as a bearer token or `x-api-key`. |
| `/control` | The control API. Authenticated by the operator key, a panel session or a `keera login` token.         |
| `/sandbox` | Attaching to a sandbox, and the Git credential a sandbox asks for.                                    |
| `/`        | The control panel, and `/metrics`, `/healthz` and `/readyz`.                                          |

`/sandbox` is not JSON. It carries a byte stream between an authenticated
caller and a port inside a sandbox, so attaching needs no second port and no
second certificate. With sandboxes switched off, it answers 501
`sandboxes_disabled`. See [sandboxes.md](sandboxes.md).

A deployment publishes one address and one certificate. The downside is that no
port separates the planes, only the credentials. To separate them, put a proxy
in front that publishes `/api` and nothing else.

`/healthz` checks only the process. `/readyz` also pings Postgres, so a replica
that has lost the database leaves the inference Service instead of being
restarted. `keera-gateway health` asks the gateway's own `/readyz`, for a
container healthcheck in an image that has no curl.

## The alias is the API contract

A client names an alias such as `keera-speed`, never a model id or a vLLM URL.
So you can swap the model behind an alias, change quantization, or point it at a
remote endpoint without any developer changing anything.

- A catalogue file (`KEERA_MODELS_FILE`) is a template. Each new organisation
  starts with a copy of every model it declares. See
  [Models belong to an organisation](#models-belong-to-an-organisation).
- `backend_model` is the name the backend serves the model under - vLLM's
  `--served-model-name`, llama.cpp's `--alias`. If it is wrong, every request
  returns 404.
- `max_context` is shown to clients on `/v1/models`, sizes a filter's or a
  router's own model, and bounds every request. See
  [Requests too long for the model](#requests-too-long-for-the-model).
- `location` says where the model runs, and so where prompts go: `ch` or `usa`
  for a hosted provider, `onprem` for your own inference plane. A provider fills
  it in. Without one, a backend inside your network is `onprem`, and a backend
  on the internet must say where it is. Clients see it on `/v1/models`.
- `release_date` is the day the model came out, as `YYYY-MM-DD`. A provider
  fills it in for the models it knows. `/v1/models` gives it as `created`.
- An alias is lowercase letters, digits and interior hyphens, because it appears
  in the client configurations `keera connect` prints.

## Models belong to an organisation

Every model belongs to one organisation. Only that organisation can call it,
and its administrators add, change and remove it. The backend can be any URL:
your own inference plane, or a provider reached with the organisation's own
key.

An administrator adds one in the panel (**Models → New model**) or with
`keera model add <alias>`. The CLI uses their own organisation, so they never
pass `--org`. An operator picks the organisation in the panel, or passes
`--org <id>` when there is more than one.

A new organisation starts with a copy of each model in the catalogue file
(`KEERA_MODELS_FILE`). The copies are the organisation's own: its
administrators change, disable or remove them like any other model. Changing
the file later does not touch organisations that already exist. To add the
file's models to one, run `keera model apply <file> --org <id>`.

The file holds no API keys. After a new organisation is created, its
administrator stores the key for each hosted model.

Each organisation has its own aliases, so two organisations can each have a
`fast`. The gateway looks up the `model` field in the organisation's models
first, then in its routers. A model and a router in the same organisation
cannot share an alias.

To stop a team or a key using a model, leave it out of that guardrail:
`keera guardrail set team <id> --models a,b`. To stop the whole organisation
using it, disable the model in its edit dialog.

Deleting a model does not change the filters and routers that use it. An
enforcing filter whose model is gone refuses every request it covers; a shadow
one records an error and lets them through. A router leaves the model out of its
choice, or falls back or refuses when it was the model that decides.
The delete confirmation lists them first.

## Three request shapes

The data plane serves the OpenAI API at `/api/v1/chat/completions`,
`/api/v1/completions` and `/api/v1/embeddings`, the Anthropic Messages API at
`/api/v1/messages`, and the OpenAI Responses API at `/api/v1/responses`. Claude
Code uses the Messages API and Codex the Responses API. All of them go through
the same guardrails, accounting and audit.

`GET /api/v1/models` lists the models a key may call, and
`GET /api/v1/models/{alias}` describes one. `HEAD /api/api/hello` answers 200,
for the warm-up probe some Anthropic clients send.

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
They are a table of defaults - endpoint, context window, prices,
description - so that pointing a model at Anthropic or OpenAI takes
three lines instead of six. The provider's name also decides the exception
above.

## Which client sent a request

Every request is recorded with the client that sent it, such as Claude Code,
Codex or curl. The gateway reads it from the `User-Agent`. A client can name
itself instead in `X-Keera-Client`, which wins, so a tool built on a common
HTTP library is not filed under that library. The name is what the client
says, so it tells what is connected, not who may connect.

`keera usage --by client` adds up usage by client.

## The tenancy model

Three levels: organisation, team, key. A guardrail can attach at any of them.

The combining rule is **restrict-only**. A level can narrow what it inherits but
never widen it. Allow-lists intersect; the output-token ceiling takes the
minimum; once a level blocks hosted tools, the levels below cannot unblock them.

The exceptions:

- **Rate limits and budgets are kept per level and checked separately**, not
  merged. An organisation cap of 10,000 and a team cap of 1,000 are two limits
  that must both hold, and each level's `rpm` and `tpm` has its own bucket.
- **System prompts and filters accumulate**, outermost first. A level may add to
  what it inherits but may not drop it.
- **Only the organisation grants repositories** to sandboxes. A team or key can
  only narrow the list.

A budget with no period is a monthly one. Budgets reset on UTC boundaries in
every deployment.

A level that sets nothing is not unlimited: it gets whatever it inherits. To see
the combined result, run `keera guardrail effective <scope> <id>` (or
`GET /control/v1/guardrails/{scope}/{id}/effective`). The endpoint returns every
level's guardrails and the combined answer; the command also shows which level
decided each value.

## What happens to one request

1. Authenticate the key, and resolve the guardrails attached to its
   organisation, its team and itself.
2. Read the body, find the model, check the rate limits and the budgets.
3. **Router**, if the client named one: choose the destination.
4. Refuse the request if it cannot fit in the destination's context.
5. **Filters**, outermost level first: each may rewrite the request or refuse it,
   with a model and an instruction or with a list of expressions.
6. Prepend the standing system prompt, take out blocked hosted tools, and clamp
   the output ceiling.
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

A model with no `max_context` is not checked. Embedding and completion inputs
are checked one by one, since each has to fit on its own.

## Streaming

A coding agent keeps one response open for minutes. So the gateway limits only
how long the inference plane may take to _start_ responding
(`KEERA_UPSTREAM_HEADER_TIMEOUT`, two minutes by default), never the response
itself. A router uses the same timeout to decide that a destination did not
answer in time.

Usage is recorded once per request, when it ends, from the token counts the
inference plane reported. If the client disconnects before those arrive, the
request is charged on an estimate and marked as estimated.

If you put an ingress in front, turn off response buffering and set a read
timeout longer than the longest completion. Most controllers buffer by default.

## Caching

The gateway keeps an in-memory view of the control plane (`internal/registry`),
so a request waits on the database only to check a key it has not seen
recently. A change made through the control API is pushed to every replica at
once, over Postgres LISTEN/NOTIFY, so a revoked key stops working right away. In
case a notification is lost, a checked key is reused for at most
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

Answers say what is left of the allowance, so a client can see a limit coming.
The budget headers are sent when some level has a budget, and the request
headers when some level has an `rpm`. They describe only the tightest level:

| Header                           | What it holds                       |
| -------------------------------- | ----------------------------------- |
| `X-Keera-Budget-Remaining`       | what is left to spend               |
| `X-Keera-Budget-Limit`           | the budget                          |
| `X-Keera-Budget-Reset`           | when it resets, in RFC 3339         |
| `X-Keera-Budget-Scope`           | the level: `org`, `team` or `key`   |
| `X-Keera-Budget-Currency`        | the currency, from `KEERA_CURRENCY` |
| `X-RateLimit-Limit-Requests`     | the `rpm`                           |
| `X-RateLimit-Remaining-Requests` | requests left in the bucket         |
| `X-Keera-RateLimit-Scope`        | the level: `org`, `team` or `key`   |

A `tpm` limit has no header. When a limit is hit, the message says which limit
and its value. A rate limit answers 429 with `Retry-After`. A budget answers
402, and its message gives the day it resets.

Other response headers:

| Header                  | What it holds                                                                             |
| ----------------------- | ----------------------------------------------------------------------------------------- |
| `X-Keera-Model`         | the alias that answered; behind a router, the destination it chose                        |
| `X-Keera-Router`        | the router and where it sent the request ([routers.md](routers.md#what-a-client-can-see)) |
| `X-Keera-Filters`       | the filters that acted on the request ([filters.md](filters.md#what-is-recorded))         |
| `X-Keera-Removed-Tools` | the hosted tools a guardrail took out ([mcp.md](mcp.md#hosted-tools))                     |
| `X-Request-Id`          | the request's id; one the client sends is kept                                            |

## Errors

Errors come in the shape of the API that was called. The codes:

| Status | Code                                                                                                                  |
| ------ | --------------------------------------------------------------------------------------------------------------------- |
| 400    | `missing_model`, `invalid_body`, `unsupported_parameter`, `context_length_exceeded`                                   |
| 401    | `missing_api_key`, `invalid_api_key`, `expired_api_key`, `revoked_api_key`                                            |
| 402    | `budget_exceeded`                                                                                                     |
| 403    | `filter_refused`                                                                                                      |
| 404    | `model_not_found`: no such model, or the key may not use it                                                           |
| 413    | `request_too_large` (`KEERA_MAX_BODY_BYTES`), `filter_input_too_large`                                                |
| 429    | `rate_limit_exceeded` (`rpm`), `token_rate_limit_exceeded` (`tpm`)                                                    |
| 502    | `upstream_unavailable`, `filter_failed`                                                                               |
| 503    | `control_plane_unavailable`, `no_backend`, `filter_unavailable`, `router_undecided`, `router_destination_unavailable` |

## Metrics

`/metrics` is the Prometheus exposition. It needs the operator key, an
operator's session, or `KEERA_METRICS_TOKEN`, because its labels name every
organisation.

| Metric                           | Labels             | What it counts                                           |
| -------------------------------- | ------------------ | -------------------------------------------------------- |
| `keera_requests_total`           | model, org, status | inference requests                                       |
| `keera_tokens_total`             | model, org, status | input plus output tokens                                 |
| `keera_request_duration_seconds` | model, org, status | wall time of a request                                   |
| `keera_gateway_overhead_seconds` | model, org         | time the gateway added, not counting filters and routers |
| `keera_upstream_errors_total`    | model, org         | failed attempts at a model                               |
| `keera_inflight_requests`        |                    | requests open against the inference plane now            |
| `keera_ratelimit_fallback_total` |                    | rate-limit decisions made without Redis                  |

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
- **It does not translate request parameters between providers**, with one
  exception for OpenAI: it sends `max_completion_tokens` instead of `max_tokens`
  to every OpenAI chat model, and drops `temperature` and `top_p` for its
  reasoning models, which refuse them. Any other field a hosted provider
  rejects is an error the client sees.
- **It does not retry.** A model with several backends takes them in turn,
  round-robin, and moves to the next one only when it cannot connect, so a
  request reaches at most one. A router moves on to its next destination when
  one cannot be reached, is too slow to start answering, or answers 5xx. Nothing
  sends the same request to the same model twice.

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
