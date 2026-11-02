# Keera Agents

Keera Agents runs agents for the people behind a gateway. Someone asks for one
in a chat, such as Slack, Microsoft Teams or Google Chat, and the agent works
in their name. Every request it makes goes through the gateway with a key of
that person's, so their guardrails, budgets and audit log apply as to anything
else they do.

Keera Agents is a separate app on its own address. This page explains what the
gateway does for it, and why. Two settings connect them:

| Variable                   | What it is                                                                                       |
| -------------------------- | ------------------------------------------------------------------------------------------------ |
| `KEERA_AGENTS_URL`         | Keera Agents' address, without a path. People can then sign in to it, and the panel links to it. |
| `KEERA_AGENTS_SERVICE_KEY` | A secret Keera Agents' server sends to the gateway. Unset, the service routes are off.           |

Generate the service key with `openssl rand -hex 32` and give Keera Agents the
same value. The gateway does not start with a service key shorter than 32
characters, or one equal to the operator key, the metrics token or the secret
key. Each credential then does one thing, and a leaked one is replaced alone.

## Signing in

Keera Agents signs people in the way [Keera Chat](sso.md#signing-in-to-keera-chat)
does: the person signs in on the gateway, and the gateway hands a one-time code
to `https://agents.example.ch/auth/callback`, that address exactly. Keera Agents
redeems the code with the proof key it kept, as `keera login` does
([sso.md](sso.md#what-it-does-and-what-makes-that-safe)).

`KEERA_AGENTS_URL` is an origin only: a path, query or fragment stops the start.
It may not be the same address as `KEERA_CHAT_URL`, since both take sign-ins at
`/auth/callback`. Unset, only loopback addresses are allowed, which is enough
for a Keera Agents on `http://127.0.0.1` during development.

Requests that name Keera Agents in `X-Keera-Client` or their `User-Agent` show
as **Keera Agents** on the map.

## The service key

An agent starts from a chat message, not from a person at a browser. So Keera
Agents' server needs to act without a sign-in: to issue a key for an agent, and
to find the person behind a message. It does this with the service key, sent as
`Authorization: Bearer <key>`.

The service key works on the routes under `/control/v1/service/` and nowhere
else. Those routes take nothing else: not the operator key, not a session, not
a `keera login` token. Each side is closed, so:

- a leaked service key cannot read usage, change a guardrail, or reach any
  other route, and
- a person's sign-in cannot issue keys in somebody else's name through these
  routes.

| Route                                  | What it does                                                                                    |
| -------------------------------------- | ----------------------------------------------------------------------------------------------- |
| `POST /control/v1/service/keys`        | Issues a key for an agent, in one person's name, optionally with a monthly budget of its own.   |
| `DELETE /control/v1/service/keys/{id}` | Revokes a key Keera Agents issued. Any other key answers 404.                                   |
| `GET /control/v1/service/users`        | Finds a person in an organisation by address, ignoring case, or by id.                          |
| `GET /control/v1/service/catalog`      | Lists an organisation's projects, and the models, routers and MCP servers a key in one may use. |

The service key is one secret for the whole deployment, so it reaches every
organisation. What keeps tenants apart is that every request names its
organisation, and the gateway checks that the person, project or key belongs
to it. A person, project or key of another organisation answers 404. The audit
log names the actor `keera-agents`.

Without `KEERA_AGENTS_SERVICE_KEY` the routes answer 501 with the code
`agents_disabled`.

## Keys issued for agents

A key Keera Agents issues is an ordinary standard key in the person's name. It
is in a project of their organisation, the oldest one unless Keera Agents names
another, and the organisation's and the project's guardrails and budgets apply.
It can be refused for a disabled person, for someone outside the organisation,
or on another organisation's project.

`budget_micros` puts a monthly budget on the key itself, so one agent cannot
spend everything the project allows. It is written
in the same transaction as the key, so the key never works without it.

Such a key is marked `issued_by: agents` in the API and **Keera Agents** in the
panel. Keera Agents holds the only copy of its secret, which changes two things:

- **It cannot be rotated.** The new secret would be shown to whoever rotated
  it, and the agent would stop working with no way to get it. A rotation
  answers 409 with the code `agents_key`.
- **It can be revoked**, by the person or an administrator, as any key. That is
  the kill switch for an agent that misbehaves. Keera Agents can revoke only the
  keys it issued, so the service key cannot take a person's own keys away.

The catalogue lists the enabled chat models and routers the organisation's and
the project's guardrails allow, and the enabled MCP servers they allow.

## When someone leaves

Disabling a person in Keera revokes every key in their name, including the ones
Keera Agents issued for them ([sso.md](sso.md#when-someone-leaves)). The gateway
also treats any key of a disabled person as revoked, even one issued while they
were being disabled. `GET /control/v1/service/users` then answers
`"disabled": true`, so Keera Agents can stop that person's agents and tell
whoever relied on them.

Removing a person from the directory only ends their sign-ins, as for every
other key: the directory never sees API keys. Their agents keep working until
they are disabled in Keera.
