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

**Postgres.** Any recent version. The gateway applies its schema on start
under an advisory lock, so several replicas can start at once.

**A GPU, to demo a coding agent.** The CPU tier runs the gateway fine but
cannot produce OpenAI-shaped `tool_calls`, so an agent writes prose instead of
edits. See [compose/README.md](../compose/README.md).

**Two generated credentials:**

```sh
openssl rand -hex 32      # KEERA_OPERATOR_KEY
openssl rand -hex 32      # KEERA_SECRET_KEY
```

The gateway does not start without both. It also refuses an operator key, a
secret key or a `KEERA_METRICS_TOKEN` shorter than 16 characters, before it
touches the database.

**Model weights.** The first start downloads them, and the inference container
stays unready for several minutes. Do not restart it.

## Configuration

Everything is an environment variable, except the models and sandbox classes
new organisations start with, which are files.

### Required

| Variable             | What it is                                                                                                                                                                                                                                                    |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `KEERA_DATABASE_URL` | Postgres connection string.                                                                                                                                                                                                                                   |
| `KEERA_OPERATOR_KEY` | The credential for the control API and the `keera` command, and the only one not stored in the database. Once an identity provider is set up, people use `keera login` and this key is for automation. See [sso.md](sso.md#signing-in-from-the-command-line). |
| `KEERA_SECRET_KEY`   | Encrypts the API keys of hosted models and MCP servers saved in the panel. Changing it later makes the stored keys unreadable. Generate one with `openssl rand -hex 32`.                                                                                      |

### Worth setting on any real deployment

| Variable                | Default     | What it is                                                                                                                                                                                            |
| ----------------------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `KEERA_METRICS_TOKEN`   | unset       | Reads `/metrics` and nothing else. Must differ from the operator key.                                                                                                                                 |
| `KEERA_PUBLIC_URL`      | Host header | The gateway's address as a browser sees it. Used for the sign-out redirect, by **Connect a client**, and for the panel link in a refusal. Unset, a refusal has no link. Required with single sign-on. |
| `KEERA_MODELS_FILE`     | unset       | The models each new organisation starts with. Existing organisations are not changed.                                                                                                                 |
| `KEERA_USAGE_RETENTION` | for ever    | How long usage events, ended sandboxes and ended keys are kept. See [sizing.md](sizing.md).                                                                                                           |
| `KEERA_AUDIT_RETENTION` | for ever    | How long audit entries are kept.                                                                                                                                                                      |
| `KEERA_CURRENCY`        | `CHF`       | The label on every money figure. Amounts are stored as integer micro-units.                                                                                                                           |

### Everything else

| Variable                        | Default             | What it is                                                                          |
| ------------------------------- | ------------------- | ----------------------------------------------------------------------------------- |
| `KEERA_ADDR`                    | `:8080`             | The one listener.                                                                   |
| `KEERA_UI`                      | `true`              | Serve the control panel at `/`.                                                     |
| `KEERA_MAX_DB_CONNS`            | `16`                | Postgres pool size.                                                                 |
| `KEERA_LOG_LEVEL`               | `info`              | `debug`, `info`, `warn` or `error`.                                                 |
| `KEERA_LOG_FORMAT`              | `text`              | `text` or `json`.                                                                   |
| `KEERA_MAX_BODY_BYTES`          | `33554432` (32 MiB) | Largest request body, in bytes. Coding agents send large contexts.                  |
| `KEERA_MAX_RESPONSE_BYTES`      | `67108864` (64 MiB) | Largest buffered (non-streamed) upstream response, in bytes.                        |
| `KEERA_UPSTREAM_HEADER_TIMEOUT` | `2m`                | How long a backend may take to _start_ answering. Does not limit the answer itself. |
| `KEERA_CACHE_TTL`               | `30s`               | How long a checked key is reused. Models, filters and the rest reload every minute. |
| `KEERA_REDIS_URL`               | unset               | Shares rate-limit buckets between replicas. Unset, they are per process. See below. |
| `KEERA_REDIS_PREFIX`            | `keera`             | Key prefix, so two deployments can share one Redis.                                 |
| `KEERA_SPEND_REFRESH`           | `10s`               | How stale a budget may be.                                                          |
| `KEERA_SESSION_GAP`             | `30m`               | Idle time that separates one task from the next. See [sessions.md](sessions.md).    |
| `KEERA_SECURE_COOKIES`          | follows the scheme  | Marks the session cookie `Secure`. On when `KEERA_PUBLIC_URL` is https.             |

Retention below `24h` is refused: deleted rows cannot be recovered, and a
typo like `10m` would delete the day's billing data. A session gap below `1m`
is refused too.

Sizes are a plain number of bytes. Durations take `h`, `m` and `s`. On/off
settings take `true`/`false`, `yes`/`no`, `on`/`off` or `1`/`0`. A number,
duration or on/off value that cannot be read, such as `64MB`, `365d` or
`enabled`, is ignored and the default is used. So is a size or count of zero or
less, and a zero or negative `KEERA_CACHE_TTL`, `KEERA_SPEND_REFRESH` or
`KEERA_UPSTREAM_HEADER_TIMEOUT`. For retention and `KEERA_SANDBOX_IDLE_SUSPEND`,
`0` means off and a negative value is refused.

These stop the start instead, with a message that names the setting: a
`KEERA_REDIS_URL` that is not a Redis URL, a `KEERA_SANDBOX_DRIVER` it does
not know, a `KEERA_SANDBOX_GIT_FORGE` it does not know when a sandbox driver is
set, and a `KEERA_OIDC_<N>_DEFAULT_ROLE` other than `admin` or `member`.

### Rate limits with more than one replica

The token buckets behind `rpm` and `tpm` live in each gateway process. With
two replicas, a limit of 600 per minute lets up to 1,200 through. Budgets are
not affected: they reconcile through Postgres and are the exact spending
control.

Set `KEERA_REDIS_URL` (`redis://host:6379/0`, or `rediss://` for TLS) to move
the buckets into Redis. The limit then holds however many replicas run. Only
the buckets are stored there: no guardrail, spend, session or prompt.

Use a single Redis endpoint, standalone or managed, not Redis Cluster. Each
request checks its organisation's, team's and key's buckets in one script, and
Redis Cluster refuses a script whose keys are in different slots. Redis holds
two small fields per scope that is currently sending traffic.

If Redis cannot be reached, the gateway keeps running. It logs the error,
counts it in `keera_ratelimit_fallback_total`, and falls back to its own
buckets. See [gateway.md](gateway.md).

### Single sign-on

`KEERA_OIDC_PROVIDERS` is a comma-separated list of provider names. Each name
`N` reads `KEERA_OIDC_<N>_ISSUER`, `KEERA_OIDC_<N>_CLIENT_ID`,
`KEERA_OIDC_<N>_CLIENT_SECRET`, `KEERA_OIDC_<N>_LABEL`,
`KEERA_OIDC_<N>_SCOPES`, `KEERA_OIDC_<N>_GROUPS_CLAIM`,
`KEERA_OIDC_<N>_ADMIN_GROUPS`, `KEERA_OIDC_<N>_OPERATOR_GROUPS`,
`KEERA_OIDC_<N>_DEFAULT_ROLE` (`admin` or `member`) and
`KEERA_OIDC_<N>_DOMAINS`, the email domains
that provider may sign people in with (`*` for any). Nothing is shared between
providers. With several, each needs its domains.

`KEERA_OPERATORS` lists the addresses that are operators, whichever provider
signs them in. SSO needs `KEERA_PUBLIC_URL`: the provider sends the browser back
to `<KEERA_PUBLIC_URL>/control/auth/callback`. See [sso.md](sso.md).

Discovery runs at start-up, so a wrong issuer URL stops the gateway with the URL
in the error, instead of breaking sign-in later. [sso.md](sso.md) is the
walkthrough.

### Sandboxes

Off unless `KEERA_SANDBOX_DRIVER` is `kubernetes` or `podman`. While it is
unset, the other `KEERA_SANDBOX_*` settings are ignored (a stray one does not
stop the start) and the panel hides sandboxes.

`KEERA_SANDBOXES_FILE` is read either way: it holds the classes new
organisations start with, so they are there once a driver is set. A missing or
invalid file stops the start.

The other settings are `KEERA_SANDBOX_*`: the namespace, the runtime per
isolation tier (a RuntimeClass on Kubernetes, an OCI runtime such as `runsc` on
podman), the address a sandbox reaches the gateway at, idle suspend, and the
forge sandboxes clone from. They are listed in
[sandboxes.md](sandboxes.md#configuration).

## Single host with compose

```sh
cd compose
cp .env.example .env         # then set KEERA_OPERATOR_KEY and KEERA_SECRET_KEY

# No GPU (llama.cpp; chat only, no tool calls):
podman compose up -d --build
podman compose logs -f keera-engine

# A GPU host (vLLM):
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml   # once, on the host
podman compose -f compose.yaml -f compose.gpu.yaml up -d --build
```

On a GPU host, pass the same `-f` flags to every later `podman compose` call.
Without them, compose acts on llama.cpp rather than the vLLM that is running.

`compose.yaml` passes the settings on this page from `.env` to the gateway,
with three exceptions:

- The database, the listener and the catalogue files are fixed in the file.
- Single sign-on has blocks only for providers named `google` and `entra`.
  Another name needs its own block.
- The sandbox settings are left out. The compose gateway cannot run sandboxes,
  so only `make dev` reads them. See [run-locally.md](run-locally.md).

The gateway is up at once. It applies its schema on start and waits only for
Postgres.

## The other shapes

The NixOS host, the Helm chart and the OpenTofu that creates the cluster are
not in this repository. They read the same settings as above. Only the image
they run is built here:

```sh
go mod vendor                                                     # once
make image GATEWAY_IMAGE=<registry>/keera-gateway:0.1.0 VERSION=0.1.0
podman push <registry>/keera-gateway:0.1.0
```

The vendor directory is not committed. Without it, the build writes its own,
which needs network access. With it, the build works offline. `VERSION` is what the binary reports; without it, the
image says `devel`. The
result is a `FROM scratch` image with one static binary. The release workflow
also publishes one for every `v*` tag, as
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

`make build` builds it from source. `make dist` builds the release archives
into `build/dist`.

The command reads these settings:

| Variable             | Default                             | What it is                                                                                             |
| -------------------- | ----------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `KEERA_CONTROL_URL`  | the gateway last signed in          | The gateway to talk to. `--url` wins over it. With neither and no sign-in, `https://gateway.keera.ch`. |
| `KEERA_OPERATOR_KEY` | unset                               | The operator key to act with. Wins over a stored sign-in.                                              |
| `KEERA_CONFIG_DIR`   | `keera` in the user's config folder | Where `credentials.json` is kept, for a shared account or a pipeline.                                  |

## First run

```sh
export KEERA_OPERATOR_KEY=…
export KEERA_CONTROL_URL=http://127.0.0.1:8080

keera org create "Example Bank"
keera team create "Payments Platform"     # --org only with several organisations
KEY=$(keera key create --team <team> --alias "a developer's laptop")
```

Organisation names are unique, ignoring case. An operator renames one with
`keera org set <org-id> --name "Example Bank AG"`, or **Edit** on the
organisations screen. Everything refers to it by id, so nothing else changes.

A key is printed once and never stored. Nobody, not even an operator, can read
it later.

`keera key rotate`, or **Rotate** on the keys screen, replaces a key in one
step. The new key has the same team, owner, lifetime and guardrails, and the
old key is revoked in the same step. Anyone who may revoke a key may rotate it,
so members can rotate their own:

```sh
KEY=$(keera key rotate "a developer's laptop")
```

The operator key is for before an identity provider is set up. After that,
people sign in as themselves with `keera login`.

Then check the deployment works, in this order:

```sh
keera model check keera-speed             # the acceptance test - see below
curl http://127.0.0.1:8080/api/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"keera-speed","messages":[{"role":"user","content":"Hello"}]}'
keera guardrail set team <team-id> --models keera-speed --rpm 120 --budget 500 --period month
keera usage --by team
keera doctor                              # and what is still missing
```

`keera limit` and `keera budget` each set one part of a guardrail. With no
flags, they print what is in force:

```sh
keera limit team <team-id> --rpm 120
keera budget team <team-id>               # every budget above this team
keera guardrail effective key <key-id>    # what a request with this key meets
```

Or open the panel at `http://127.0.0.1:8080` and sign in with the operator
key. On an empty deployment, **Overview** shows a first-run checklist.

### In a script, and on a screen

Output is coloured only when it goes to a terminal. The words say the same
without colour: a failing check reads `FAIL` whether or not it is red.

`--color always` or `CLICOLOR_FORCE=1` forces colour, for example on a CI runner
with a real console. `--color never`, `--no-color`, `NO_COLOR=1` or `TERM=dumb`
turns it off, and wins over `CLICOLOR_FORCE`. Listing commands
also take `--json`, which is never coloured.

### `keera model check` is the acceptance test

A ready container only means the backend answered `/health`. It does not mean
the model returns OpenAI-shaped `tool_calls`. Without them, a coding agent
writes prose instead of edits. No error is logged; developers just think the
model is bad.

`keera model check <alias>`, or **Check** on the panel's Models screen, gives
the model a tool and a question only that tool can answer. It reports whether
the call came back in `tool_calls`, whether the response streamed, and whether
the backend answered as the model the alias names.

Run it after every change of model, quantization or vLLM version. Each check is
written to the audit log, so you can show when a model last worked.

## Publishing it

Port 8080 carries the panel, the control API, the inference API and sandbox
attach. There is no second port to firewall; only the operator key and a
session cookie protect the control side. So:

- **Put a TLS terminator in front.** None of these deployments terminates TLS.
- **Turn off response buffering** on the proxy, and set a read timeout longer
  than the longest completion. Most ingress controllers buffer by default.
- **Allow large bodies.** The gateway accepts 32 MiB by default. A proxy with a
  lower limit rejects long conversations before they reach the gateway.
- **Choose which paths to publish.** To give developers the inference API
  without the panel, publish only `/api`.
- **Set up single sign-on** before anyone else can reach the panel. Without it,
  the only way in is the operator key, which can change every guardrail, and
  every audit entry reads `operator key`.

Set `KEERA_PUBLIC_URL` to the panel's public address. The sign-out redirect
uses it, and **Connect a client** gives developers that address with `/api`
appended.

## Backups

Postgres holds everything: tenancy, keys, guardrails, the usage log you bill
from and the audit log.

The Helm chart's Postgres is a single StatefulSet. It does not survive losing
its node. If you need a recovery objective, run Postgres the way the rest of
the cluster does (an operator, or a managed service) and set
`db.enabled=false`.

See [sizing.md](sizing.md) for what grows and how fast.

## Upgrading

The gateway applies its schema on start, so an upgrade is a new image and a
restart. Also:

- **The catalogue files only seed new organisations.** A change to one does not
  reach an organisation that already exists. Use
  `keera model apply <file> --org <id>` or
  `keera sandbox apply <file> --org <id>` for that.
- **Run `keera model check` afterwards** if anything on the inference side
  changed.

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
one generation per model, so only the organisation's administrators can run it.

A failing check exits non-zero, so `keera doctor` can be the last step of an
install script.

| Symptom                                               | Where to look                                                                                                                                                                                                                                  |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Gateway exits at once with a message about a variable | A required setting is missing or too short. The message names it.                                                                                                                                                                              |
| Every request comes back 404 from the backend         | `backend_model` does not match what the backend serves the model as - vLLM's `--served-model-name`, llama.cpp's `--alias`.                                                                                                                     |
| Long prompts fail with a 400 from the backend         | `max_context` on the model does not match the backend's window. They have to move together.                                                                                                                                                    |
| A filter refuses long conversations with 413          | `max_context` on that filter's model is too small for the traffic. A router whose model is too small cannot decide instead: it uses its fallback or answers 503 `router_undecided`. See [filters.md](filters.md) and [routers.md](routers.md). |
| The agent answers in prose and never edits a file     | Tool calls come back as text. Run `keera model check`. On the CPU tier this is expected.                                                                                                                                                       |
| The panel shows no sign-on button                     | Both an issuer and a client ID are needed under each name in `KEERA_OIDC_PROVIDERS`. Naming any provider also needs `KEERA_PUBLIC_URL`, or the gateway does not start. See [sso.md](sso.md).                                                   |
| A rate limit admits more than it is set to            | The buckets are per replica unless `KEERA_REDIS_URL` is set. If it is set, check `keera_ratelimit_fallback_total`.                                                                                                                             |
| Streaming arrives all at once                         | Something in front is buffering the response.                                                                                                                                                                                                  |
| A developer says their agent stopped this afternoon   | The **Requests** screen filtered to their key, or their **My access** screen. Each refusal is logged with the message the client got.                                                                                                          |
| A limit is set and does not seem to apply             | `keera guardrail effective <scope> <id>`. Guardrails nest and the tightest level wins. The command shows which level each value came from.                                                                                                     |
| `keera` acts on a gateway you did not mean            | No deployment was set, so it used the default, `https://gateway.keera.ch`. Run `keera login --url <your deployment>` or set `KEERA_CONTROL_URL`. `keera whoami` shows why.                                                                     |
