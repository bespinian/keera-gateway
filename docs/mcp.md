# MCP servers

Tool calls send data out of the organisation just like model calls do: to an
issue tracker, a chat channel, a cloud API. So the gateway sits in front of MCP
servers too.

A client reaches a server at `<gateway>/api/mcp/<alias>` with its Keera key. The
gateway then:

- **shows only the tools the key may call**, and refuses the others,
- **runs the key's filters** over what a call sends, before it leaves,
- **presents the server's credential**, which never reaches a laptop,
- **records every call**: which tool, the outcome, how long it took and how many
  bytes went each way - never the content.

It speaks MCP's Streamable HTTP transport, up to its 2026-07-28 version, and
passes everything else through unchanged: sessions, notifications, the server's
own requests, resources and prompts.

Since 2026-07-28 a request also names its method and target in headers:
`Mcp-Method`, `Mcp-Name` and `Mcp-Param-*`. The gateway checks `Mcp-Method` and
`Mcp-Name` against the body and refuses a mismatch with a 400 and JSON-RPC error
-32020, as the server would. A load balancer behind the gateway may route on
these headers, so they must say what the body says.

## Adding a server

A server belongs to one organisation, like a model. Only that organisation's
keys reach it, and its administrators add, change and remove it. Two
organisations can each have a `github`. An operator with several organisations
passes `--org <id>`.

On the command line:

```sh
keera mcp add github --endpoint https://api.githubcopilot.com/mcp/ \
  --description "Issues and pull requests" --api-key @-
```

`@-` reads the credential from stdin, so it stays out of the shell history. It
is stored encrypted under `KEERA_SECRET_KEY`, like a hosted model's key.

The credential is sent in `Authorization` as a bearer token. For a server that
reads another header, pass `--auth-header X-Api-Key`; the credential is then
sent as is. A header name holds only letters, digits and hyphens.
`--no-api-key` removes the stored credential.

`--disabled` adds a server without serving it yet, and `keera mcp enable` and
`keera mcp disable` switch it later. A disabled server answers 404, like one
that does not exist.

In the panel, the **MCP** screen does the same: **New MCP server**, and Edit and
Delete on each row. It lists every server with the address to give clients. An
administrator also sees each tool's calls and the latest ones. `keera mcp list`
shows the same list in a terminal.

Like Filters, Routers and Sandboxes, the screen is folded into one entry at the
bottom of the sidebar, which names the folded screens, until the organisation
has its first server.

## Connecting a client

`keera mcp connect github` prints the configuration for Claude Code, Codex and
anything that reads an `mcp.json`. For Claude Code:

```sh
claude mcp add --transport http github https://keera.example.ch/api/mcp/github \
  --header "Authorization: Bearer $KEERA_API_KEY"
```

The key is the same one the agent uses for models.

## Which tools a key may call

A guardrail sets this, like the models a key may use:

```sh
keera guardrail set org <org-id> --tools github,jira/search
keera guardrail set project <project-id> --tools github/search_code
```

An entry is a server alias, for all its tools, or `alias/tool` for one tool.
`--tools any` clears a level's list.
Each level narrows the one above: a tool must be on every level's list. Here
the project may call only `github/search_code`. The project's list does not name
`jira/search`, and it names no other github tool. A level that sets no list
gets the list of the level above. If no level sets one, the key may call every
tool of every server.

A server named in a list cannot be deleted. The refusal names the guardrails
that list it; take it off them first.

A tool the key may not call is left out of `tools/list`, so the agent never sees
it. If called anyway, it is refused as an unknown tool and recorded as `denied`.
A server where the key may call no tool answers 404, like a model it may not
use. A [subscription key](subscriptions.md#what-a-subscription-key-can-do)
is refused by every server: with a 401 `subscription_key` when it is sent in
`Authorization`, and with a 404 `mcp_server_not_found` when it is sent in
`X-Keera-Key`. Because the list depends on the key, a list the server marks `public` for
caching is passed on as `private`.

`keera guardrail effective` shows the tools in force and which level narrowed
them.

## Filters

The key's filters run over every string in a call's arguments before the call is
forwarded - the same filters, in the same order, as on its model requests. A
rewrite goes to the server, and so does an `Mcp-Param-*` header that carried the
old value. A refusal comes back to the agent as a failed tool result that says
why, so the model can carry on.

Results are not filtered here. A result goes into the agent's next model
request, and the filters read it there.

## The tool-call log

```sh
keera mcp calls --since 1h
keera mcp calls --summary --since 168h
```

Each call is one row: server, tool, outcome, time, bytes sent and received, the
key, and the error if any. The outcomes are:

| Outcome          | Means                                                                           |
| ---------------- | ------------------------------------------------------------------------------- |
| `ok`             | the tool answered                                                               |
| `tool_error`     | the tool answered that it failed                                                |
| `error`          | no result: the server failed or could not be reached, or a filter could not run |
| `denied`         | the key may not call this tool                                                  |
| `refused`        | a filter stopped the call                                                       |
| `input_required` | the server asked for the user's input first; the retry is a row of its own      |

A session's screen in the panel lists its tool calls under its model calls:
those between the session's first and last request. If the client names the
session with one of the [session headers](sessions.md#a-client-stated-id), send
the same header to the MCP server and the tool calls are matched by name. Other
sessions are matched by key, so a key running two tasks at once shows both
tasks' calls in each.

When the server itself fails, the client gets a 502: `mcp_credential_refused`
when it rejects the stored credential (401 or 403), and `mcp_unavailable` when
it cannot be reached.

Tool calls are kept as long as the request log (`KEERA_USAGE_RETENTION`).
`keera_tool_calls_total` on `/metrics` counts them by server, organisation and
outcome, and `keera_tool_call_duration_seconds` times them.

## Hosted tools

Anthropic and OpenAI can run tools on their side: web search, URL fetch, code
execution, remote MCP servers. The provider makes those calls, so the gateway
never sees them.

```sh
keera guardrail set org <org-id> --block-hosted-tools
```

removes those tools from every request before it is forwarded, including
Anthropic's `mcp_servers` and OpenAI's `web_search_options`. Tools the client
runs itself are kept: its own functions, Anthropic's bash, editor, computer and
memory tools, and OpenAI's shell, local shell, computer use and apply-patch
tools. Every other tool type is removed, including ones released after this
build. The response header `X-Keera-Removed-Tools` names what was removed.

`--allow-hosted-tools` stops a level blocking them. Once a level blocks hosted
tools, no level below can unblock them.

## Limits

- **One identity upstream.** The server sees the same credential for every key:
  the organisation's own, stored on the server entry. So its own per-user
  permissions do not apply. The allow-list is the control. Signing each person
  in to the server is not built yet.
- **Only remote servers.** An MCP server that an agent starts as a local process
  never passes through the gateway. For those, give the agent a
  [sandbox](sandboxes.md), whose egress can be limited
  ([what is enforced](sandboxes.md#what-is-actually-enforced)).
- **One tool call per request.** A JSON-RPC batch that holds a tool call is
  refused. MCP removed batches in its 2025-06-18 version.
- **A tool call needs an id.** One sent as a notification is refused with
  JSON-RPC error -32602, because it would get no answer to check or record.
