# Installing Keera

Keera runs in four deployment shapes. All use the same binary and tenancy
model. They differ in what runs the process and what runs Postgres.

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

**Postgres.** Version 14 or later. The gateway applies its schema on start
under an advisory lock, so several replicas can start at once and no migration
job is needed. Its two notification listeners turn off `idle_session_timeout`
for their own sessions, so a server-wide timeout does not end them.

**Two generated credentials:**

```sh
openssl rand -hex 32      # KEERA_OPERATOR_KEY
openssl rand -hex 32      # KEERA_SECRET_KEY
```

The gateway does not start without both. It also refuses an operator key, a
secret key or a `KEERA_METRICS_TOKEN` shorter than 16 characters, before it
touches the database.

## Configuration

Everything is an environment variable, except a few files: the models and
sandbox classes new organisations start with, and the
[Git credentials for sandboxes](sandboxes.md#configuration).

### Required

| Variable             | What it is                                                                                                                                                                                                                                                             |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `KEERA_DATABASE_URL` | Postgres connection string.                                                                                                                                                                                                                                            |
| `KEERA_OPERATOR_KEY` | The credential for the control API and the `keera` command, and the only one not stored in the database. Once an identity provider is set up, people use `keera login` and this key is for automation. See [sso.md](sso.md#signing-in-from-the-command-line).          |
| `KEERA_SECRET_KEY`   | Encrypts the API keys of hosted models and MCP servers saved in the panel, and the sign-in refresh tokens Keera keeps to ask the directory again ([sso.md](sso.md#asking-the-directory-again)). To change it, see [Changing the secret key](#changing-the-secret-key). |

### Worth setting on any real deployment

| Variable                 | Default     | What it is                                                                                                                                                                                                                                       |
| ------------------------ | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `KEERA_METRICS_TOKEN`    | unset       | Reads `/metrics` and nothing else. Must differ from the operator key.                                                                                                                                                                            |
| `KEERA_PUBLIC_URL`       | Host header | The gateway's address as a browser sees it. Used for the sign-out redirect, by **My clients**, and for the panel link in a refusal. Unset, or with `KEERA_UI=false`, a refusal has no link. Required with single sign-on, passkeys and payments. |
| `KEERA_MODELS_FILE`      | unset       | The models each new organisation starts with. See [Template files](gateway.md#template-files).                                                                                                                                                   |
| `KEERA_USAGE_RETENTION`  | for ever    | How long the request log, ended sandboxes and ended keys are kept. See [Retention and sizing](#retention-and-sizing).                                                                                                                            |
| `KEERA_AUDIT_RETENTION`  | for ever    | How long audit entries are kept.                                                                                                                                                                                                                 |
| `KEERA_UPSTREAM_DENY`    | see below   | Addresses the gateway never connects to. See [Where the gateway may connect](#where-the-gateway-may-connect).                                                                                                                                    |
| `KEERA_UPSTREAM_PRIVATE` | `all`       | Host names inside the network that organisations may reach. Required with sign-up. See [Where the gateway may connect](#where-the-gateway-may-connect).                                                                                          |

### Everything else

| Variable                        | Default             | What it is                                                                                                                                  |
| ------------------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `KEERA_ADDR`                    | `:8080`             | The one listener.                                                                                                                           |
| `KEERA_UI`                      | `true`              | Serve the control panel at `/`.                                                                                                             |
| `KEERA_MAX_DB_CONNS`            | `16`                | Postgres pool size per replica. At least `4`.                                                                                               |
| `KEERA_LOG_LEVEL`               | `info`              | `debug`, `info`, `warn` or `error`.                                                                                                         |
| `KEERA_LOG_FORMAT`              | `text`              | `text` or `json`.                                                                                                                           |
| `KEERA_MAX_BODY_BYTES`          | `33554432` (32 MiB) | Largest request body, in bytes. Coding agents send large contexts.                                                                          |
| `KEERA_MAX_RESPONSE_BYTES`      | `67108864` (64 MiB) | Largest buffered upstream response, and largest streamed event, in bytes.                                                                   |
| `KEERA_UPSTREAM_HEADER_TIMEOUT` | `2m`                | How long a backend may take to _start_ answering. Longer on [the deployment's own keys](billing.md#what-is-billed).                         |
| `KEERA_CACHE_TTL`               | `30s`               | How long a checked key is reused. Models, filters and the rest reload every minute.                                                         |
| `KEERA_REDIS_URL`               | unset               | Shares rate-limit buckets between replicas. Unset, they are per process. See below.                                                         |
| `KEERA_REDIS_PREFIX`            | `keera`             | Key prefix, so two deployments can share one Redis.                                                                                         |
| `KEERA_SPEND_REFRESH`           | `10s`               | How stale a budget may be.                                                                                                                  |
| `KEERA_SESSION_GAP`             | `30m`               | Idle time between two tasks; at least `1m`. See [sessions.md](sessions.md).                                                                 |
| `KEERA_SECURE_COOKIES`          | follows the scheme  | Marks the session cookie `Secure`. On when `KEERA_PUBLIC_URL` is https.                                                                     |
| `KEERA_CLAUDE_SUBSCRIPTIONS`    | `false`             | Lets Claude Code on a Claude plan through. See [subscriptions.md](subscriptions.md).                                                        |
| `KEERA_CHAT_URL`                | unset               | Keera Chat's address, without a path, so people can sign in to it and the panel links to it. See [sso.md](sso.md#signing-in-to-keera-chat). |
| `KEERA_TRUSTED_PROXIES`         | private ranges      | The proxies in front, whose `X-Forwarded-For` names the client. See [Publishing it](#publishing-it).                                        |

Retention below `24h` is refused: deleted rows cannot be recovered, and a
typo like `10m` would delete the day's request log.

Sizes are a plain number of bytes. Durations take `h`, `m` and `s`. On/off
settings take `true`/`false`, `yes`/`no`, `on`/`off` or `1`/`0`. Lists, such as
addresses, groups, domains and scopes, are comma-separated. A number,
duration or on/off value that cannot be read, such as `64MB`, `365d` or
`enabled`, is ignored and the default is used. So is a size or count of zero or
less, and a zero or negative `KEERA_CACHE_TTL`, `KEERA_SPEND_REFRESH` or
`KEERA_UPSTREAM_HEADER_TIMEOUT`. For retention and `KEERA_SANDBOX_IDLE_SUSPEND`,
`0` means off and a negative value is refused. The sandbox setting is only
checked while a driver is set ([Sandboxes](#sandboxes)).

Some values stop the start instead, with a message that names the setting.
Among them: a `KEERA_LOG_LEVEL` or `KEERA_LOG_FORMAT` not listed above, a
`KEERA_REDIS_URL` that is not a Redis URL, a `KEERA_UPSTREAM_DENY` or
`KEERA_TRUSTED_PROXIES` entry that is not an address or a prefix, a
`KEERA_METRICS_TOKEN` equal to the operator key, a
subscription model in `KEERA_MODELS_FILE` while `KEERA_CLAUDE_SUBSCRIPTIONS` is
off, and a `KEERA_OIDC_<NAME>_DEFAULT_ROLE` other than `admin` or
`member`. The single sign-on, passkey, sandbox and payment settings have checks
of their own, which their sections, [sso.md](sso.md),
[sandboxes.md](sandboxes.md#configuration) and
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

### Provider keys of the deployment's own

`KEERA_PROVIDER_<NAME>_API_KEY` gives organisations the deployment's own key for
that provider, billed at list price. A provider that serves each customer at
their own address, such as Infomaniak, also needs
`KEERA_PROVIDER_<NAME>_PRODUCT_ID`. See [billing.md](billing.md#set-a-provider-key).

`KEERA_BILLING_POSTFINANCE_*` lets organisations pay in advance, and
`KEERA_BILLING_VAT` adds VAT to each payment. See
[billing.md](billing.md#set-it-up).

### Rate limits with more than one replica

Rate limits are kept per replica unless `KEERA_REDIS_URL` is set. See
[Rate limits across replicas](gateway.md#rate-limits-across-replicas).

### Single sign-on

`KEERA_OIDC_PROVIDERS` names the identity providers, and each reads its own
`KEERA_OIDC_<NAME>_*`. An `ldap://` or `ldaps://` issuer makes one an
[LDAP directory](sso.md#ldap-and-active-directory). `KEERA_PASSKEYS` turns on
passkeys. `KEERA_OPERATORS`
names the email addresses that are operators
([sso.md](sso.md#the-operator-role-comes-from-the-environment-always)). Every
setting is in [sso.md](sso.md#settings).

### Sandboxes

Off unless `KEERA_SANDBOX_DRIVER` is `kubernetes` or `podman`; the podman
driver is for local development and tests. Every `KEERA_SANDBOX_*` setting, and
which are read without a driver, is in
[sandboxes.md](sandboxes.md#configuration).

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

The command reads these settings:

| Variable             | Default                             | What it is                                                                                                                             |
| -------------------- | ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `KEERA_CONTROL_URL`  | the gateway last signed in to       | The gateway to talk to. `--url` wins over it.                                                                                          |
| `KEERA_OPERATOR_KEY` | unset                               | The operator key to act with. Wins over a stored sign-in, since someone exported it on purpose. `keera login` warns when it finds one. |
| `KEERA_CONFIG_DIR`   | `keera` in the user's config folder | Where `credentials.json` is kept, for a shared account or a pipeline.                                                                  |
| `KEERA_ORG`          | the one `keera org switch` chose    | The organisation commands act on. Wins over `keera org switch`. See [Several organisations](gateway.md#several-organisations).         |
| `CLAUDE_CONFIG_DIR`  | `~/.claude`                         | Where `keera connect claude-code` writes Claude Code's settings, as Claude Code itself reads it.                                       |

A command picks the gateway in this order: `--url`, then `KEERA_CONTROL_URL`,
then the gateway last signed in to, then `https://gateway.keera.ch`. You can be
signed in to several gateways at once. If `keera logout` removes the default,
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

```sh
export KEERA_OPERATOR_KEY=…
export KEERA_CONTROL_URL=http://127.0.0.1:8080

keera org create "Example Bank"
keera project create "Payments Platform"     # with several: keera org switch first
KEY=$(keera key create --project <project> --name "a developer's laptop")
```

Who owns a key, how long it lasts, and how to rotate one or leave it for
someone to activate: [Keys and projects](gateway.md#keys-and-projects). The
operator key is for before an identity provider is set up. After that, people
sign in as themselves with `keera login`.

Then check the deployment works, in this order:

```sh
keera model check keera-speed             # the acceptance test - see below
curl http://127.0.0.1:8080/api/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"keera-speed","messages":[{"role":"user","content":"Hello"}]}'
keera guardrail set project <project-id> --models keera-speed --requests 120 --budget 500 --period month
keera usage --by project
keera doctor                              # and what is still missing
```

`keera limit` and `keera budget` each set one part of a guardrail. With no
flags, they print what is in force:

```sh
keera limit project <project-id> --requests 120
keera limit key <key-id> --tokens 2000000 --per day
keera budget project <project-id>               # every budget above this project
keera guardrail effective key <key-id>    # what a request with this key meets
```

Or open the panel at `http://127.0.0.1:8080` and sign in with the operator
key. On an empty deployment, **Getting started** takes the place of
**Dashboard** and shows a first-run checklist. It stays until the first request
has been served and, with [card payments](billing.md#pay-in-advance), until the
organisation has had credit, paid or granted, or is billed by invoice. A member
gets a short guide of their own: get the key's secret, then connect a client.
It stays until one of their keys has made a request.

### `keera model check` is the acceptance test

A ready container only means the backend answered `/health`. It does not mean
the model returns OpenAI-shaped `tool_calls`. Without them, a coding agent
writes prose instead of edits. No error is logged; developers just think the
model is bad.

`keera model check <alias>`, or **Check** on the panel's Models screen, gives
the model a tool and a question only that tool can answer. It reports whether
the call came back in `tool_calls`, whether the response streamed, and whether
the backend answered as the model the alias names.

A vLLM started without `--enable-auto-tool-choice` refuses every request that
offers tools. The check then asks again without them and passes with a warning,
since plain chat still works. Coding agents still do not.

Run it after every change of model, quantisation or vLLM version. Each check is
written to the audit log, so you can show when a model last worked.

## Publishing it

One port carries the control side and the inference side, and only credentials
keep them apart ([gateway.md](gateway.md#one-listener-four-surfaces)). So:

- **Put a TLS terminator in front.** None of these deployments terminates TLS.
- **Turn off response buffering** on the proxy, and set a read timeout longer
  than the longest completion. Most ingress controllers buffer by default.
- **Allow large bodies.** A proxy with a lower limit than
  `KEERA_MAX_BODY_BYTES` rejects long conversations before they reach the
  gateway.
- **Choose which paths to publish.** To give developers the inference API
  without the panel, publish only `/api`.
- **Set up single sign-on or passkeys** before anyone else can reach the panel.
  Without either, the only way in is the operator key, which can change every
  guardrail, and every audit entry reads `operator@localhost`.
- **Set `KEERA_PUBLIC_URL`** to the public address.
- **Check `KEERA_TRUSTED_PROXIES`** if the proxy is not on loopback or a
  private address. The sign-in throttle counts attempts per client, and the
  gateway takes the client's address from `X-Forwarded-For` only when a proxy
  in this list sent it. The default is loopback and the private ranges. Behind
  an untrusted proxy, every client looks like the proxy and they share one
  allowance. `none` ignores the header.

## Backups

Postgres holds everything: tenancy, keys, guardrails, the request log you bill
from and the audit log.

A Postgres in a single container or StatefulSet does not survive losing its
node. If you need a recovery objective, run Postgres the way the rest of the
cluster does (an operator, or a managed service), and point `KEERA_DATABASE_URL`
at it.

### Changing the secret key

Set the new key as `KEERA_SECRET_KEY` and the old one as
`KEERA_SECRET_KEY_PREVIOUS`, then restart. Both keys read what is stored, and at
start the gateway encrypts it all again with the new key. Once every replica runs
with the new key, restart once more, then remove `KEERA_SECRET_KEY_PREVIOUS`.

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
| `sandboxes`     | sandbox lent out                                 |
| `api_keys`      | key, and one more for each sandbox               |
| `billing_lines` | billable call, on the deployment's keys          |
| `audit_log`     | administrative action, so it grows slowly        |

`usage_events` is the request log, and by far the largest table. A row is ids,
counts and a latency breakdown: about 500 bytes, and as much again in indexes.
No prompt or completion text is stored. `usage_daily` holds its totals per day,
and is small: a row per key, model and client that was used that day.

```sh
KEERA_USAGE_RETENTION=720h           # 30 days of the request log
KEERA_AUDIT_RETENTION=17520h         # two years
```

**Both default to for ever, on purpose.** Reports read usage and compliance
reads the audit log, so the gateway deletes neither unless told to. At start-up
it logs the windows, or `no retention window is configured`. The daily totals
are never deleted: they are small, and long reports read them.

Past the usage window, the gateway deletes the request log, filter runs, tool
calls and closed budget windows. It also deletes sandboxes that ended, and keys
revoked or expired, with their guardrails. A key that a sandbox still names
stays. Billing rows and payments are never deleted ([billing.md](billing.md)).

**Which screens reach back how far:**

- **Usage totals and charts over more than two days** read `usage_daily`, so
  they reach back to the start, whatever `KEERA_USAGE_RETENTION` is. They count
  whole UTC days. So do the totals on the keys screen and **My clients**.
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

Background jobs, such as the sandbox sweep, retention and payments, run on one
replica per interval, whichever asks first.

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
- **A database created before v0.4.0 cannot be upgraded in place.** Its first
  schema file changed up to that release, and the gateway runs each file only
  once. Start from an empty database.

## When it does not work

Start with `keera doctor`. It reads the deployment's configuration and an
organisation's models and reports what is set, what is missing and what is declared but not
enforced, each with a fix. It changes nothing, so it is safe on a live
deployment.

```sh
keera doctor            # configuration, models, tenancy, filters, routers
keera doctor --probe    # and put a real request through every enabled model
```

`--probe` runs `keera model check` against each enabled model. This catches a
backend that answers 200 with prose when a tool call was asked for. It costs
one generation per model, two when the backend refuses tools, so only the
organisation's administrators can run it.

A failing check exits non-zero, so `keera doctor` can be the last step of an
install script.

| Symptom                                               | Where to look                                                                                                                                                                                                                                                                    |
| ----------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Gateway exits at once with a message about a variable | A required setting is missing or too short. The message names it.                                                                                                                                                                                                                |
| Every request comes back 404 from the backend         | `backend_model` does not match what the backend serves the model as - vLLM's `--served-model-name`, llama.cpp's `--alias`.                                                                                                                                                       |
| Long prompts fail with a 400 from the backend         | `max_context_tokens` on the model does not match the backend's window. They have to move together.                                                                                                                                                                               |
| A filter refuses long conversations with 413          | `max_context_tokens` on that filter's model is too small for the traffic. A router's instruction step whose deciding model is too small cannot decide either: it uses its fallback or answers 503 `router_undecided`. See [filters.md](filters.md) and [routers.md](routers.md). |
| The agent answers in prose and never edits a file     | Tool calls come back as text. Run `keera model check`. On the CPU tier this is expected.                                                                                                                                                                                         |
| The panel shows no sign-on button                     | See [sso.md](sso.md#when-it-does-not-work).                                                                                                                                                                                                                                      |
| A rate limit admits more than it is set to            | The buckets are per replica unless `KEERA_REDIS_URL` is set. If it is set, check `keera_ratelimit_fallback_total`.                                                                                                                                                               |
| Streaming arrives all at once                         | Something in front is buffering the response.                                                                                                                                                                                                                                    |
| A developer says their agent stopped this afternoon   | The **Requests** screen filtered to their key, or their **My access** screen. Each refusal is logged with the message the client got.                                                                                                                                            |
| A limit is set and does not seem to apply             | `keera guardrail effective <scope> <id>`. Guardrails nest and the tightest level wins. The command shows which level each value came from.                                                                                                                                       |
| `keera` acts on a gateway you did not mean            | No deployment was set, so it used the default, `https://gateway.keera.ch`. Run `keera login --url <your deployment>` or set `KEERA_CONTROL_URL`. `keera whoami` shows why.                                                                                                       |
