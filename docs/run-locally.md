# Running it locally

The development loop runs the gateway on your host under
[air](https://github.com/air-verse/air), which rebuilds and restarts it about a
second after each save. Postgres, the inference backend and Keera Decider run in containers.

```sh
make dev
```

It prints the panel's address and the operator key. `^C` stops the gateway but
leaves the containers running, so later starts are instant.

## What it does

1. **`dev-env`** - on a fresh clone, generates `compose/.env` with new
   credentials, like the NixOS unit does on first boot. An existing `.env` is
   left alone.
2. **`dev-backends`** - starts `keera-db`, `keera-engine` and `keera-decider`
   in podman, stops any gateway container holding port 8080, and waits for
   Postgres.
3. **air** - builds and runs the gateway on the host against those three, with
   the settings below.

The gateway reads every setting in `compose/.env`, sandbox ones included.
`make dev` sets these:

| Setting                    | `make dev` sets                                                                                                                     | A value in `.env` |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | ----------------- |
| `KEERA_DATABASE_URL`       | the `keera-db` container, on `127.0.0.1:5432`                                                                                       | is ignored        |
| `KEERA_LOG_LEVEL`          | `debug`                                                                                                                             | is ignored        |
| `KEERA_LOG_FORMAT`         | `text`                                                                                                                              | is ignored        |
| `KEERA_MODELS_FILE`        | `build/models.dev.yaml`, a copy of `compose/models.yaml` with the backends at `127.0.0.1:8000` and `:8001`                          | wins              |
| `KEERA_SANDBOXES_FILE`     | `compose/sandboxes.yaml`                                                                                                            | wins              |
| `KEERA_SANDBOX_PUBLIC_URL` | `http://host.containers.internal:8080`, when `KEERA_SANDBOX_DRIVER` is set                                                          | wins              |
| `KEERA_UPSTREAM_DENY`      | the default list without loopback, because the backends are on `127.0.0.1` ([install.md](install.md#where-the-gateway-may-connect)) | wins              |

The two files only seed new organisations
([Template files](gateway.md#template-files)). The addresses `make dev` prints
assume the default `KEERA_ADDR` of `:8080`.

Install air once if you do not have it:

```sh
go install github.com/air-verse/air@latest
```

## Which inference backend

```sh
make dev            # llama.cpp on the CPU (the default)
make dev TIER=gpu   # vLLM, needs a GPU
```

Use `TIER=gpu` for anything that reads `tool_calls`, such as the model check,
filters and routers: the CPU tier
[cannot do tool calls](../compose/README.md#the-cpu-tier-cannot-do-tool-calls).

## Driving it

```sh
make build
export KEERA_CONTROL_URL=http://127.0.0.1:8080
export KEERA_OPERATOR_KEY=$(sed -n 's/^KEERA_OPERATOR_KEY=//p' compose/.env)
```

Then follow [First run](install.md#first-run) with `./build/keera`. The panel is
at <http://127.0.0.1:8080>. Sign in with the operator key.

## Stopping

```sh
# ^C stops the gateway, containers keep running
make dev-down    # stop the containers, keep the volumes
```

The volumes are kept, so the model weights and the database survive.
`podman-compose` prints a harmless `no container ... keera_keera-gateway_1`
here, because it tries to remove the gateway service, which this loop never
starts.

## Running the whole thing in containers instead

[compose/README.md](../compose/README.md) runs the gateway as a container
too. It is good for a demo but slow for development: every change needs an
image build.

Both setups share one database, but reach the engine at different addresses.
An organisation created under one keeps that one's backend address, so its
models do not answer under the other. Use a fresh organisation, or point its
models at the other address with `keera model set`.

## Tests

```sh
make check             # go test -race, go vet, gofmt
make test-integration  # store, control plane and rate limiter, against Postgres and Redis
make check-all         # both
```

`make check` needs Go and a C compiler, because the race detector needs cgo.
The store's tests skip without a database, so `make check` does not cover the
schema, its queries or the migrations. `make test-integration` starts a
throwaway Postgres and Redis in podman, runs those packages against them and
removes them. To use your own instead, set both `KEERA_TEST_DATABASE_URL` and
`KEERA_TEST_REDIS_URL`.

The control plane is in the integration run for the live request log, and the
rate limiter for the buckets shared through Redis. No fake covers either.

## Building

```sh
make build          # ./build/keera-gateway and ./build/keera
make dist           # the keera release archives, to build/dist
nix build .#keera -o build/result
```

Every build writes to `build/`: the two binaries, the SBOM, the third-party
notices, the release archives, and air's binary and log from `make dev`.
`make clean` deletes that directory. The one file written elsewhere is
`compose/.env`, which `make dev` creates on a fresh clone. `make clean` keeps
it.

Run `go mod vendor` after cloning and whenever `go.mod` changes; every Go
command, `make image` included, builds from `vendor/`. When it is missing,
`make image` runs `go mod vendor` inside the image build, which needs network
access and leaves your own `vendor/` missing. It does not refresh an old one. The Nix build
checks the modules against `vendorHash` in `flake.nix`: when `go.mod` changes,
set it to `pkgs.lib.fakeHash`, build, and copy the hash Nix prints.

```sh
make image      # the gateway image, tagged localhost/keera-gateway:latest
make sbom       # rebuild the image and write build/sbom/keera-gateway.cdx.json
```

`make sbom` needs podman. It runs `syft` in a container unless you have `syft`
installed. It always rebuilds the image first; the build is cached, so this is
free when nothing changed.
