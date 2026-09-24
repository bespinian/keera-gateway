# Sessions

A session is the calls one coding agent made working on one task.

Other reports count requests. But one instruction from a developer becomes
thirty or forty requests as the agent reads files, calls tools and answers
again. "Four rappen a call" is not something anybody can act on. "One franc
twenty a task, eleven minutes, forty-one calls" is the number for a budget
conversation.

## What defines a session

Two things, and both are needed:

1. **The conversation.** The gateway puts a `session_key` on every chat request
   it records. Requests with the same key are in the same conversation.
2. **The idle gap.** A conversation's requests are split wherever more than
   `KEERA_SESSION_GAP` passed between two of them. Half an hour by default.

The gap is needed on its own. A client that opens every task with the same words
("continue", "fix the tests") would otherwise have one session weeks long. And a
developer who resumes yesterday's conversation this morning is starting today's
task.

## How the conversation is identified

In order of preference.

### A client-stated id

A request with any of these headers is in the session that header names.

| Header                | Why it is read                                                                  |
| --------------------- | ------------------------------------------------------------------------------- |
| `X-Keera-Session`     | This gateway's own. It wins if more than one is sent.                           |
| `X-Session-Id`        | The generic convention.                                                         |
| `Helicone-Session-Id` | Helicone's proxy groups calls by it, so clients written for such a gateway fit. |

```sh
curl http://127.0.0.1:8080/api/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H "X-Keera-Session: $MY_CONVERSATION_ID" \
  -H 'Content-Type: application/json' -d @request.json
```

The value is hashed, so the client's own conversation id is not stored. Which of
the three headers carries it makes no difference to the key.

**There is no standard for this.** The closest options, and why they do not fit:

- Neither the OpenAI API nor the Anthropic Messages API has a field for the
  conversation. The nearest is a _person_ - OpenAI's `user` field, Anthropic's
  `metadata.user_id` - and grouping by person would put every task a developer
  ever ran into one session.
- W3C Trace Context has the wrong shape. `traceparent` names a trace, and tools
  that give each completion its own trace would make every call its own task.
- OpenTelemetry's GenAI conventions name the concept, `gen_ai.conversation.id`,
  but only as a span attribute, not on the wire.
- The OpenAI Responses API's `conversation` and `previous_response_id` name a
  conversation stored at OpenAI, which the gateway cannot see. A client that
  sends only the new turn with them looks like a new task on every request,
  unless it states a session.

**The client must compute a stated id per conversation.** A constant in a static
configuration - the same `X-Session-Id` on every request - merges that key's
whole history into one session. The panel marks a stated session as _named by
the client_, so a single session of nine thousand calls is easy to spot.

### Otherwise, the prompt that opened the task

Without a header, the gateway hashes **the first message with role `user`** in
the request, together with the id of the API key that sent it. That message is
the same, byte for byte, on the first call of a task and on its fortieth.

**The system prompt is left out on purpose.** Agents put the date, the working
directory and the git branch there. Including it would split a task at midnight
or when the developer changes directory.

**The key id is mixed in.** Two developers who open a task with the same
sentence get two sessions, and another key can never join a key's session.

**Tool calls through the gateway's MCP proxy are shown with the session.** A
call that names a session carries the same header and is matched by it; any
other is matched by key and time. See [mcp.md](mcp.md#the-tool-call-log).

**Only the chat surfaces get a session.** `/api/v1/chat/completions`,
`/api/v1/messages` and `/api/v1/responses` carry a conversation.
`/api/v1/completions` carries a bare prompt that is different on every call, and
`/api/v1/embeddings` has no conversation at all. Requests to those two belong to
no session.

### What is actually stored

Twelve bytes of SHA-256, base64-encoded, with one character in front for where
it came from: `c` for a client-stated id, `p` for a hash of the opening prompt.
Seventeen characters on the row, and nothing else.

```
pU6F5haDO6VBv8V-H     inferred from the opening prompt
cP2poiSV7MJegn5py     the client named this session itself
```

The hash cannot be reversed or searched for a phrase. A database dump shows only
that two requests belong together. No prompt, no part of one and no length of
one is stored.

The hash covers at most 128 KiB of the opening message.

## What it cannot promise

The derived key is a good guess, not a fact:

**A client that rewrites its own opening prompt starts a new session there.**
Some agents compact a long conversation by summarising its start. When the first
user message changes, so does the key.

**Two tasks opened with the same sentence within the gap count as one.** A
developer who types "fix the tests" twice in twenty minutes has one session.

The stated headers exist for both cases. The panel shows whether a session was
stated or inferred.

An agent sandbox avoids the problem: it is exactly one task, so it sends its own
id in `X-Keera-Session`. See [sandboxes.md](sandboxes.md).

## How the grouping is computed

At read time, not at write time. Nothing is stored per session, so the write
path stays a single insert with no lookup. The read path walks one
conversation's rows in time order and counts the gaps it passes; that count is
the session number.

This means:

**The threshold can change later.** Changing `KEERA_SESSION_GAP` re-cuts every
session already recorded, not only new ones.

**It works across replicas.** Two gateways serving one conversation write the
same key, because the key depends only on the request.

**A session has no stored id**, so it is named by the id of the request that
opened it. Any request in it leads to it, so one row in the request log is
enough to open the whole task.

### The window edge

A session already under way when the report's window opens is reported whole.
The walk starts one gap before the window and drops the sessions that ended in
that extra span. Otherwise a task that had run for ten minutes would show only
what its last two minutes cost.

## Reading the report

### The panel

**Sessions**, under Administration, next to Requests. Administrators only, like
the request log, because the rows name other people's keys.

The screen starts with per-task figures for the window. Each is a median, with
the maximum below it. Then come the rankings by **Cost**, **Calls** and
**Time**, which are the point of the screen. The row worth reading is rarely the
latest one.

Opening a session shows its four numbers, every call in order, and a strip with
one bar per call, sized by cost and coloured by outcome. Forty even bars is an
agent working. A long tail of identical small bars is an agent looping. A red
bar two thirds along is where the task went wrong; click it to open that call.

Each row also shows how the task **ended**: the outcome of its last call and the
message its client got. A task of forty calls that stopped on a spent budget did
not finish.

### The command line

```sh
keera sessions                        # ranked by cost, the last seven days
keera sessions --sort requests        # the tasks that would not stop
keera sessions --unhappy              # only the ones that hit trouble
keera sessions --model keera-frontier # the tasks that reached the hosted model
keera sessions --team <team-id> --since 720h
keera session <request-id>            # one task, from beginning to end
```

`keera sessions` ranks by cost, not by recency. `keera session` takes the id of
**any** request in the task, which is what `keera failures` and the request log
show.

### The API

```
GET /v1/sessions      ?org_id&from&to&since&sort&unhappy&alias&team_id&key_id&user_id&limit&before
GET /v1/sessions/{id}  where {id} is any request in the session
```

Both return `gap_seconds`, the threshold used for the grouping. `format=csv` on
the list exports the whole filtered window, one row per task.

Paging with `before` works only with the default order. Under a ranking the
cursor would skip rows without warning.

## Narrowing, and the one filter that is not what it looks like

`team_id`, `key_id` and `user_id` filter the rows before the runs are cut. That
is safe: a session belongs to exactly one key, so to one team and one person.

`alias` filters the **sessions**, not the rows. Filtering rows would cut a task
that used a hosted model in its middle into three pieces, reported as three
tasks. Filtering sessions picks the ones that used that model and still reports
each one whole.

## What it costs

**On the write path, nothing measurable.** One SHA-256 over the opening message
per chat request, no lookup, no extra round trip.

**In storage**, seventeen characters per row of `usage_events`, and one partial
index over the rows that have one.

**On the read path**, the walk. Its cost grows with the number of requests in
the window, not the number of tasks.

On 400,000 recorded requests, one organisation's 30-day window - 100,000
requests in 10,000 conversations - takes about 250 ms, and its last 7 days about
13 ms. The screen makes two walks: one for the page of rows and one for the
totals over the window, so the totals do not change as somebody scrolls.

The index is `(org_id, session_key, ts)`, partial on the rows that have a key.
It makes reading _one_ session a range scan over that one conversation: under a
millisecond, however much traffic the deployment has.

## Configuration

| Variable            | Default | What it does                                                                                                 |
| ------------------- | ------- | ------------------------------------------------------------------------------------------------------------ |
| `KEERA_SESSION_GAP` | `30m`   | How long a conversation may go quiet before the next request on it starts a new session. Refused below `1m`. |

Thirty minutes fits how a coding agent is used. Within a task, calls are seconds
or a minute or two apart. Between tasks there is a meeting, a lunch or a night.
Much under ten minutes starts splitting one task into several. Much over an hour
starts joining an afternoon's tasks into one.

Grouping happens when the report is read, so changing this is safe and takes
effect at once. Read a busy week at two or three values before choosing one.
