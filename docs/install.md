# Installing Keera

Keera runs in four deployment shapes. All use the same binary and tenancy
model. They differ in what runs the process and what runs Postgres.

The short version is in [Self-host](https://docs.keera.ch/operate/). This page
is the full reference: every setting, how each part works, and why.

| Shape                   | What it is for                                       | Postgres                |
| ----------------------- | ---------------------------------------------------- | ----------------------- |
| [`compose`](../compose) | A single host or a laptop. Development and demos.    | A container             |
| `nixos`                 | A single host, declared. No container above the LLM. | Native, peer auth       |
| `kubernetes`            | A cluster serving a team. vLLM on a GPU.             | A StatefulSet, or yours |
| `infomaniak`            | The cluster the chart runs on, on Infomaniak KaaS.   | The chart's             |

Only `compose` is in this repository, because it is also the development
loop. This page covers what all four share: every setting, the first run, and
troubleshooting.

## Before you start

What you need is in
[What you need](https://docs.keera.ch/operate/install/#what-you-need). Beyond
that:

- The gateway applies its schema on start under an advisory lock, so several
  replicas can start at once and no migration job is needed. Its two
  notification listeners turn off `idle_session_timeout` for their own
  sessions, so a server-wide timeout does not end them.
- It refuses an operator key, a secret key or a `KEERA_METRICS_TOKEN` shorter
  than 16 characters, before it touches the database.

## Configuration

Every setting, with its default, is in
[Settings](https://docs.keera.ch/operate/settings/). These pages explain how
they work:

| Settings                                                       | Explained in                                                                                 |
| -------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `KEERA_OPERATOR_KEY`                                           | [sso.md](sso.md#signing-in-from-the-command-line), [The `keera` command](#the-keera-command) |
| `KEERA_SECRET_KEY`, `KEERA_SECRET_KEY_PREVIOUS`                | [Changing the secret key](#changing-the-secret-key)                                          |
| `KEERA_MODELS_FILE`                                            | [Template files](gateway.md#template-files)                                                  |
| `KEERA_USAGE_RETENTION`, `KEERA_AUDIT_RETENTION`               | [Retention and sizing](#retention-and-sizing)                                                |
| `KEERA_UPSTREAM_DENY`, `KEERA_UPSTREAM_PRIVATE`, `HTTPS_PROXY` | [Where the gateway may connect](#where-the-gateway-may-connect)                              |
| `KEERA_TRUSTED_PROXIES`                                        | [Publishing it](#publishing-it)                                                              |
| `KEERA_METRICS_TOKEN`                                          | [Metrics](gateway.md#metrics)                                                                |
| `KEERA_UPSTREAM_HEADER_TIMEOUT`                                | [Streaming](gateway.md#streaming), [billing.md](billing.md#what-is-billed)                   |
| `KEERA_CACHE_TTL`                                              | [Caching](gateway.md#caching)                                                                |
| `KEERA_REDIS_URL`, `KEERA_REDIS_PREFIX`, `KEERA_SPEND_REFRESH` | [Rate limits across replicas](gateway.md#rate-limits-across-replicas)                        |
| `KEERA_SESSION_GAP`                                            | [sessions.md](sessions.md#configuration)                                                     |
| `KEERA_OIDC_*`, `KEERA_OPERATORS`, `KEERA_PASSKEYS`            | [sso.md](sso.md)                                                                             |
| `KEERA_CHAT_URL`                                               | [sso.md](sso.md#signing-in-to-keera-chat)                                                    |
| `KEERA_AGENTS_URL`, `KEERA_AGENTS_SERVICE_KEY`                 | [agents.md](agents.md)                                                                       |
| `KEERA_PROVIDER_*`, `KEERA_BILLING_*`                          | [billing.md](billing.md)                                                                     |
| `KEERA_CLAUDE_SUBSCRIPTIONS`                                   | [subscriptions.md](subscriptions.md)                                                         |

What Settings leaves out:

- `KEERA_PUBLIC_URL` is also used for the sign-out redirect, by **Connect
  client**, and for the panel link in a refusal. Unset, or with
  `KEERA_UI=false`, a refusal has no link.
- `KEERA_MAX_RESPONSE_BYTES` also caps the text a stream translated to another
  API holds.
- Sizes are a plain number of bytes. On/off settings take `true`/`false`,
  `yes`/`no`, `on`/`off` or `1`/`0`. A size or count of zero or less is
  ignored, and so is a zero or negative `KEERA_CACHE_TTL`,
  `KEERA_SPEND_REFRESH` or `KEERA_UPSTREAM_HEADER_TIMEOUT`.

Some values stop the start, with a message that names the setting. Among
them: a `KEERA_LOG_LEVEL` or `KEERA_LOG_FORMAT` that Settings does not list, a
`KEERA_REDIS_URL` that is not a Redis URL, a `KEERA_UPSTREAM_DENY` or
`KEERA_TRUSTED_PROXIES` entry that is not an address or a prefix, a
`KEERA_METRICS_TOKEN` equal to the operator key, a `KEERA_AGENTS_SERVICE_KEY`
shorter than 32 characters or equal to another credential, a
subscription model in `KEERA_MODELS_FILE` while `KEERA_CLAUDE_SUBSCRIPTIONS` is
off, and a `KEERA_OIDC_<NAME>_DEFAULT_ROLE` other than `admin` or
`member`. The single sign-on, passkey and payment settings have checks of their
own, which their sections, [sso.md](sso.md) and
[billing.md](billing.md#set-it-up) describe.

### Where the gateway may connect

An organisation's administrators set where its models and MCP servers are, and
the gateway connects there for them. `KEERA_UPSTREAM_DENY` keeps it away from
the gateway's own host. It is a comma-separated list of addresses and prefixes.
The default is

```text
127.0.0.0/8,::1/128,169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128,fd00:ec2::254/128,100.100.100.200/32,168.63.129.16/32
```

That is loopback, link-local (where cloud metadata services answer), the
unspecified address, AWS's IPv6 metadata address, Alibaba Cloud's metadata
address and Azure's WireServer. Private ranges are allowed, because that is
where a self-hosted inference plane runs.

- **A backend on the same host**, such as vLLM on `127.0.0.1`: set the list
  without the two loopback entries. `make dev` does this. Loopback counts as
  inside the network, so with `KEERA_UPSTREAM_PRIVATE` set, name it there too,
  such as `127.0.0.1`.
- **Organisations you do not trust with your network**: add your private ranges,
  such as `10.0.0.0/8`. A model the gateway must still reach then needs an
  address outside them. An organisation that
  [signed itself up](sso.md#what-a-new-organisation-can-use) is kept from
  private ranges until it has paid, whatever this list says.
- `none` turns the check off.

`KEERA_UPSTREAM_PRIVATE` limits what organisations reach inside the network.
Set it to the host names they may use there, comma-separated. The models every
new organisation starts with need `keera-engine,keera-decider`. Every
other private address is then blocked for every organisation, even through a
public name that resolves into the network. `none` blocks the whole private
network, and `all` allows it, which is the default. With
[sign-up](sso.md#letting-people-sign-up) on, the gateway does not start until it
is set, because anyone who signs up runs an organisation. If you set
`HTTPS_PROXY` or `HTTP_PROXY` to a private address, name the proxy host too.

The check applies to the address a name resolves to, so a DNS name that points
at a blocked address is blocked too. The gateway reads `HTTPS_PROXY`,
`HTTP_PROXY` for plain-http backends, and `NO_PROXY`. Behind a proxy, the
gateway only connects to the proxy, and the proxy has to do this filtering. The gateway does
not follow redirects from a backend or an MCP server.

## Single host with compose

[compose/README.md](../compose/README.md) runs it, and says which settings
`compose.yaml` fixes and the few that only compose has, such as `KEERA_BIND`.

## The other shapes

The NixOS host, the Helm chart and the OpenTofu that creates the cluster are
not in this repository. They read the same settings as above. Only the image
they run is built here:

```sh
make image GATEWAY_IMAGE=<registry>/keera-gateway:v0.1.0 VERSION=v0.1.0
podman push <registry>/keera-gateway:v0.1.0
```

A `vendor/` older than `go.mod` breaks the build;
[run-locally.md](run-locally.md#building) has this and the other build steps.

`VERSION` is what the binary reports. Without it, the image says the tag when
the commit has one, and `devel` otherwise, followed by the short commit, and
`-dirty` if the tree had changes. The result is a `FROM scratch` image with one
static binary. The release workflow also publishes one for every `v*` tag, as
`ghcr.io/bespinian/keera-gateway:<tag>`.

## The `keera` command

Every release has the `keera` command for Linux, macOS and Windows on amd64 and
arm64, one `.tar.gz` per platform. Each archive also has the `LICENSE` and
`THIRD_PARTY_NOTICES` files. To verify a download:

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify keera_v0.1.0_linux_amd64.tar.gz --repo bespinian/keera-gateway \
  --signer-workflow bespinian/keera-gateway/.github/workflows/release-build.yml
```

`--signer-workflow` checks that the release workflow signed the file, not some
other workflow in the repository. The image is verified the same way, with
`oci://ghcr.io/bespinian/keera-gateway:<tag>` in place of the file.

To build it from source, see [run-locally.md](run-locally.md#building).

The settings it reads, and how a command picks the gateway and organisation,
are in [The keera CLI](https://docs.keera.ch/reference/cli/#settings-it-reads).
It also reads `KEERA_OPERATOR_KEY`, the operator key to act with. That wins
over a stored sign-in, since someone exported it on purpose. It is only sent to
a gateway named by `--url` or `KEERA_CONTROL_URL`, never to a remembered or
default one. `keera login` warns when it finds one.

You can be signed in to several gateways at once. If `keera logout` removes the default,
the one sign-in left becomes the default; with several left, there is none.
`keera whoami` shows which gateway and which credential are used, and why.

[`keera login`](sso.md#signing-in-from-the-command-line) keeps its credentials
in `keera/credentials.json` in the user's configuration directory (mode 0600),
one entry per gateway: `~/.config` on Linux, `~/Library/Application Support` on
macOS and `%AppData%` on Windows. With `KEERA_CONFIG_DIR` set, the file is
`$KEERA_CONFIG_DIR/credentials.json`.

### In a script, and on a screen

Output is coloured only when it goes to a terminal. The words say the same
without colour: a failing check reads `FAIL` whether or not it is red.

`--color always` or `CLICOLOR_FORCE=1` forces colour, for example on a CI runner
with a real console. `--color never`, `--no-color`, `NO_COLOR=1` or `TERM=dumb`
turns it off, and wins over `CLICOLOR_FORCE`. Listing commands also take
`--json`, which is never coloured.

## First run

The steps are in
[Self-host](https://docs.keera.ch/operate/install/#first-run). With several
organisations, run `keera org switch` before `keera project create`. The
operator key is for before an identity provider is set up. After that, people
sign in as themselves with `keera login`.

On an empty deployment, the panel's **Getting started** takes the place of
**Dashboard**. It stays until the first request has been served and, with
[card payments](billing.md#pay-in-advance), until the organisation has had
credit, paid or granted, or is billed by invoice. A member gets a short guide
of their own: get the key's secret, then connect a client. It stays until one
of their keys has made a request.

### `keera model check` is the acceptance test

A ready container only means the backend answered `/health`. It does not mean
the model returns OpenAI-shaped `tool_calls`. Without them, a coding agent
writes prose instead of edits. No error is logged; developers just think the
model is bad.

So `keera model check`
([Check a model](https://docs.keera.ch/manage/models/#check-a-model)) gives the
model a tool and a question only that tool can answer, and looks for the call
in `tool_calls`.

A vLLM started without `--enable-auto-tool-choice` refuses every request that
offers tools. The check then asks again without them and passes with a warning,
since plain chat still works. Coding agents still do not.

Run it after every change of model, quantisation or vLLM version. Each check is
written to the audit log, so you can show when a model last worked.

## Publishing it

What to put in front of the gateway:
[Publish it](https://docs.keera.ch/operate/install/#publish-it). That list
matters more than usual, because one port carries the control side and the
inference side, and only credentials keep them apart
([gateway.md](gateway.md#one-listener-three-surfaces)). Until single sign-on or
passkeys are set up, the only way in is the operator key, which can change
every guardrail, and every audit entry reads `operator@localhost`.

Check `KEERA_TRUSTED_PROXIES` if the proxy is not on loopback or a private
address. The sign-in throttle counts attempts per client, and the gateway takes
the client's address from `X-Forwarded-For` only when a proxy in this list sent
it. The default is loopback and the private ranges. Behind an untrusted proxy,
every client looks like the proxy and they share one allowance. `none` ignores
the header.

## Backups

What to back up is in
[Backups and upgrades](https://docs.keera.ch/operate/install/#backups-and-upgrades).

A Postgres in a single container or StatefulSet does not survive losing its
node. If you need a recovery objective, run Postgres the way the rest of the
cluster does (an operator, or a managed service), and point `KEERA_DATABASE_URL`
at it.

### Changing the secret key

The steps are in
[Required](https://docs.keera.ch/operate/settings/#required). Both keys read
what is stored while `KEERA_SECRET_KEY_PREVIOUS` is set.

If you change the key without setting the previous one, nothing stored can be
read: the API keys of hosted models and MCP servers have to be entered again, and
people who sign in through a directory are signed out at their next
[check](sso.md#asking-the-directory-again).

## Retention and sizing

Most tables grow with the number of customers. These grow with use:

| Table           | One row per                                      |
| --------------- | ------------------------------------------------ |
| `usage_events`  | inference request                                |
| `filter_runs`   | filter run, per request                          |
| `tool_calls`    | MCP tool call                                    |
| `usage_daily`   | organisation, key, model, client and day, in UTC |
| `api_keys`      | key                                              |
| `billing_lines` | billable call, on the deployment's keys          |
| `audit_log`     | administrative action, so it grows slowly        |

`usage_events` is the request log, and by far the largest table. A row is ids,
counts and a latency breakdown: about 400 bytes, and 350 more in indexes. A
filter run adds about 300 bytes for each filter on the request. No prompt or
completion text is stored. `usage_daily` holds its totals per day,
and is small: a row per key, model and client that was used that day.

How to set the windows is in
[Retention](https://docs.keera.ch/operate/settings/#retention). Both default to
for ever, on purpose. At start-up the gateway logs the windows, or
`no retention window is configured`. `0` turns a window off. Below `24h` is
refused, because deleted rows cannot be recovered, and a typo like `10m` would
delete the day's request log.

Past the usage window, the gateway deletes the request log, filter runs, tool
calls and closed budget windows. It also deletes keys revoked or expired, with
their guardrails. Billing rows and payments are never deleted
([billing.md](billing.md)).

**Which screens reach back how far:**

- **Usage totals and charts over more than two days** read `usage_daily`, so
  they reach back to the start, whatever `KEERA_USAGE_RETENTION` is. They count
  whole UTC days. So do the totals on the keys screen.
- **Everything else reads `usage_events`**, so it reaches back only as far as
  `KEERA_USAGE_RETENTION`. That is windows of two days or less, the request log,
  sessions, the map, first-token times, and the model, router, filter and tool
  call screens.

Retention runs hourly, deletes 5,000 rows at a time so it does not block the
replicas, and logs how many rows went. Postgres reuses the space of deleted
rows, but does not give it back to the disk. After shortening a window, run
`pg_repack` (online) or `VACUUM FULL` (locks the table) once to shrink the files.

To size the database, run a representative week, read
`SELECT pg_size_pretty(pg_total_relation_size('usage_events'));` and multiply
by the window. Rows follow tasks × calls per task, not the number of developers:
a coding agent makes thirty or forty calls per instruction.
[sessions.md](sessions.md) shows your ratio.

A gateway's memory grows with how many organisations, keys, models and the
like there are, and with the requests in flight. Per request it holds the body
(up to `KEERA_MAX_BODY_BYTES`), and a non-streamed answer whole; a rewrite
filter's answer is held whole up to 32 MiB. A client has two minutes to send
its body. Give the container a memory limit: the gateway reads it from cgroup
v2 and keeps its heap under nine tenths of it, collecting harder near the limit
rather than being killed. `GOMEMLIMIT` overrides that. Size Postgres for
replicas × (`KEERA_MAX_DB_CONNS` + 2) connections: the two listeners hold
their own.

Background jobs, such as retention and payments, run on one replica per
interval, whichever asks first.

## Upgrading

The gateway applies its schema on start, so an upgrade is a new image and a
restart. Also:

- **A changed template file does not reach existing organisations.** See
  [Template files](gateway.md#template-files) for how to apply it to one.
- **Run `keera model check` afterwards** if anything on the inference side
  changed.
- **Upgrade to v0.10.0 before you shorten `KEERA_USAGE_RETENTION`.** The
  upgrade fills `usage_daily` from the request log still there. It reads the
  whole table and rebuilds some of its indexes before the gateway serves, so on
  a large database try it on a copy first, and stop the old replicas before
  starting the new ones.
- **v0.10.0 renames `max_context` to `max_context_tokens`** in model files,
  in the API and in `keera model set --max-context-tokens`. A model file that still
  says `max_context` is refused on start, so rename it there first.
- **v0.11.0 replaces routers.** The old ones are deleted, and a client naming
  one is told the model does not exist until it is written again. See
  [Upgrading from v0.10](routers.md#upgrading-from-v010).
- **v0.11.0 changes guardrail limits.** `keera guardrail set` takes
  `--requests` and `--tokens`, with `--per minute|day|month`, instead of
  `--rpm` and `--tpm`. In the API, `rpm`, `tpm`, `budget_micros` and
  `budget_period` become the lists `rate_limits` and `budgets`. Existing limits
  are carried over. The output-token guardrail (`--max-output-tokens`,
  `max_output_tokens`) is removed, and its values are dropped. Update scripts
  and API clients that set these.
- **v0.13.0 removes sandboxes.** [Keera Agents](agents.md) replaces them. The
  upgrade revokes the keys of sandboxes still running and drops their tables
  and guardrail limits, along with `allowed_repos`. It does not stop the
  machines: remove leftover sandbox pods or containers yourself. The
  `KEERA_SANDBOX_*` and `KEERA_SANDBOXES_FILE` settings are ignored, and
  `keera sandbox` is gone.
- **v0.14.0 takes refusing away from rewrite filters.** One whose instruction
  still tells it to refuse now fails with `502` where it used to refuse with
  `403`; the request is stopped either way. Move that part of the instruction
  into a [gate](filters.md#for-a-gate) placed before it. `keera filter try`
  shows which prompts it tries to refuse.
- **A database created before v0.4.0 cannot be upgraded in place.** Its first
  schema file changed up to that release, and the gateway runs each file only
  once. Start from an empty database.

## When it does not work

Start with `keera doctor`, then the common problems:
[Troubleshooting](https://docs.keera.ch/operate/troubleshooting/).
`keera doctor --probe` runs `keera model check` against each enabled model. This
catches a backend that answers 200 with prose when a tool call was asked for.
It costs one generation per model, two when the backend refuses tools, so only
the organisation's administrators can run it.

Not on that page:

- **A filter refuses long conversations with 413.** `max_context_tokens` on
  that filter's model is too small for the traffic. A router's instruction step
  whose deciding model is too small cannot decide either: it uses its fallback
  or answers 503 `router_undecided`. See [filters.md](filters.md) and
  [routers.md](routers.md).
- **A rate limit admits more than it is set to, with `KEERA_REDIS_URL` set.**
  Check `keera_ratelimit_fallback_total`.
- **The panel shows no sign-on button.** See
  [sso.md](sso.md#when-it-does-not-work).
