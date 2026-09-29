# Keera on podman compose

Three containers on one host: Keera Gateway, an inference backend and Postgres.
No NixOS build is needed. Use it on a single machine or a laptop.

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

### The CPU tier cannot do tool calls

Tested on llama.cpp build `b10853-9dcf84e5a` in September 2026: nine attempts
with the Qwen2.5-Coder 1.5B and 7B GGUFs gave **zero** OpenAI-shaped
`tool_calls`. Every reply came back as prose in `content` (a fenced JSON block,
or `<xml>` tags). This is the `ToolCallAsText` failure that
`internal/gateway/probe.go` reports.

The prompt and the catalogue are not the cause:

- `/apply-template` renders the full `# Tools` block and the `<tool_call>`
  instruction.
- `chat_template_caps` on `/props` reports `supports_tools` and
  `supports_tool_calls` both true.
- `tool_choice: "required"`, which should force a tool call, returns HTTP 200
  and is then ignored.

Qwen's own GGUF repositories also ship a **corrupted chat template**: `{{"name"`
where [the original
repo](https://huggingface.co/Qwen/Qwen2.5-Coder-7B-Instruct/raw/main/tokenizer_config.json)
has `{"name"`. One 7B reply echoed the doubled brace back. Using the correct
template with `--chat-template-file` did not help, so that bug is real but is
not the cause.

Use this tier to test **the gateway** without a GPU: keys, budgets, rate
limits, streaming, usage accounting and the panel. Do not use it to demo **a
coding agent**. OpenCode and Claude Code will write prose instead of edits. Use
the GPU tier for that, where vLLM's `--tool-call-parser hermes` handles tool
calls.

On either tier, `keera model check keera-speed` is the acceptance test. Re-run
it on a newer llama.cpp build before trusting the above.

On the first start the weights download into the `llama-cache` volume
(`hf-cache` on the GPU tier), and the inference container stays unready for
minutes. Watch the logs; do not restart it.

The gateway is up at once. It applies its schema on start and waits only for
`keera-db`.

## Use it

Administration uses the `keera` binary, which is separate from the
`keera-gateway` server and not in its image. Build it with `make build` from
the repository root.

```sh
export KEERA_OPERATOR_KEY=…                    # the value from .env
export KEERA_CONTROL_URL=http://127.0.0.1:8080

keera org create "Example Bank"
keera team create "Payments Platform"       # --org is inferred when there is one
KEY=$(keera key create --team <team-id> --alias "a developer's laptop")
```

`keera key create` prints the key once and does not store it. Nobody, not even
an operator, can read it later.

Then call the gateway:

```sh
curl http://127.0.0.1:8080/api/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"keera-speed","messages":[{"role":"user","content":"What is Kubernetes?"}],"max_tokens":200}' \
  | jq '.choices[0].message.content'
```

`keera-speed` is an alias. Only `models.yaml` should name a backend model id.
To swap the model, change `KEERA_LLAMA_MODEL` or `KEERA_GPU_MODEL` in `.env`
and recreate `keera-engine`. `KEERA_LLAMA_IMAGE` and `KEERA_VLLM_IMAGE` choose
the server images the same way. llama.cpp's `--alias` and vLLM's
`--served-model-name` keep serving it as `keera-speed`, so `models.yaml` and
developers' editors do not change.

Guardrails and reports:

```sh
keera guardrail set team <team-id> --models keera-speed --rpm 120 --budget 500 --period month
keera guardrail get team <team-id>
keera usage --by team
keera usage --by day --since 168h
```

Stop with `podman compose down`, using the same `-f` flags you started with.
Add `-v` to also delete the Postgres and model volumes; the weights then
download again next time.

## Notes

- **`models.yaml` is what each new organisation starts with.** Every new
  organisation gets a copy of every model in the file, whether it comes from
  `keera org create`, the panel, or signing in to the panel with the operator
  key while no organisation exists yet (that one is called "Keera"). After
  that, the models are the organisation's own. `backend_model` must match the
  name the backend serves the model under (vLLM's `--served-model-name`,
  llama.cpp's `--alias`). If it does not, every request returns 404.
- **The backends use the model's own chat template.** Qwen's adds "You are
  Qwen, created by Alibaba Cloud..." when a request has no system message. For
  a standing system prompt, set one on a guardrail:
  `keera guardrail set org <org-id> --system-prompt ...`.
- **Port 8080 carries everything**, split by path: the panel at `/`, the
  inference API under `/api`, the control API under `/control`, sandbox attach
  under `/sandbox`. Wherever the
  inference API is reachable, so is the control API, protected only by the
  operator key and a session cookie. It binds to loopback by default
  (`KEERA_BIND`). Before opening it up, put a TLS terminator in front. If
  editors must not reach the control plane, add a proxy that serves only
  `/api`.
- **Port 8000 is not published on purpose.** Neither vLLM's nor llama.cpp's
  OpenAI server has authentication, so reaching one directly skips the
  gateway's keys, budgets and audit. For the same reason the llama.cpp tier
  runs with `--no-webui`.
- **`.env` reaches the gateway only through `compose.yaml`.** Compose passes
  on only the variables the file names. It passes every setting in
  [docs/install.md](../docs/install.md) except the sandbox ones, and single
  sign-on only for providers named `google` and `entra`. The database, the
  listener and the catalogue files are fixed in the file.
- The gateway image is `FROM scratch`: one static binary, no shell. So its
  healthcheck is `keera-gateway health`, which asks the running server's
  `/readyz` instead of `curl`.
- **`KEERA_SHM_SIZE`**: podman gives a container 64 MB of `/dev/shm`, and vLLM
  needs far more. It fails at startup with `Insufficient space in /dev/shm`.
  The usual fix is `--ipc=host`, but podman-compose 1.6 ignores `ipc:`, so the
  GPU tier sets the `/dev/shm` size instead, 8 GB by default.
- **`max_context` must match the backend's window.** Clients read it from
  `/v1/models`, and filter and router models are sized against it. A wrong
  value misleads every client and mis-sizes every hook. `models.yaml` says
  16384, which is safe on both tiers. On the GPU tier, raise it on each
  organisation's model with `keera model set keera-speed --max-context 32768`.
- **Do not override `healthcheck.test`** on podman-compose 1.6. It appends the
  new command to the old one and runs neither. The GPU tier does not override
  it; this works because vLLM's `/health` uses the same port and path as
  llama.cpp's. Only the scalar keys (`start_period` and similar) are safe to
  override.

## Sandboxes do not work in this shape

`sandboxes.yaml` is here but `KEERA_SANDBOX_DRIVER` is not set. This is on
purpose. The gateway still reads the file, so each new organisation starts with
its classes, ready for when the gateway runs with a driver.

The single-host sandbox driver creates containers by running `podman`. Here the
gateway _is_ a container, so it would need podman's socket mounted into a
`FROM scratch` image with no shell. The sandbox containers would then run next
to the gateway with all the power of the host's podman.

So the podman driver is for a host that runs the gateway binary directly, as
`make dev` does:

```sh
make sandbox-image                                  # once
echo 'KEERA_SANDBOX_DRIVER=podman' >> compose/.env
make dev
```

`.env.example` documents the other sandbox settings. Only `make dev` reads
them: `compose.yaml` does not pass them on. Note `KEERA_SANDBOX_PUBLIC_URL`: a
sandbox reaches the gateway at `host.containers.internal`, not `127.0.0.1`
(inside a container, that is the container itself). `make dev` sets it
correctly.

On a cluster this does not apply: the Kubernetes driver only writes objects to
an API server, so the gateway keeps all its restrictions.

See [docs/sandboxes.md](../docs/sandboxes.md).

## Differences from the NixOS deployment

- **The gateway is a container here.** On NixOS it is a systemd unit running
  the binary, with native Postgres and peer authentication over the local
  socket. Only the inference backend is a container there.
- **The NixOS host runs vLLM on CPU.** Compose runs llama.cpp on CPU, which is
  faster on the same hardware, and vLLM only on a GPU.
- **Named volumes** (`llama-cache`, `hf-cache`, `pgdata`) instead of paths under
  `/var/lib/keera`. Podman will not create a missing bind-mount source, so bind
  mounts would need `tmpfiles` rules. Named volumes avoid that.
- **The operator key comes from `.env`.** The NixOS variant generates one into
  `/var/lib/keera/env` on first boot.
