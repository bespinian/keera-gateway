# Sizing and retention

Most of what Keera stores grows with the number of customers, so it barely
grows. Three tables grow with traffic.

## What grows

| Table           | One row per                           | Grows with        |
| --------------- | ------------------------------------- | ----------------- |
| `usage_events`  | inference request                     | traffic           |
| `filter_runs`   | filter run, per request               | traffic × filters |
| `tool_calls`    | MCP tool call                         | traffic           |
| `audit_log`     | administrative action                 | people, slowly    |
| everything else | org, team, key, model, filter, router | customers         |

`usage_events` is the busiest table. It is the request log, the report source,
the session report and the billing record. Every completed request writes one
row.

A row is mostly small integers and short ids, plus two variable parts:

- **`spans`**, the latency breakdown, is a few hundred bytes. Each step is a
  name, two integers and sometimes a short note.
- **`error`**, on failed requests, holds the message the client got.

`session_key` adds seventeen characters to a chat request's row, plus one
partial index over the rows that have one.

**No prompt or completion is stored**, not even a fragment or a length. What a
filter or router read, and what a session was about, is reduced to counts and
hashes.

## Retention

```sh
KEERA_USAGE_RETENTION=8760h    # a year
KEERA_AUDIT_RETENTION=17520h   # two years
```

**Both default to for ever, on purpose.** Billing reads usage, and compliance
reads the audit log, so the gateway never deletes either unless told to. If
you leave them unset, everything is kept and these tables keep growing.

At start-up the gateway logs which windows are set, or `no retention window is
configured` when neither is.

What `KEERA_USAGE_RETENTION` covers:

- `usage_events` past the cutoff.
- `filter_runs` past the cutoff. It describes the same requests, so there is
  no point keeping it longer.
- `tool_calls` past the cutoff: the other half of the same log. See
  [mcp.md](mcp.md).
- Closed `spend` windows. Nothing reads them: budgets check the open day and
  month, and reports aggregate the events directly.

Keep in mind:

- **The request log, the session report and every per-team, per-key and
  per-model chart read `usage_events`.** They all stop where retention stops.
  With 90-day retention, they cover only the last 90 days.
- **Deleted rows cannot be recovered.** Anything under `24h` is refused at
  start-up.

Retention runs hourly and deletes in batches of 5,000, so it does not block the
replicas that write to the usage log. Each pass logs how many rows it deleted.

## Read cost

Reports aggregate `usage_events` by time and organisation, using these
indexes: `(ts)`, `(org_id, ts)`, `(team_id, ts)`, and a partial
`(org_id, session_key, ts)` over the rows with a session key.

One measurement, from the session report, the most expensive read:

> On 400,000 recorded requests, one organisation's 30-day window - 100,000
> requests in 10,000 conversations - takes about 250 ms, and its last 7 days
> about 13 ms.

Reading _one_ session is a range scan over that conversation. It takes under a
millisecond, however much traffic the deployment has.

The Sessions screen makes two passes: one for the page of rows, one for the
totals over the window. The request log makes three, because totals computed
from the current page would change as you scroll.

## Sizing the database

There is no fixed number. Measure it:

1. Run a representative week.
2. Read the table sizes:
   `SELECT pg_size_pretty(pg_total_relation_size('usage_events'));`
3. Multiply by the retention window you want. Include the indexes, which are a
   large share of this table.

A coding agent makes thirty or forty calls per developer instruction, so row
counts follow _tasks × calls per task_, not the number of developers.
[sessions.md](sessions.md) shows how to read your actual ratio.

## Sizing the gateway

The gateway keeps an in-memory view of the control plane (`internal/registry`),
refreshed every `KEERA_CACHE_TTL`. Its size depends on the number of orgs,
teams, keys, models, filters and routers, not on traffic.

Per request, it holds the request body, up to `KEERA_MAX_BODY_BYTES` (32 MB by
default). For a non-streamed answer it also holds the response body, up to
`KEERA_MAX_RESPONSE_BYTES`. Streamed answers are forwarded as they arrive.

`KEERA_MAX_DB_CONNS` is 16 per replica by default. Size Postgres for replicas ×
that.

Rate limits multiply too: the token buckets are per process, so N replicas
admit up to N times the per-minute limit. `KEERA_REDIS_URL` moves them to a
shared store; see [gateway.md](gateway.md). Budgets are already reconciled in
Postgres and need nothing.

Redis holds two small fields per active bucket, keyed by scope. They expire one
refill after last use, so Redis size depends on how many organisations, teams
and keys send traffic at once.

## Sizing the inference side

This is where the cost is, and it is mostly outside the gateway. Two gateway
features add to it:

- **A rewrite or gate filter adds a second generation** to every request its
  guardrails cover. The filter's model shares GPUs with the model it guards. A
  pattern filter adds none: it runs in the gateway and never calls a backend.
- **An instruction router adds a generation** to every request that names it.
  The size, fallback, latency and least-busy modes add none.

So keep those models small, local and fast, and prefer the modes that need no
model where they fit. [filters.md](filters.md) and [routers.md](routers.md) list
what to measure and the screens that show it.
