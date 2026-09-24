# Sandboxes

A sandbox is a machine Keera lends out for one task or one working day.

The rest of Keera controls the requests an editor sends. It cannot see what an
agent does with the answer on a laptop. A sandbox moves that work into the
cluster.

## What it buys

**Egress can be enforced.** A network policy can allow only the gateway, the
internal registry and the internal Git. A script the agent writes to upload the
repository then fails. How much this is worth depends on the driver and the
cluster; see [What is actually enforced](#what-is-actually-enforced). On the
podman driver there is no egress control today.

**The API key never lands on a laptop.** Each sandbox gets its own key at
creation, scoped to whoever asked for it. The key expires with the sandbox and
is revoked when it goes. The sandbox boots already pointed at the gateway, so
`keera connect` is not needed inside.

**A session is known, not guessed.** [sessions.md](sessions.md) groups requests
into tasks by inference, which can be wrong in two ways. An agent sandbox is
exactly one task, so it sends its own id in `X-Keera-Session`. The session key
is computed once at creation, the same way the gateway computes it per request,
so a sandbox and its task can be read side by side.

## Two lifecycles, one substrate

|                  | Engineer                          | Agent                              |
| ---------------- | --------------------------------- | ---------------------------------- |
| Default lifetime | the class's, typically 8h         | the class's, typically 1h          |
| When it expires  | suspended, volume kept, resumable | terminated                         |
| When it is idle  | suspended after `IDLE_SUSPEND`    | never - it has nothing to wait for |
| Session header   | not set                           | `X-Keera-Session: <sandbox id>`    |
| Attaching        | ssh, from its owner               | nothing attaches                   |
| How work leaves  | the developer pushes              | a branch, pushed by the agent      |

An agent sandbox's work is already pushed as a branch, so nothing in it is worth
keeping. An engineer's sandbox holds unfinished work, so it is kept.

**Nothing attaches to an agent sandbox.** A shell would not be less safe - the
isolation is the same. The point is that only a branch comes out of it.

## Agent-in-sandbox, not sandbox-as-a-tool

The upstream SDK is built for an agent outside that drives a sandbox through
`Run()` and `Files().Write()`. That sends the source code out through the API on
every read.

In Keera the agent runs _inside_. `keera sandbox agent <name> --repo … --task …`
starts a machine, checks out the repository, runs Claude Code, OpenCode or Pi on
the task, and pushes a branch. Only that branch leaves.

## Isolation

The upstream controller has no isolation of its own; it uses a `RuntimeClass`.
So the tier is set per class. The catalogue names the tier, and the deployment
maps each of the three tiers to a runtime name once.

| Tier       | Runtime           | Boundary                        | Cost                                                                    |
| ---------- | ----------------- | ------------------------------- | ----------------------------------------------------------------------- |
| `standard` | the cluster's own | the host kernel, as for any pod | none                                                                    |
| `isolated` | gVisor (`runsc`)  | syscalls served in userspace    | some syscall-heavy work; no `io_uring`, some FUSE, no nested containers |
| `vm`       | Kata              | **a kernel of its own**         | 1–2s of boot, ~100–200 MB per sandbox, hardware virtualisation          |

`vm` is the best tier for an agent sandbox, since it runs code a model wrote.

**It is not the default, because of hardware.** Kata needs hardware
virtualisation on the node. When the nodes are VMs themselves - VMware, Nutanix,
most Swiss enterprise setups - that means nested virtualisation, which a
platform team may not enable. So gVisor is a full tier, not a fallback.

A class that asks for a tier the deployment has not mapped is **refused at
creation**, with the name of the setting to fix. It never runs at a weaker tier.

Every tier gets this hardening: a non-root user, `no-new-privileges`, all
capabilities dropped, no service account token mounted, and `RuntimeDefault`
seccomp. The root filesystem stays writable, because people work in a sandbox.

## Getting in, without a second address

The gateway's one listener has a fourth surface:

```
GET /sandbox/v1/{name-or-id}/tcp/{port}
Connection: Upgrade
Upgrade: keera-sandbox/1
```

The gateway authenticates the caller as it does for the control API, checks that
the sandbox is theirs, dials it, answers `101`, and copies bytes until one end
stops. The far end is a normal ssh server, so **VS Code Remote-SSH and JetBrains
Gateway work unmodified**.

sshd listens on **2222**, not 22, because a process with every capability
dropped cannot bind a privileged port. Nobody types the number: the gateway
dials it, and a developer goes through the ProxyCommand.

```sh
keera sandbox create fix-login --class standard --repo git@internal:team/service.git
keera sandbox ssh fix-login
keera sandbox config >> ~/.ssh/config   # then: ssh keera-fix-login, or open it in any editor
```

`keera sandbox config` writes a `ProxyCommand` that runs `keera sandbox proxy`.
It speaks the upgrade above and pipes it to its own stdin and stdout.

**What this needs from a proxy in front.** The upgrade must survive whatever
publishes the gateway. nginx-ingress and Traefik pass `Connection: Upgrade` by
default. If a proxy strips it, `keera sandbox ssh` gets a 426 with a message
saying so.

## Who may do what

|                            | See it | Terminate, suspend, extend | Open a shell |
| -------------------------- | ------ | -------------------------- | ------------ |
| Operator                   | yes    | yes                        | yes          |
| Organisation administrator | yes    | yes                        | **no**       |
| The owner                  | yes    | yes                        | yes          |
| Anybody else               | no     | no                         | no           |

An administrator can see and terminate every sandbox in their organisation,
because the quota and the bill are theirs. They cannot get inside: a sandbox
holds someone's source code and what they typed into a terminal. An
administrator who needs access can create their own sandbox on the same
repository.

A sandbox the caller may not see answers 404, not 403. So nobody can use this to
find out whether a colleague has a sandbox called `acquisition-model`.

## The catalogue

A class name is an API contract. People type it, commit it into repository
config and get used to it. So the operator can change its image, isolation tier
or memory without anyone else editing anything.

```yaml
# KEERA_SANDBOXES_FILE
sandboxes:
  - name: standard
    description: The repo toolchain, an editor server, an agent.
    image: registry.internal/keera/sandbox-base:1
    isolation: isolated # standard | isolated | vm
    cpu: "4" # or 4000m
    memory: 16Gi
    disk: 50Gi # empty keeps nothing across a suspend
    default_ttl: 8h
    max_ttl: 24h
    warm: 2
    purposes: [engineer] # empty is both

  - name: agent
    image: registry.internal/keera/sandbox-agent:1
    isolation: vm
    cpu: "4"
    memory: 8Gi
    default_ttl: 1h
    max_ttl: 4h
    purposes: [agent]
```

The file is applied on every start and is idempotent: the same file and an empty
database give the same deployment. A class declared in the file belongs to the
file; the panel and the control API refuse to change it. A class removed from
the file is _released_, not deleted, as with models.

Deleting a class does not break running sandboxes. Each one keeps its own copy
of the image, the isolation tier and the resources it was given.

## What is actually enforced

|                     | Kubernetes driver                                       | podman driver   |
| ------------------- | ------------------------------------------------------- | --------------- |
| Network isolation   | a default-deny NetworkPolicy over the sandbox namespace | **none**        |
| Where it comes from | the Helm chart, `networkPolicy.enabled`                 | -               |
| Granularity         | one rule for the whole namespace                        | -               |
| Per-class `egress:` | **not applied**                                         | **not applied** |

**A podman sandbox has unrestricted outbound access.** It joins podman's default
bridge and can reach the internet, the host and the LAN. This is tested:
`curl https://github.com` from inside one returns 200. Use the single-host
driver for development, or on a host the deployment already trusts.

**The Kubernetes rule covers the whole namespace and needs a CNI that enforces
it.** On a network plugin without NetworkPolicy support - flannel without a
policy plugin, for example - the policy does nothing, and nothing warns you.
Check it on the cluster.

**The per-class `egress:` list does nothing.** It is parsed, validated, stored
and shown, but neither driver reads it. It is kept because per-class rules are
where this should go. On every start, the gateway warns for each class that sets
it.

## Quotas

Sandbox quotas are fields on the guardrail row. They combine by the usual
restrict-only rule: the minimum for a ceiling, the intersection for a list.

```sh
keera guardrail set team <team-id> \
  --max-sandboxes 6 \
  --max-sandbox-ttl 8h \
  --sandbox-classes standard,small \
  --max-sandbox-cpu 8 \
  --max-sandbox-memory 32768
```

A size ceiling **refuses** a request; it does not quietly downgrade it. A scope
capped at four cores that asks for an eight-core class gets an error, not a
smaller machine that cannot build its code.

**Extending is safe to allow.** The new lifetime counts from now, not from what
is left, and the same two ceilings as at creation apply. A developer can extend
as often as needed, and the sandbox still cannot live forever.

Quotas attach to the organisation and the team. There is no per-key sandbox
quota: a sandbox's key is minted _for_ that sandbox and dies with it.

**Only the command line sets them.** The panel's guardrails dialog has no
sandbox fields. The panel's Sandboxes screen shows what is left of the quota and
refuses a class the quota forbids. Saving the guardrails dialog keeps the
sandbox fields unchanged: a guardrail is written whole, and the dialog only
changes the fields it shows.

## Naming a team

`keera sandbox create <name> --team <id>`, or the Team field in the panel's
dialog. The sandbox's key is scoped to that team, which decides:

- which **budget** the agent's inference is charged to,
- which **rate limit** it uses,
- whose **sandbox quota** the machine counts against,
- which **system prompt** every request it makes carries,
- and **which models** its agent may reach, which is what the picker inside the
  sandbox shows.

Without a team, the sandbox uses the organisation's own guardrails.

**The team must belong to the same organisation.** The foreign key only checks
that the team exists, so the control plane checks the owner. Otherwise a sandbox
could use another tenant's system prompt, budget, rate limit and model
allow-list.

Only an administrator may choose a team. A member's sandbox uses the
organisation's guardrails and no team, just as a member cannot choose the team
of a key they issue themselves.

## What it costs

Sandbox time is recorded beside tokens, so a task's cost includes the machine it
ran on, such as "eleven minutes of a four-core machine".

```sh
keera sandbox usage --by team --since 720h
keera sandbox usage --by user
keera sandbox usage --by class
```

The report has three columns, because no single one tells the whole story.
Forty sandboxes that each lived ninety seconds is an agent fleet; one that lived
a week is somebody who forgot. Core-seconds is what a chargeback uses.

Running time is summed across runs, not derived from timestamps, because a
sandbox can be suspended and resumed many times. The clock stops as soon as a
sandbox stops holding compute, not at the next sweep.

If the gateway was down, the time it could not observe is still charged,
because the sandbox was running.

## Code in, work out

**In.** At creation the gateway mints a credential for the sandbox: one
repository, short-lived, and never the developer's own. No ssh agent is
forwarded.

Two forges are built in. Each needs a credential of the deployment's own, which
stays on the gateway.

|                             | GitHub                                  | GitLab                                                              |
| --------------------------- | --------------------------------------- | ------------------------------------------------------------------- |
| What a sandbox gets         | an installation token of a GitHub App   | a project access token                                              |
| Scope                       | one repository, contents read and write | one project, Developer role, repository read and write              |
| Lifetime                    | one hour, fixed by GitHub               | until midnight UTC after the sandbox ends, and revoked when it ends |
| The deployment's credential | the App's id and private key            | a token with the `api` scope, held by a Maintainer of the projects  |

If neither is configured, a sandbox with a repository is refused with that
reason, instead of failing inside the sandbox.

**Any address works.** `git@host:group/app.git`, `ssh://…` and `https://…` are
all accepted and cloned over HTTPS, because tokens only work there. A repository
on a different host than the forge's is refused.

**Tokens are refreshed.** A GitHub token lasts an hour; an engineer's sandbox
lasts a working day. Git in the sandbox asks `git-credential-keera`, which keeps
the current token. When it has five minutes left, the helper gets a new one from
`POST /sandbox/v1/git-credential` with the sandbox's own key. The gateway
revokes the replaced token where the forge allows it. The helper only answers
for the repository's own host, so a submodule on another host never sees the
token.

**When the sandbox ends, so does access.** Its key is revoked, so it cannot get
another token. A GitLab token is revoked at once. A GitHub token cannot be
revoked without the token itself, which is not kept, so the last one works for
up to an hour more.

**When the owner is disabled** (see [sso.md](sso.md#when-someone-leaves)), their
agent sandboxes are terminated, their engineer sandboxes are suspended, and
every repository credential they held is revoked.

**What it cannot do.** Neither token can change CI: GitHub gets no `workflows`
permission, and GitLab's Developer role cannot push to a protected branch. Keep
the default branch protected, and an agent can only propose a change.

Setting up GitHub: create a GitHub App with **Repository permissions → Contents:
Read and write** and nothing else, install it on the repositories sandboxes may
use, and download its private key. For GitHub Enterprise Server, set
`KEERA_SANDBOX_GIT_URL` to `https://<host>/api/v3`.

Setting up GitLab: create a token with the `api` scope for a user, group or
service account that is a Maintainer of those projects. Project access tokens
need GitLab Premium on GitLab.com; self-managed GitLab has them on every tier.

**Out.** Push a branch. For an agent sandbox that is the only egress. The agent
runner never writes to a default branch: its output is a proposal, and a person
opens it.

## Speed, and what it costs to have

Cold start takes 3–8 seconds on a node that already has the image, a minute or
more on one that does not, plus a second or two for a microVM.

Warm pools fix this. They are opt-in twice - `KEERA_SANDBOX_WARM` for the
deployment, `warm:` per class - because every warm sandbox holds its class's
full CPU and memory all the time, used by nobody.

They need the upstream warm-pool extension (`extensions.agents.x-k8s.io/v1beta1`:
`SandboxTemplate`, `SandboxWarmPool`, `SandboxClaim`) installed next to the
controller. The pool's update strategy is `OnReplenish`, not `Recreate`, so
changing a class in the afternoon does not empty the pool while people use it.
The downside: for a few minutes after a change, some sandboxes get the previous
image. Every sandbox records which image it got.

Whether a sandbox came from a pool is stored on its row, not derived from
configuration. So a deployment that turns warm pools off still knows which
sandboxes came from one.

Pools do not outlive their class. The sweep deletes the pool and template of a
class removed from the catalogue, and a gateway started with warm pools off
deletes every pool it finds. Both need `list` on templates and pools.

## The two drivers

|                    | `kubernetes`                                      | `podman`                           |
| ------------------ | ------------------------------------------------- | ---------------------------------- |
| Where              | a cluster, via agent-sandbox                      | one host                           |
| Isolation          | whatever RuntimeClasses are mapped                | whatever OCI runtimes the host has |
| Expiry enforced by | the controller                                    | **the gateway**                    |
| Network isolation  | a namespace NetworkPolicy, if the CNI enforces it | **none**                           |
| Warm pools         | with the extension                                | no                                 |
| Suspend/resume     | yes                                               | yes (`stop`/`start`)               |
| Volumes            | a PVC                                             | a named volume                     |

Expiry is the real difference. On Kubernetes it is `spec.shutdownTime` and the
cluster enforces it, so **a gateway that crashes and never comes back leaves no
sandboxes behind.** On podman there is no controller, so the gateway's sweep
enforces it. A gateway that is down over a weekend comes back to containers that
should have ended on Friday. `Reap` terminates those at startup, reading the
expiry from container labels, not from the database.

To run podman locally, the gateway must run as a host process: run
`make sandbox-image` once, then set `KEERA_SANDBOX_DRIVER=podman` in
`compose/.env` and run `make dev`. The compose deployment's own gateway cannot
use this driver: it is a `FROM scratch` container, with no podman in it.

## No client-go

The Kubernetes driver calls the API server over plain HTTPS with the service
account token, and decodes only the fields it reads. `client-go` would make the
module graph a hundred times bigger to save a few hundred lines of JSON handling
over six endpoints. This binary has to vendor cleanly for air-gapped customers.

What `client-go` would give is avoided, not rebuilt:

- no watch with resync - the driver polls on the sweep;
- no server-side apply - it upserts with a JSON merge patch;
- no discovery - the startup probe lists the sandbox resource, which proves at
  once that the API server is there, the CRD is installed and the service
  account may use it.

## The security boundary

A sandbox's API key is in its pod spec. The key is minted for that sandbox,
expires with it and is revoked when it goes. A Secret would be a second object
with the same lifetime and the same value, and one more thing to leak when a
sandbox is terminated uncleanly.

The cost: **anybody who may read pods in the sandbox namespace may read every
live sandbox's key**. So sandboxes get their own namespace, not the gateway's.
The Helm chart creates it, gives the gateway's service account a Role scoped to
it, and puts a default-deny NetworkPolicy over every pod in it.

The Role is small: Sandboxes and, with warm pools, their templates, pools and
claims; read-only on the pods, Services, PVCs and events the controller makes.
Nothing cluster-scoped, no Secrets, no other namespace.

**`pods/portforward` is not granted.** The driver reaches a sandbox by dialling
its pod IP from inside the cluster, which needs no permission. Port-forward
would let the holder reach any pod in the namespace on any port. As a result, a
gateway running _outside_ the cluster cannot serve sandbox attachments.

The egress rule allows DNS, the gateway, and whatever the deployment adds in
`sandboxes.egress.extra`. **The internet is not on that list**, on purpose. A
deployment that needs package downloads points its sandboxes at an internal
proxy and adds it there. That is also the only way this works on an air-gapped
site.

That rule is the only network control, and it is one rule for the whole
namespace. See [What is actually enforced](#what-is-actually-enforced).

## Configuration

| Variable                           | Default                  | What it does                                                                            |
| ---------------------------------- | ------------------------ | --------------------------------------------------------------------------------------- |
| `KEERA_SANDBOX_DRIVER`             | -                        | `kubernetes`, `podman`, or unset for no sandboxes, and no Sandboxes screen in the panel |
| `KEERA_SANDBOXES_FILE`             | -                        | the catalogue, applied on every start                                                   |
| `KEERA_SANDBOX_NAMESPACE`          | the gateway's            | where sandboxes run. Should not be the gateway's - see above                            |
| `KEERA_SANDBOX_RUNTIME_STANDARD`   | -                        | the RuntimeClass for the standard tier, if not the cluster's default                    |
| `KEERA_SANDBOX_RUNTIME_ISOLATED`   | -                        | the gVisor RuntimeClass. Unset makes that tier unavailable                              |
| `KEERA_SANDBOX_RUNTIME_VM`         | -                        | the Kata RuntimeClass. Unset makes that tier unavailable                                |
| `KEERA_SANDBOX_PUBLIC_URL`         | `KEERA_PUBLIC_URL`       | the gateway **as a sandbox reaches it** - the in-cluster Service, not the ingress       |
| `KEERA_SANDBOX_MODEL`              | -                        | the alias a sandbox's agent is pointed at                                               |
| `KEERA_SANDBOX_IDLE_SUSPEND`       | off                      | how long an unattended engineer's sandbox runs before it is suspended. Refused below 5m |
| `KEERA_SANDBOX_STORAGE_CLASS`      | the cluster's            | what a home volume is provisioned from                                                  |
| `KEERA_SANDBOX_SERVICE_ACCOUNT`    | the namespace's          | what a sandbox pod runs as; its token is never mounted                                  |
| `KEERA_SANDBOX_IMAGE_PULL_SECRETS` | -                        | comma-separated, for a sandbox image in a private registry                              |
| `KEERA_SANDBOX_WARM`               | off                      | warm pools, which need the upstream extension                                           |
| `KEERA_SANDBOX_PODMAN_BINARY`      | `podman`                 | the single-host driver's command                                                        |
| `KEERA_SANDBOX_PODMAN_NETWORK`     | podman's default         | the network a single-host sandbox joins                                                 |
| `KEERA_SANDBOX_GIT_FORGE`          | -                        | `github`, `gitlab`, or unset for sandboxes without a repository                         |
| `KEERA_SANDBOX_GIT_URL`            | GitHub.com or GitLab.com | GitHub's API (`https://<host>/api/v3` for Enterprise Server), or the GitLab instance    |
| `KEERA_SANDBOX_GIT_APP_ID`         | -                        | the GitHub App's id                                                                     |
| `KEERA_SANDBOX_GIT_APP_KEY_FILE`   | -                        | a file holding the GitHub App's private key                                             |
| `KEERA_SANDBOX_GIT_TOKEN_FILE`     | -                        | a file holding the GitLab token                                                         |

The four below are only for a gateway that is not a pod in the cluster where it
creates sandboxes. Inside the cluster, the service account's own token and CA
and the `KUBERNETES_SERVICE_*` variables cover all of it.

| Variable                        | Default                             | What it does                                                |
| ------------------------------- | ----------------------------------- | ----------------------------------------------------------- |
| `KEERA_SANDBOX_KUBE_SERVER`     | the in-cluster API server           | its URL                                                     |
| `KEERA_SANDBOX_KUBE_TOKEN_FILE` | the projected service-account token | a bearer token to read instead                              |
| `KEERA_SANDBOX_KUBE_CA_FILE`    | the in-cluster CA bundle            | the CA that signs the API server                            |
| `KEERA_SANDBOX_KUBE_INSECURE`   | off                                 | skip that verification. For a test cluster and nothing else |

`KEERA_SANDBOX_PUBLIC_URL` is the one to get right. A sandbox pointed at the
public name leaves the cluster and comes back through the load balancer to reach
a pod nearby. On the podman driver, `127.0.0.1` inside a sandbox is the sandbox
itself. The host is `host.containers.internal`, and the gateway must listen on
more than loopback to answer there. The default, `KEERA_ADDR=:8080`, already
does.

## The image

`sandbox/Containerfile` builds a base image. A deployment builds its own on top,
with the languages, language servers, internal certificates and agent its
engineers need, and names that image in the catalogue.

The base provides what the gateway depends on: a fixed uid 1000 (the home volume
uses it as `fsGroup`), sshd, the coding agent, and the entrypoint.

It is Alpine, and the coding agent is **Pi**, pinned. Alpine because its Node is
24.18 and Pi needs at least 22.19. Pinned because an image is built once and run
for months, and two sandboxes of the same class must run the same software.

The gateway configures Pi, not the image. The manager renders
`~/.pi/agent/models.json` and the entrypoint writes it.

**It lists every model the sandbox's own key may reach** - the organisation's
allow-list narrowed by the team's - not just one model the deployment picked.
Listing models the key is refused for would end prompts in refusals. Listing
only one would hide the rest. Embedding models are left out.

**The key is not in that file.** The file names `${KEERA_API_KEY}`, which Pi
expands from the environment, so a home volume kept across a suspend never holds
the key.

Three things must match what `keera connect pi` prints for a laptop: the
provider name, the API shape, and the credential being a reference, not a
literal. A test checks them against that template.

A developer who edits their own `models.json` keeps it: the entrypoint rewrites
the file only while it still matches what the gateway last wrote.

**No vendor credential is exported into a sandbox**, so the picker stays honest.
Pi has built-in Anthropic and OpenAI providers wired to those vendors'
endpoints. They turn on when `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or
`OPENAI_API_KEY` is set. Setting any of them fills `/model` with dozens of GPT
and Claude models the key cannot use, and picking one sends the gateway's key
and the prompt to the vendor. Pi's documentation says a provider with no auth
_"loads but stays unavailable in `/model`"_, so with them unset the picker shows
only the organisation's allow-list.

The base URLs stay set, because they are not credentials and Pi ignores them. A
deployment whose image adds Claude Code or an OpenAI SDK sets that client's
variable from `KEERA_API_KEY` in its own entrypoint - one line. Doing so turns
that vendor's provider back on in the picker.

Pi's **default model and provider** are pinned too, in
`~/.pi/agent/settings.json`. The model is `KEERA_SANDBOX_MODEL` if the key may
reach it, otherwise the first model it may reach. The deployment default is one
global setting, but an allow-list is per team.

Pinning `defaultProvider: keera` is a security control. Pi picks its built-in
providers over a configured one when both are available. So `pi` with no
`--model`, on an image that exports a vendor variable, would send the gateway's
key to `api.anthropic.com`. Naming the provider stops that. Any tool in a
sandbox that reads a vendor's variable and ignores its base URL does the same,
and the egress rule is the only general defence. See
[What is actually enforced](#what-is-actually-enforced).

The environment reaches an ssh session through `~/.ssh/environment` and
`PermitUserEnvironment`, not a shell startup file. An ssh session inherits
nothing from the container. Alpine's bash does not read `~/.bashrc` for a
non-interactive `ssh host cmd`, which scp and every editor's remote server use.

The entrypoint prepares the home directory idempotently, writes the gateway's
address into a file every login shell sources, checks out the repository, and
then starts sshd or runs the agent. **sshd starts last**, so the readiness probe
(a TCP connection to its port) also means setup finished.

The host key is generated at build time, so every sandbox from an image shares
it. That is fine here: the connection never crosses a network a stranger can see
(TLS to an authenticated caller, then a pod IP inside the cluster). A
per-sandbox host key would mean an unknown-host prompt on every new sandbox,
which trains people to accept them.

## What is not built, and what to watch

**No sandbox image is published.** It would be a second family of images to
build, scan and ship into air-gapped sites.

**Nothing has run against a real cluster.** The Kubernetes driver is tested
against a stand-in API server that answers the shapes the CRD documents. Most
likely to need a second look: the warm-pool claim's condition names, and whether
`envVarsInjectionPolicy: Overrides` behaves as documented.

**agent-sandbox is v1beta1 and still changing.** Two of its own notes matter
here: the `Suspended` condition is not cleared on resume, so trust `Ready`; and
`volumeClaimTemplates` cannot change after creation, so a class that grows its
disk cannot apply that to running sandboxes.

**Kata needs nested virtualisation** on a VM-based node pool, which some
platform teams will not allow.

**Warm pools cost money all the time.** A pool of two per class on a deployment
nobody uses is the most likely way for this feature to waste money.

**Keep GPU nodes and sandbox nodes in separate pools**, so a developer filling a
node with build jobs does not take Keera Engine's GPU. The chart does not
enforce this; set `nodeSelector` on both.
