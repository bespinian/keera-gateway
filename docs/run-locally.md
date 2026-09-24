# Running it locally

The development loop runs the gateway on your host under
[air](https://github.com/air-verse/air), which rebuilds and restarts it about a
second after each save. Postgres and the inference backend run in containers.

```sh
make dev
```

It prints the panel's address and the operator key. `^C` stops the gateway but
leaves the containers running, so later starts are instant.

## What it does

1. **`dev-env`** - on a fresh clone, generates `compose/.env` with new
   credentials, like the NixOS unit does on first boot. An existing `.env` is
   left alone.
2. **`dev-backends`** - starts `keera-db` and `keera-engine` in podman, stops
   any gateway container holding port 8080, and waits for Postgres.
3. **air** - builds and runs the gateway on the host against those two, with
   `compose/models.dev.yaml` as the catalogue and debug logging on.

Install air once if you do not have it:

```sh
go install github.com/air-verse/air@latest
```

## Which inference backend

```sh
make dev                        # llama.cpp on the CPU (the default)
GATEWAY_DEV_TIER=gpu make dev   # vLLM, needs a GPU
```

**Tool calls do not work on the CPU tier.** Replies come back as prose in
`content` instead of `tool_calls`. Use `GATEWAY_DEV_TIER=gpu` when working on
the model probe, filters, routers or anything else that reads `tool_calls`. See
[compose/README.md](../compose/README.md).

## Driving it

```sh
make build
export KEERA_CONTROL_URL=http://127.0.0.1:8080
export KEERA_OPERATOR_KEY=$(sed -n 's/^KEERA_OPERATOR_KEY=//p' compose/.env)

./build/keera org create "Example Bank"
./build/keera team create "Payments Platform"
KEY=$(./build/keera key create --team <team-id> --alias "local")
```

The panel is at <http://127.0.0.1:8080>. Sign in with the operator key.

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

```sh
make compose-up
make compose-down
make compose-up COMPOSE_TIER=gpu    # vLLM instead of llama.cpp
```

This also runs the gateway as a container, as described in `compose/`. It is
good for a demo but slow for development: every change needs an image build.

`COMPOSE_TIER` picks the backend like `GATEWAY_DEV_TIER` does for `make dev`,
and defaults to `cpu`. Pass the same value to `compose-down`, or it stops a
different `keera-engine` than the one `up` started.

## Tests

```sh
make check             # go test -race, go vet, gofmt
make test-integration  # the store and the control plane, against a real Postgres
make check-all         # both
```

`make check` needs only Go. The store's tests skip without a database, so
`make check` does not cover the schema, its queries or the migrations.
`make test-integration` starts a throwaway Postgres in podman, runs those
packages against it and removes it. To use your own database, set
`KEERA_TEST_DATABASE_URL`.

The control plane is in the integration run for one thing no fake covers: the
live request log. The write path sends a Postgres notification, and a listener
picks it up.

## Building

```sh
make build          # ./build/keera-gateway and ./build/keera
nix build .#keera -o build/result
```

Every build writes to `build/`: the two binaries, the SBOM, and air's binary
and log from `make dev`. `make clean` deletes that directory.

The Nix build sets `vendorHash = null`, so it needs a `vendor` directory in the
source tree. That directory is not committed; run `go mod vendor` once after
cloning. With it, both builds work offline.

```sh
make image      # the gateway image, tagged localhost/keera-gateway:latest
make sbom       # rebuild the image and write build/sbom/keera-gateway.cdx.json
```

`make sbom` needs podman. It runs `syft` in a container unless you have `syft`
installed. It always rebuilds the image first; the build is cached, so this is
free when nothing changed.
