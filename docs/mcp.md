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

It speaks MCP's Streamable HTTP transport and passes everything else through
unchanged: sessions, notifications, the server's own requests, resources and
prompts.

## Adding a server

On the command line:

```sh
keera mcp add github --endpoint https://api.githubcopilot.com/mcp/ \
  --description "Issues and pull requests" --api-key @-
```

`@-` reads the credential from stdin, so it stays out of the shell history. It
is stored encrypted under `KEERA_SECRET_KEY`, like a hosted model's key. To keep
it out of the database, name an environment variable with `--api-key-env`
instead.

The credential is sent in `Authorization` as a bearer token. For a server that
reads another header, pass `--auth-header X-Api-Key`; the credential is then
sent as is.

Or in the catalogue file, next to the models:

```yaml
mcp_servers:
  - alias: github
    url: https://api.githubcopilot.com/mcp/
    description: Issues and pull requests.
    api_key_env: GITHUB_TOKEN
```

As with models, a server declared in the file belongs to the file. The command
line and the panel cannot change it, except for its stored credential.

In the panel, the **MCP** page does the same: **New MCP server**, and Edit and
Remove on each row. It lists every server with the address to give clients. An
administrator also sees each tool's calls and the latest ones. `keera mcp list`
shows the same list in a terminal.

Like Filters and Routers, the page sits under **More** in the sidebar until the
deployment has its first server.

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
keera guardrail set team <team-id> --tools github/search_code
```

An entry is a server alias, for all its tools, or `alias/tool` for one tool.
Each level narrows the one above. Here the team may call only
`github/search_code`: the organisation allows no other jira tool, and the team
allows no other github one. A scope that sets nothing may call every tool of
every server.

A tool the key may not call is left out of `tools/list`, so the agent never sees
it. If called anyway, it is refused as an unknown tool and recorded as `denied`.
A server where the key may call no tool answers 404, like a model it may not
use.

`keera guardrail effective` shows the tools in force and which level narrowed
them.

## Filters

The key's filters run over every string in a call's arguments before the call is
forwarded - the same filters, in the same order, as on its model requests. A
rewrite goes to the server. A refusal comes back to the agent as a failed tool
result that says why, so the model can carry on.

Results are not filtered here. A result goes into the agent's next model
request, and the filters read it there.

## The tool-call log

```sh
keera mcp calls --since 1h
keera mcp calls --summary --since 168h
```

Each call is one row: server, tool, outcome, time, bytes sent and received, the
key, and the error if any. The outcomes are:

| Outcome      | Means                                                |
| ------------ | ---------------------------------------------------- |
| `ok`         | the tool answered                                    |
| `tool_error` | the tool answered that it failed                     |
| `error`      | no result: the server failed or could not be reached |
| `denied`     | the key may not call this tool                       |
| `refused`    | a filter stopped the call                            |

A session's screen in the panel lists its tool calls under its model calls. If
the client names the session with `X-Keera-Session`, send the same header to the
MCP server and the tool calls are matched by name. Other sessions are matched by
time - the key's calls between the session's first and last request - so a key
running two tasks at once shows both tasks' calls in each.

Tool calls are kept as long as the request log (`KEERA_USAGE_RETENTION`).
`keera_tool_calls_total` on `/metrics` counts them by server and outcome.

## Hosted tools

Anthropic and OpenAI can run tools on their side: web search, URL fetch, code
execution, remote MCP servers. The provider makes those calls, so the gateway
never sees them.

```sh
keera guardrail set org <org-id> --block-hosted-tools
```

removes those tools from every request before it is forwarded, including
Anthropic's `mcp_servers` and OpenAI's `web_search_options`. Tools the client
runs itself are kept: its own functions, and Anthropic's bash, editor and
computer tools. The response header `X-Keera-Removed-Tools` names what was
removed. Once a level blocks hosted tools, no level below can unblock them.

## Limits

- **One identity upstream.** The server sees the gateway's credential for every
  key, so its own per-user permissions do not apply. The allow-list is the
  control. Signing each person in to the server is not built yet.
- **Only remote servers.** An MCP server that an agent starts as a local process
  never passes through the gateway. For those, give the agent a
  [sandbox](sandboxes.md), whose egress is enforced.
- **One tool call per request.** A JSON-RPC batch that holds a tool call is
  refused. MCP removed batches in its 2025-06-18 version.
