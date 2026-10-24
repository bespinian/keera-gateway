# Sessions

A session is the requests one coding agent made working on one task.

To read them, see
[Requests and sessions](https://docs.keera.ch/monitor/requests-and-sessions/).
This page explains how sessions are found and why.

## What defines a session

Two things, and both are needed:

1. **The conversation.** The gateway puts a `session_key` on every chat request
   it records. Requests with the same key are in the same conversation.
2. **The idle gap.** A conversation's requests are split wherever more than
   [`KEERA_SESSION_GAP`](#configuration) passed between two of them.

The gap is needed on its own. A client that opens every task with the same words
("continue", "fix the tests") would otherwise have one session weeks long. And a
developer who resumes yesterday's conversation this morning is starting today's
task.

Tool calls through the gateway's MCP proxy are shown with the session they
belong to; [mcp.md](mcp.md#the-tool-call-log) says how they are matched.

## How the conversation is identified

In order of preference.

### A client-stated id

A request with any of these headers is in the session that header names.

| Header                     | Why it is read                                                                     |
| -------------------------- | ---------------------------------------------------------------------------------- |
| `X-Keera-Session`          | This gateway's own. It wins if more than one is sent.                              |
| `X-Session-Id`             | The generic convention.                                                            |
| `Helicone-Session-Id`      | Helicone's proxy groups requests by it, so clients written for such a gateway fit. |
| `X-Claude-Code-Session-Id` | Claude Code sends it on every call, its subagents' too, so it needs no setup.      |

The value is hashed, so the client's own conversation id is not stored. Which of
the headers carries it makes no difference to the key.

**There is no standard for this.** Neither the OpenAI nor the Anthropic API has
a field for the conversation; their `user` fields name a person, which would
put every task a developer ran into one session. Trace ids do not fit either.
A Responses client that sends only the new turn, with `previous_response_id`,
looks like a new task on every request unless it states a session.

**The client must compute a stated id per conversation.** A constant in a static
configuration - the same `X-Session-Id` on every request - merges that key's
whole history into one session. The panel marks a stated session as _named by
the client_, so a single session of nine thousand requests is easy to spot.

### Otherwise, the prompt that opened the task

Without a header, the gateway hashes **the first message with role `user`** in
the request, together with the id of the API key that sent it. That message is
the same, byte for byte, on the first request of a task and on its fortieth.

**The system prompt is left out on purpose.** Agents put the date, the working
directory and the git branch there. Including it would split a task at midnight
or when the developer changes directory.

**The key id is mixed in.** Two developers who open a task with the same
sentence get two sessions, and another key can never join a key's session.

**Only the chat surfaces get a session.** `/api/v1/chat/completions`,
`/api/v1/messages` and `/api/v1/responses` carry a conversation.
`/api/v1/completions` carries a bare prompt that is different on every request, and
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
id in `X-Keera-Session`. The gateway configures Pi and Claude Code to do so; an
image that adds another agent has to configure it. The sandbox's session key is
computed once at creation, the same way as per request, so a sandbox and its
task can be read side by side. See [sandboxes.md](sandboxes.md).

## Sessions are cut when the report is read

Nothing is stored per session, so recording a request stays one insert. This
means:

- **The gap can change later.** A new `KEERA_SESSION_GAP` re-cuts every session
  already recorded.
- **It works across replicas.** The key depends only on the request, so two
  gateways serving one conversation write the same one.
- **A session is named by its first request.** Any request in it leads to it,
  so one row in the request log opens the whole task.

A task already under way when the report's window opens keeps up to one gap of
what came before, so it does not show only its last minutes. Opening the
session shows it whole.

## Reading the report

How to use the screen and the command:
[Requests and sessions](https://docs.keera.ch/monitor/requests-and-sessions/#sessions).
The API is `GET /control/v1/sessions`, in the
[API reference](https://docs.keera.ch/reference/api/). Beyond those:

- A member sees only the sessions of their own keys, and so do the live map
  and the request log.
- A filter set on Sessions carries over to Requests, and back. A session's
  status is its last request's, the message its client got, so a task that
  stopped on a spent budget shows 402.
- On a project's, key's or model's page, **Outcome** filters instead. Failed
  means at least one failed request, Success that every request succeeded.
- Ranking by a column ranks the whole window on the server, but shows only the
  top 100, since **Load more** works only in time order. **Live** updates the
  screen as requests arrive.
- `keera session list` also takes `--model`, `--project`, `--since` and
  `--user`. `keera session show` takes the id of any request in the task, such
  as the first column of `keera failures`.

## Narrowing, and the one filter that is not what it looks like

`project_id`, `key_id` and `user_id` filter the rows before the runs are cut.
That is safe: a session belongs to exactly one key, so to one project and one
person.

`alias` filters the **sessions**, not the rows. Filtering rows would cut a task
that used a hosted model in its middle into three pieces, reported as three
tasks. Filtering sessions picks the ones that used that model and still reports
each one whole.

## What it costs

Recording costs one SHA-256 per chat request and seventeen characters per row.
Reading walks every request in the window: a 30-day window of 100,000 requests
takes about 250 ms. Reading one session takes under a millisecond.

## Configuration

[`KEERA_SESSION_GAP`](install.md#everything-else) is the idle time that ends a
task. Within a task,
requests are seconds or a minute or two apart; between tasks there is a
meeting, a lunch or a night. Much under ten minutes splits one task into
several, much over an hour joins an afternoon's tasks into one. Sessions are
cut when they are read, so after a restart a new value applies to every
recorded request too. Read a busy week at two or three values before choosing
one.
