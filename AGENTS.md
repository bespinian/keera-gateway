# AGENTS.md

Keera is the AI stack run in Switzerland: **Keera Gateway** is the one endpoint
every AI request passes through, **Keera Engine** serves the open-weight coding
models behind it. See [README.md](README.md).

The repository is one Go module: a gateway (`cmd/keera-gateway`), a CLI
(`cmd/keera`), and the packages under `internal/`. `vendor/` is gitignored - run
`go mod vendor` once after cloning, and do not edit what it writes.

This repository holds what is built from this source tree: the gateway's
image, the compose deployment, which is also the development loop, and the
sandbox base image.

## Layout

| Path                 | What it is                                                 |
| -------------------- | ---------------------------------------------------------- |
| `cmd/keera-gateway`  | The server binary: the listener and the schema.            |
| `cmd/keera`          | Administration command line, over the control API.         |
| `internal/server`    | Wiring and lifecycle of the server binary.                 |
| `internal/cli`       | The `keera` command: every subcommand and its help.        |
| `internal/gateway`   | Inference data plane: guardrails, streaming, accounting.   |
| `internal/control`   | Control plane: tenancy, guardrails, reports, audit.        |
| `internal/authn`     | Roles, permissions and the OpenID Connect flow.            |
| `internal/auth`      | API key hashing and verification.                          |
| `internal/webui`     | The control panel, embedded in the server binary.          |
| `internal/policy`    | The tenancy model and how limits combine.                  |
| `internal/catalog`   | The model catalogue file and the hosted-provider table.    |
| `internal/registry`  | The gateway's in-memory view of the control plane.         |
| `internal/sandbox`   | The machines it lends out: two drivers, and the manager.   |
| `internal/forge`     | Short-lived repository credentials, from GitHub or GitLab. |
| `internal/connect`   | The client catalogue the panel and `keera connect` share.  |
| `internal/ratelimit` | Request and token rate limiting.                           |
| `internal/usage`     | The usage recorder behind billing and reports.             |
| `internal/metrics`   | The Prometheus exposition on `/metrics`.                   |
| `internal/secret`    | Encryption for hosted-provider API keys.                   |
| `internal/store`     | Postgres schema and queries.                               |
| `internal/config`    | Every setting, read from the environment.                  |
| `internal/httpx`     | Shared HTTP plumbing: prefixes, compression, request ids.  |
| `internal/id`        | The identifiers every row and every key are named with.    |
| `internal/version`   | The build version both binaries report.                    |
| `Containerfile`      | The image the gateway ships in. `FROM scratch`.            |
| `compose`            | Single-host deployment, and the development loop.          |
| `sandbox`            | The sandbox image: sshd, the entrypoint, the Git helper.   |
| `scripts`            | The third-party notices the image and the archives carry.  |

## Commands

```sh
make build             # both binaries
make check             # test + vet + fmt-check, what CI runs
make test              # go test -race -cover ./...
make test-integration  # store, control and ratelimit tests, against throwaway containers
make check-all         # check + test-integration
make lint              # golangci-lint, if installed
make dev               # the development loop: backends in containers, gateway under air
make image             # the gateway image, tagged localhost/keera-gateway:latest
make sandbox-image     # the sandbox base image, which the podman driver needs
make sbom              # rebuild the image and write build/sbom/keera-gateway.cdx.json
make notices           # the licences of what is compiled in, to build/THIRD_PARTY_NOTICES
make dist              # the keera command for every release platform, to build/dist
nix build .#keera      # needs the vendor directory
```

Everything those write goes to `build/` - the two binaries, the SBOM, the
notices, the release archives, and what air rebuilds during `make dev`. `make clean` removes it.

Run `make check` before handing work back.

## Conventions

- Format with `gofmt`. Standard library first, no extra dependencies unless asked.
- Comments explain why something is the way it is, not what the code does.
- Tests live beside the code. Tests that need Postgres or Redis skip without
  `KEERA_TEST_DATABASE_URL` / `KEERA_TEST_REDIS_URL`.
- Secrets are never logged and keys are never stored in plaintext.
- The CLI paints its output only when the stream is a terminal, and the words
  say the same thing without the colour. Paint through `style`/`styleErr` in
  `internal/cli/style.go`, and write table rows to `*table`, which measures a
  cell by what it shows.

## Docs

`docs/` explains the pieces. `run-locally.md` is the development loop and
`install.md` every setting; `gateway.md`, `filters.md`, `routers.md`,
`sandboxes.md`, `providers.md`, `mcp.md`, `sessions.md`, `sizing.md` and
`sso.md` each take one part. Update them when behaviour changes.

## Instructions

- Always keep code comments, documentation, and user-facing texts concise, easy to read, and in simple language
