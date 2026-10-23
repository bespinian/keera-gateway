# Keera on podman compose

Four containers on one host: Keera Gateway, an inference backend, Keera
Decider and Postgres. Use it on a single machine or a laptop.

This is a **single-host development and demo deployment**. There is no TLS and
no ingress. Port 8080 is bound to loopback until you change `KEERA_BIND`.

## Run it

Pick a tier. `compose.yaml` on its own is the CPU tier; `compose.gpu.yaml`
switches the backend to vLLM, so `.env` does not change between tiers.

```sh
cp .env.example .env      # then set KEERA_OPERATOR_KEY and KEERA_SECRET_KEY

# A laptop, or any host without a GPU: llama.cpp, Qwen2.5-Coder-1.5B at Q4_K_M.
# Chat only - tool calling does not work on this tier. See the warning below.
podman compose up -d --build
podman compose logs -f keera-engine

# A GPU host: vLLM, Qwen2.5-Coder-7B-Instruct-AWQ at a 32k window.
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml   # once, on the host
podman compose -f compose.yaml -f compose.gpu.yaml up -d --build
```

On the GPU tier, pass the same `-f` flags to every later `podman compose` call
(`logs`, `exec`, `down`). Without them, compose acts on llama.cpp rather than
the vLLM that is running.

On the first start the weights download into the `llama-cache` volume
(`hf-cache` on the GPU tier); do not restart the inference container while it
is unready. The gateway is up at once. It
waits only for `keera-db`.

Stop with `podman compose down`, using the same `-f` flags you started with.
Add `-v` to also delete the Postgres and model volumes; the weights then
download again next time.

### The CPU tier cannot do tool calls

On llama.cpp build `b10853-9dcf84e5a` (September 2026), the Qwen2.5-Coder 1.5B
and 7B GGUFs gave no OpenAI-shaped `tool_calls` in nine attempts, only prose.
So use this tier to test the gateway, and the GPU tier, where vLLM's
`--tool-call-parser hermes` handles tool calls, to demo a coding agent.
`keera model check keera-speed` tells you which case you are in.

## Use it

Build the `keera` command with `make build` from the repository root, then
point it at this deployment:

```sh
export KEERA_OPERATOR_KEY=…                    # the value from .env
export KEERA_CONTROL_URL=http://127.0.0.1:8080
```

[First run](../docs/install.md#first-run) creates an organisation, a project and
a key, and sends a first request. Or open <http://127.0.0.1:8080> and sign in
with the operator key.

`keera-speed` is an alias. Only `models.yaml` should name a backend model id.
To swap the model, change `KEERA_LLAMA_MODEL` or `KEERA_GPU_MODEL` in `.env`
and recreate `keera-engine`. `KEERA_LLAMA_IMAGE` and `KEERA_VLLM_IMAGE` choose
the server images the same way. llama.cpp's `--alias` and vLLM's
`--served-model-name` keep serving it as `keera-speed`, so `models.yaml` and
developers' editors do not change.

The default GPU model is about 5.6 GB, so it fits a 24 GB card (g6.xlarge on
AWS) with room for about eight 32k sessions. On an Ada card, drop the `-AWQ`
suffix and add `--quantization fp8` to the command. For a stronger model,
`Qwen/Qwen2.5-Coder-14B-Instruct-AWQ` (about 9.9 GB) runs one or two sessions
on the same card; the unquantised 14B needs a 48 GB card (g6e.xlarge).

## Notes

- **`models.yaml` is what each new organisation starts with.** After that, the
  models are the organisation's own
  ([Template files](../docs/gateway.md#template-files)).
- **The backends use the model's own chat template.** Qwen's adds "You are
  Qwen, created by Alibaba Cloud..." when a request has no system message. For
  a standing system prompt, set one on a guardrail:
  `keera guardrail set org <org-id> --system-prompt ...`.
- **Port 8080 carries the panel, the control API and the inference API**
  ([gateway.md](../docs/gateway.md#one-listener-four-surfaces)). Put a TLS
  terminator in front before you change `KEERA_BIND`.
- **Port 8000 is not published on purpose**: the inference backend has no
  authentication ([gateway.md](../docs/gateway.md#failure-modes-worth-knowing)).
  For the same reason the llama.cpp tier runs with `--no-webui`.
- **Every setting in `.env` reaches the gateway**
  ([docs/install.md](../docs/install.md) lists them). `compose.yaml` fixes the
  database, the listen address and the template files, and keeps
  `KEERA_SANDBOX_DRIVER` empty. A variable exported in your shell but not
  written in `.env` does not reach it.
- The gateway image is `FROM scratch`: one static binary, no shell. So its
  healthcheck is `keera-gateway health`, which asks the running server's
  `/readyz` instead of `curl`.
- **`KEERA_SHM_SIZE`**: podman gives a container 64 MB of `/dev/shm`, and vLLM
  needs far more. It fails at startup with `Insufficient space in /dev/shm`.
  The usual fix is `--ipc=host`, but podman-compose 1.6 ignores `ipc:`, so the
  GPU tier sets the `/dev/shm` size instead, 8 GB by default.
- **`max_context_tokens` must match the backend's window.** `models.yaml` says 16384,
  which is safe on both tiers. On the GPU tier, raise it on each
  organisation's model with `keera model set keera-speed --max-context-tokens 32768`.
- **Do not override `healthcheck.test`** on podman-compose 1.6. It appends the
  new command to the old one and runs neither. The GPU tier does not override
  it; this works because vLLM's `/health` uses the same port and path as
  llama.cpp's. Only the scalar keys (`start_period` and similar) are safe to
  override.

## Keera Decider

`keera-decider` runs [Laya](https://huggingface.co/convaiinnovations/laya), a
421M classifier, with its own server, `laya-serve`, on CPU. Laya publishes no
image, so `decider/Containerfile` installs it. The first start downloads about
0.8 GB into the `decider-cache` volume.

It is a [decision model](../docs/gateway.md#decision-models): route steps and
gate filters can decide with it, and it writes no text. For example, a router
clients call as `auto`, once `keera-frontier` is uncommented in `models.yaml`:

```sh
keera router add auto --model keera-decider \
  --destinations keera-speed,keera-frontier --fallback keera-speed \
  --prompt "Which model should answer this request?"
```

`backend_model` in `models.yaml` picks the checkpoint: `english`,
`multilingual` or `typed-decisions`. Add it to `LAYA_MODELS` in the
Containerfile too, or `laya-serve` loads it on the first request.

## Sandboxes do not work in this shape

`sandboxes.yaml` is read, so each new organisation starts with its classes, but
`KEERA_SANDBOX_DRIVER` is not set. The podman driver runs `podman`, and the
gateway here is a `FROM scratch` container: it would need the host's podman
socket, and its sandboxes would then have all the power of the host's podman.

So run sandboxes with `make dev`, where the gateway runs on the host:

```sh
make sandbox-image                                  # once
echo 'KEERA_SANDBOX_DRIVER=podman' >> compose/.env
make dev
```

The gateway under compose runs no sandboxes, so only `make dev` uses the driver
settings in `.env`, and it sets `KEERA_SANDBOX_PUBLIC_URL` for you.
`KEERA_SANDBOX_IMAGES` reaches both. See [docs/sandboxes.md](../docs/sandboxes.md).
