# MCP servers

Tool calls send data out of the organisation just like model calls do: to an
issue tracker, a chat channel, a cloud API. So the gateway sits in front of MCP
servers too.

How to add a server and connect an agent:
[MCP servers](https://docs.keera.ch/manage/mcp/). This page explains how it
works and why.

A client reaches a server at `<gateway>/api/mcp/<alias>` with its Keera key. The
gateway speaks MCP's Streamable HTTP transport, up to its 2026-07-28 version. It
acts on tool calls and the tool list, and passes everything else through
unchanged: sessions, notifications, the server's own requests, resources and
prompts.

Since 2026-07-28 a request also names its method and target in headers:
`Mcp-Method`, `Mcp-Name` and `Mcp-Param-*`. The gateway checks `Mcp-Method` and
`Mcp-Name` against the body and refuses a mismatch with a 400 and JSON-RPC error
-32020, as the server would. A load balancer behind the gateway may route on
these headers, so they must say what the body says.

## Adding a server

A server belongs to one organisation, like a model. Only that organisation's
keys reach it, and its administrators add, change and remove it. Two
organisations can each have a `github`.

A credential is stored encrypted under `KEERA_SECRET_KEY`, like a hosted
model's key.

With `--auth-header`, the credential is sent as is, without `Bearer`. A header
name holds only letters, digits and hyphens. `--no-api-key` removes the stored
credential.

Moving a server to another host needs the credential again, or
`--no-api-key`: a stored credential only goes to the host it was entered for.

`--disabled` adds a server without serving it yet, and `keera mcp enable` and
`keera mcp disable` switch it later. A disabled server answers 404, like one
that does not exist.

## Connecting a client

`keera mcp connect` prints the configuration
([Connect your tools](https://docs.keera.ch/use/connect/#mcp-servers)). The key
is the same one the agent uses for models. The shell expands
`$KEERA_API_KEY`, so Claude Code stores the key in its settings. Codex and an
`mcp.json` read it from the environment each time they start.

## Which tools a key may call

A guardrail sets this
([Choose which tools a key may call](https://docs.keera.ch/manage/mcp/#choose-which-tools-a-key-may-call)).
Each level narrows the one above: a tool must be on every level's list. So an
organisation list of `github,jira/search` and a project list of
`github/search_code` leave the project only `github/search_code`. A level that
sets no list gets the list of the level above, and `--no-tools` clears a level's
list. If no level sets one, the key may call every tool of every server.

A server named in a list cannot be deleted. The refusal names the guardrails
that list it; take it off them first.

A tool the key may not call is left out of `tools/list`, so the agent never sees
it. If called anyway, it is refused as an unknown tool and recorded as `denied`.
A server where the key may call no tool answers 404, like a model it may not
use. A [subscription key](subscriptions.md#what-a-subscription-key-can-do)
is refused by every server: with a 401 `subscription_key` when it is sent in
`Authorization`, and with a 404 `mcp_server_not_found` when it is sent in
`X-Keera-Key`. While `KEERA_CLAUDE_SUBSCRIPTIONS` is off, both get a 403
`subscriptions_off`. A server inside your network answers a 403 `org_limited`
while the organisation [has not paid yet](sso.md#what-a-new-organisation-can-use).
Because the list depends on the key, a list the server marks `public` for
caching is passed on as `private`.

A key that may call only some of a server's tools reaches only its tools. The
server's resources, prompts and completions are left out of what `initialize`
says it offers, and requests for them are refused with JSON-RPC error -32601.
So is `tasks/list`: the server sees one identity for every key, so it would
list other people's tasks. A key allowed the whole server reaches all of it.

## Rate limits and budgets

A tool call counts as one request against the key's rate limits per minute, day
and month, like a model request. It is refused while any of them, for requests
or tokens, is used up. A call costs nothing unless
the key has filters, so only then does a budget stop it. If one of those filters
runs on a model on the deployment's provider key, the call also needs
[prepaid credit](billing.md#pay-in-advance). A refused call comes back as a
failed tool result that says which limit, and is recorded as `denied`. Other
MCP requests, such as `tools/list`, are not counted.

## Filters

The key's filters run over every string in a call's arguments before the call is
forwarded - the same filters, in the same order, as on its model requests. A
rewrite goes to the server, and so does an `Mcp-Param-*` header that carried the
old value. A refusal comes back to the agent as a failed tool result that says
why, so the model can carry on.

Results are not filtered here. A result goes into the agent's next model
request, and the filters read it there.

## The tool-call log

`keera mcp calls` reads the log
([See the calls](https://docs.keera.ch/manage/mcp/#see-the-calls)). It is for
administrators. A member sees their own tool calls in
[`keera session show`](sessions.md).

Each call is one row: server, tool, outcome, time, bytes sent and received, the
key, and the error if any. Besides the outcomes listed there, `error` also means
a filter could not run, and `input_required` means the server asked for the
user's input first; the retry is a row of its own.

A session's screen in the panel lists its tool calls under its model calls:
the first 1,000 between the session's first and last request. If the client
names the session with one of the
[session headers](sessions.md#a-client-stated-id), send the same header to the
MCP server and the tool calls are matched by name. Other
sessions are matched by key, so a key running two tasks at once shows both
tasks' calls in each.

When the server itself fails, the client gets a 502: `mcp_credential_refused`
when it rejects the stored credential (401 or 403), and `mcp_unavailable` when
it cannot be reached. An answer that is neither JSON nor an event stream is
passed on with its status only, not its body.

Tool calls are kept as long as the request log (`KEERA_USAGE_RETENTION`).
`keera_tool_calls_total` on `/metrics` counts them by server, organisation and
outcome, and `keera_tool_call_duration_seconds` times them.

## Hosted tools

Anthropic and OpenAI can run tools on their side: web search, URL fetch, code
execution, remote MCP servers. The provider makes those calls, so the gateway
never sees them, and the allow-list above does not apply to them.

On a model with the organisation's own key, they are passed on as sent. On a
model with the deployment's own provider key ([billing.md](billing.md)), they
are removed from every request before it is forwarded, including Anthropic's
`mcp_servers` and OpenAI's `web_search_options`. Through a router, that happens
when any model the request may reach is on the deployment's key. The provider charges for them
on top of tokens, and Keera does not bill that.

Tools the client runs itself are kept: its own functions and namespaces of
them, Anthropic's bash, editor, computer and memory tools, and OpenAI's shell,
local shell, computer use and apply-patch tools. Every other tool type is removed, including ones
released after this build. The response header `X-Keera-Removed-Tools` names
what was removed.

## Limits

- **One identity upstream.** The server sees the same credential for every key:
  the organisation's own, stored on the server entry. So its own per-user
  permissions do not apply. The allow-list is the control. Signing each person
  in to the server is not built yet.
- **Only remote servers.** An MCP server that an agent starts as a local process
  never passes through the gateway. For those, give the agent a
  [sandbox](sandboxes.md), whose egress can be limited
  ([The security boundary](sandboxes.md#the-security-boundary)).
- **One tool call per request.** A JSON-RPC batch that holds a tool call is
  refused. MCP removed batches in its 2025-06-18 version.
- **A tool call needs an id.** One sent as a notification is refused with
  JSON-RPC error -32602, because it would get no answer to check or record.
