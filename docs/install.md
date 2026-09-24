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

The gateway does not start without an operator key, or with one shorter than
16 characters.

**Model weights.** The first start downloads them, and the inference container
stays unready for several minutes. Do not restart it.

## Configuration

Everything is an environment variable, except the model catalogue, which can
also be a file.

### Required

| Variable             | What it is                                                                                                                                                                                                                                                    |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `KEERA_DATABASE_URL` | Postgres connection string.                                                                                                                                                                                                                                   |
| `KEERA_OPERATOR_KEY` | The credential for the control API and the `keera` command, and the only one not stored in the database. Once an identity provider is set up, people use `keera login` and this key is for automation. See [sso.md](sso.md#signing-in-from-the-command-line). |

### Worth setting on any real deployment

| Variable                | Default     | What it is                                                                                              |
| ----------------------- | ----------- | ------------------------------------------------------------------------------------------------------- |
| `KEERA_SECRET_KEY`      | unset       | Encrypts hosted-model API keys saved in the panel. Without it, that field is disabled.                  |
| `KEERA_METRICS_TOKEN`   | unset       | Reads `/metrics` and nothing else. Must differ from the operator key.                                   |
| `KEERA_PUBLIC_URL`      | Host header | The gateway's address as a browser sees it. Used for the sign-out redirect and by **Connect a client**. |
| `KEERA_MODELS_FILE`     | unset       | The catalogue of models and MCP servers. Applied on every start; idempotent.                            |
| `KEERA_USAGE_RETENTION` | for ever    | How long usage events are kept. See [sizing.md](sizing.md).                                             |
| `KEERA_AUDIT_RETENTION` | for ever    | How long audit entries are kept.                                                                        |
| `KEERA_CURRENCY`        | `CHF`       | The label on every money figure. Amounts are stored as integer micro-units.                             |

### Everything else

| Variable                        | Default            | What it is                                                                           |
| ------------------------------- | ------------------ | ------------------------------------------------------------------------------------ |
| `KEERA_ADDR`                    | `:8080`            | The one listener.                                                                    |
| `KEERA_UI`                      | `true`             | Serve the control panel at `/`.                                                      |
| `KEERA_MAX_DB_CONNS`            | `16`               | Postgres pool size.                                                                  |
| `KEERA_LOG_LEVEL`               | `info`             | `debug`, `info`, `warn` or `error`.                                                  |
| `KEERA_LOG_FORMAT`              | `text`             | `text` or `json`.                                                                    |
| `KEERA_MAX_BODY_BYTES`          | 32 MB              | Largest request body. Coding agents send large contexts.                             |
| `KEERA_MAX_RESPONSE_BYTES`      | 64 MB              | Largest buffered (non-streamed) upstream response.                                   |
| `KEERA_UPSTREAM_HEADER_TIMEOUT` | `2m`               | How long a backend may take to _start_ answering. Does not limit the answer itself.  |
| `KEERA_CACHE_TTL`               | `30s`              | How stale the gateway's view of the control plane may be.                            |
| `KEERA_REDIS_URL`               | unset              | Shares rate-limit buckets between replicas. Unset, they are per process. See below.  |
| `KEERA_REDIS_PREFIX`            | `keera`            | Key prefix, so two deployments can share one Redis.                                  |
| `KEERA_SPEND_REFRESH`           | `10s`              | How stale a budget may be.                                                           |
| `KEERA_SESSION_GAP`             | `30m`              | Idle time that separates one task from the next. See [sessions.md](sessions.md).     |
| `KEERA_SECURE_COOKIES`          | follows the scheme | Marks the session cookie `Secure`. On when the OIDC redirect or public URL is https. |

Retention below `24h` is refused: deleted rows cannot be recovered, and a
typo like `10m` would delete the day's billing data. A session gap below `1m`
is refused too.

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

`KEERA_OIDC_ISSUER`, `KEERA_OIDC_CLIENT_ID`, `KEERA_OIDC_CLIENT_SECRET`,
`KEERA_OIDC_REDIRECT_URL`, `KEERA_OIDC_SCOPES`, `KEERA_OIDC_GROUPS_CLAIM`,
`KEERA_OIDC_ADMIN_GROUPS`, `KEERA_OIDC_OPERATOR_GROUPS`, `KEERA_OPERATORS`,
`KEERA_OIDC_DEFAULT_ROLE`.

These configure one identity provider, which is what a dedicated or
on-premises deployment has. For several, set `KEERA_OIDC_PROVIDERS` to a
comma-separated list of names. Each name `N` takes `KEERA_OIDC_<N>_ISSUER`,
`KEERA_OIDC_<N>_CLIENT_ID`, `KEERA_OIDC_<N>_CLIENT_SECRET` and, to override
anything above, `KEERA_OIDC_<N>_LABEL`, `KEERA_OIDC_<N>_GROUPS_CLAIM`,
`KEERA_OIDC_<N>_ADMIN_GROUPS`, `KEERA_OIDC_<N>_OPERATOR_GROUPS`,
`KEERA_OIDC_<N>_DEFAULT_ROLE`, `KEERA_OIDC_<N>_SCOPES` and
`KEERA_OIDC_<N>_REDIRECT_URL`. `KEERA_OIDC_ADOPT_BY_EMAIL` is off by default and
is only for a one-time migration.

Discovery runs at start-up, so a wrong issuer URL stops the gateway with the URL
in the error, instead of breaking sign-in later. [sso.md](sso.md) is the
walkthrough.

### Sandboxes

Off unless `KEERA_SANDBOX_DRIVER` is `kubernetes` or `podman`. While it is
unset, all other sandbox settings are ignored (a stray one does not stop the
start) and the panel hides sandboxes.

The other settings are `KEERA_SANDBOX_*`: the catalogue file, the namespace, the
RuntimeClass per isolation tier, the address a sandbox reaches the gateway at,
idle suspend, and the forge sandboxes clone from. They are listed in
[sandboxes.md](sandboxes.md#configuration).

## Single host with compose

```sh
cd compose
cp .env.example .env         # then set KEERA_OPERATOR_KEY and KEERA_SECRET_KEY

# No GPU (llama.cpp; chat only, no tool calls):
podman compose -f compose.yaml -f compose.cpu.yaml up -d --build

# A GPU host (vLLM):
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml   # once, on the host
podman compose -f compose.yaml -f compose.gpu.yaml up -d --build

podman compose -f compose.yaml -f compose.cpu.yaml logs -f keera-engine
```

Pass the same `-f` flags to every later `podman compose` call. Without them,
compose reads only the base file and acts on a different `keera-engine` than
the one running.

The gateway is up at once. It applies its schema and catalogue on start and
waits only for Postgres.

## The other shapes

The NixOS host, the Helm chart and the OpenTofu that creates the cluster are
not in this repository. They read the same settings as above. Only the image
they run is built here:

```sh
go mod vendor                                                       # once
podman build -t <registry>/keera-gateway:0.1.0 -f Containerfile .
podman push <registry>/keera-gateway:0.1.0
```

The vendor directory is not committed. With it, the build works offline. The
result is a `FROM scratch` image with one static binary. No image is published
yet: the release workflow builds one on a `v*` tag, and no tag exists yet.

## The `keera` command

Every release has the `keera` command for Linux, macOS and Windows on amd64 and
arm64, one `.tar.gz` per platform. Each archive also has the `LICENSE` and
`THIRD_PARTY_NOTICES` files. To verify a download:

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify keera_v0.1.0_linux_amd64.tar.gz --repo bespinian/keera-gateway
```

`make build` builds it from source. `make dist` builds the release archives
into `build/dist`.

## First run

```sh
export KEERA_OPERATOR_KEY=…
export KEERA_CONTROL_URL=http://127.0.0.1:8080

keera org create "Example Bank"
keera team create "Payments Platform"     # --org is inferred when there is one
KEY=$(keera key create --team <team-id> --alias "a developer's laptop")
```

A key is printed once and never stored. Nobody, not even an operator, can read
it later.

`keera key rotate` replaces a key in one step. The new key has the same team,
owner, lifetime and guardrails. The old key is revoked only after the new one
exists:

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

`--color always` forces colour, for example on a CI runner with a real console.
`--color never`, `NO_COLOR=1` or `TERM=dumb` turns it off. Listing commands
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

Port 8080 carries the panel, the control API and the inference API. There is
no second port to firewall; only the operator key and a session cookie protect
the control side. So:

- **Put a TLS terminator in front.** None of these deployments terminates TLS.
- **Turn off response buffering** on the proxy, and set a read timeout longer
  than the longest completion. Most ingress controllers buffer by default.
- **Allow large bodies.** The gateway accepts 32 MB. A proxy with a lower limit
  rejects long conversations before they reach the gateway.
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

- **The catalogue file is applied on every start.** The control API refuses
  changes to a model the file declares. A change to the file takes effect at
  the next start or the next `keera model apply`.
- **Run `keera model check` afterwards** if anything on the inference side
  changed.

## When it does not work

Start with `keera doctor`. It reads the deployment's configuration and
catalogue and reports what is set, what is missing and what is declared but not
enforced, each with a fix. It changes nothing, so it is safe on a live
deployment.

```sh
keera doctor            # configuration, catalogue, tenancy, filters, routers
keera doctor --probe    # and put a real request through every enabled model
```

`--probe` runs `keera model check` against each enabled model. This catches a
backend that answers 200 with prose when a tool call was asked for. It costs
one generation per model, and only an operator can run it.

A failing check exits non-zero, so `keera doctor` can be the last step of an
install script.

| Symptom                                                | Where to look                                                                                                                                                              |
| ------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Gateway exits at once with a message about a variable  | A required setting is missing or too short. The message names it.                                                                                                          |
| Every request comes back 404 from the backend          | `backend_model` does not match what the backend serves the model as - vLLM's `--served-model-name`, llama.cpp's `--alias`.                                                 |
| Long prompts fail with a 400 from the backend          | `max_context` on the model does not match the backend's window. They have to move together.                                                                                |
| A filter or router refuses long conversations with 413 | `max_context` on that filter's or router's model is too small for the traffic. See [filters.md](filters.md).                                                               |
| The agent answers in prose and never edits a file      | Tool calls come back as text. Run `keera model check`. On the CPU tier this is expected.                                                                                   |
| The panel shows no sign-on button                      | Both an issuer and a client ID are needed - `KEERA_OIDC_ISSUER` and `KEERA_OIDC_CLIENT_ID`, or the pair under each name in `KEERA_OIDC_PROVIDERS`. See [sso.md](sso.md).   |
| A revoked key still works for a few seconds            | `KEERA_CACHE_TTL`. The gateway refreshes its view of the control plane on a timer, not per request.                                                                        |
| A rate limit admits more than it is set to             | The buckets are per replica unless `KEERA_REDIS_URL` is set. If it is set, check `keera_ratelimit_fallback_total`.                                                         |
| Streaming arrives all at once                          | Something in front is buffering the response.                                                                                                                              |
| A developer says their agent stopped this afternoon    | The **Requests** screen filtered to their key, or their **My access** screen. Each refusal is logged with the message the client got.                                      |
| A limit is set and does not seem to apply              | `keera guardrail effective <scope> <id>`. Guardrails nest and the tightest level wins. The command shows which level each value came from.                                 |
| `keera` acts on a gateway you did not mean             | No deployment was set, so it used the default, `https://gateway.keera.ch`. Run `keera login --url <your deployment>` or set `KEERA_CONTROL_URL`. `keera whoami` shows why. |
