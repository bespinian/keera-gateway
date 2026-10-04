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
creation, scoped to whoever asked for it. The key expires with the sandbox,
moves with it when the sandbox is extended, and is revoked when it ends or
fails. The sandbox boots already pointed at the gateway, so
`keera connect` is not needed inside.

**A session is known, not guessed.** [sessions.md](sessions.md) groups requests
into tasks by inference, which can be wrong in two ways. An agent sandbox is
exactly one task, so it sends its own id in `X-Keera-Session`: the gateway
configures Pi and Claude Code to do so. The session key
is computed once at creation, the same way the gateway computes it per request,
so a sandbox and its task can be read side by side.

## Two lifecycles, one substrate

|                  | Engineer                                               | Agent                              |
| ---------------- | ------------------------------------------------------ | ---------------------------------- |
| Default lifetime | the class's; 4h if it sets none                        | the class's; 4h if it sets none    |
| When it expires  | suspended, key revoked, resumable                      | terminated                         |
| When it is idle  | suspended after `IDLE_SUSPEND` with no connection open | never - it has nothing to wait for |
| Session header   | not set                                                | `X-Keera-Session: <sandbox id>`    |
| Attaching        | ssh, from its owner or an operator                     | nothing attaches                   |
| How work leaves  | the developer pushes                                   | a branch, pushed by the agent      |

An agent sandbox's work is already pushed as a branch, so nothing in it is worth
keeping. An engineer's sandbox holds unfinished work, so it is kept.

**An expired engineer sandbox keeps its volume and loses its access.** Its key
and repository credential are revoked. It still counts towards the quota and
keeps its name, because its volume still takes up space. `keera sandbox resume`
brings it back with a new key, a new repository credential and the class's
default lifetime, under the guardrails as they are now. The same ssh keys let
you in. `resume` takes no
`--ttl`: `extend` the sandbox once it is back. `extend` does not work while it
is expired. `terminate` frees the volume.

**A sandbox that fails is over.** Nothing resumes it. Its key and repository
credential are revoked at once. An agent's sandbox is also removed. An
engineer's keeps its volume, in case it holds work, until somebody terminates
it. Until then it is still live, like an expired one: it counts towards the
quota, keeps its name and shows in `keera sandbox ls`.

A sandbox whose process exits by itself, with success, is not a failure on
podman: it shows as suspended, because podman can start it again. On Kubernetes
the pod is finished and cannot be restarted, so it has failed. Either way, an
agent's sandbox that exits cleanly has done its task and is removed.

**Idle means no connection.** With `KEERA_SANDBOX_IDLE_SUSPEND` set, an
engineer's sandbox is suspended once no connection has been open to it for that
long. An open `keera sandbox ssh` or editor session counts, even if nobody
types. The clock starts again when a sandbox is resumed.

**Nothing attaches to an agent sandbox.** A shell would not be less safe - the
isolation is the same. The point is that only a branch comes out of it.

**What creating one needs.** A name of lowercase letters, digits and interior
hyphens, at most 40 characters, because it becomes a hostname in the cluster.
An engineer's sandbox needs an ssh public key: `keera sandbox create` sends the
ones in `~/.ssh`, or the one `--ssh-key` names. An agent's needs both `--repo`
and `--task`. Through the API, `env` adds variables for the first start. It
cannot name a `KEERA_` variable: those are the gateway's.

## Agent-in-sandbox, not sandbox-as-a-tool

The upstream SDK is built for an agent outside that drives a sandbox through
`Run()` and `Files().Write()`. That sends the source code out through the API on
every read.

In Keera the agent runs _inside_.
`keera sandbox agent <name> --class … --repo … --task …` starts a machine,
checks out the repository, runs the image's coding agent on the task (Pi in the
base image), and pushes a branch. Where egress is enforced, only that branch
leaves; see [What is actually enforced](#what-is-actually-enforced). When the
agent exits cleanly, the sandbox is terminated. When it fails, its machine is
removed too, and the sandbox is marked failed with the reason.

## Isolation

The upstream controller has no isolation of its own; it uses a `RuntimeClass`.
So the tier is set per class. The catalogue names the tier, and the deployment
maps each of the three tiers to a runtime name once: a RuntimeClass on
Kubernetes, an OCI runtime on podman.

| Tier       | Runtime                                  | Boundary                        | Cost                                                                    |
| ---------- | ---------------------------------------- | ------------------------------- | ----------------------------------------------------------------------- |
| `standard` | the cluster's own; on podman, the host's | the host kernel, as for any pod | none                                                                    |
| `isolated` | gVisor (`runsc`)                         | syscalls served in userspace    | some syscall-heavy work; no `io_uring`, some FUSE, no nested containers |
| `vm`       | Kata; on podman, `krun`                  | **a kernel of its own**         | 1–2s of boot, about 100 MB per sandbox, hardware virtualisation         |

`vm` is the best tier for an agent sandbox, since it runs code a model wrote.

**It is not the default, because of hardware.** Kata and krun need hardware
virtualisation on the node. When the nodes are VMs themselves - VMware, Nutanix,
most Swiss enterprise setups - that means nested virtualisation, which a
platform team may not enable. So gVisor is a full tier, not a fallback.

A class that asks for a tier the deployment has not mapped is **refused at
creation**, with the name of the setting to fix. It never runs at a weaker tier.

Every tier runs as a non-root user, with `no-new-privileges` and all
capabilities dropped. On Kubernetes, no service account token is mounted and
seccomp is `RuntimeDefault`. Podman uses its own default seccomp profile. The
root filesystem stays writable, because people work in a sandbox.

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
dials it, and a developer goes through the ProxyCommand. The route accepts any
port from 1024 to 65535. On podman only 2222 is published, so no other port can
be reached.

```sh
keera sandbox create fix-login --class standard --repo git@internal:team/service.git
keera sandbox ssh fix-login
keera sandbox config >> ~/.ssh/config   # then: ssh sbx-fix-login, or open it in any editor
```

`keera sandbox config` writes a `ProxyCommand` that runs `keera sandbox proxy`.
It speaks the upgrade above and pipes it to its own stdin and stdout. The
command names the gateway it was made for with `--url`, so ssh always reaches
the sandbox through that gateway.

**What this needs from a proxy in front.** The upgrade must survive whatever
publishes the gateway. nginx-ingress and Traefik pass `Connection: Upgrade` by
default. If a proxy strips it, `keera sandbox ssh` gets a 426. The message asks
for the `Upgrade` header and names `keera sandbox ssh` as the client, so a 426
from the command itself points at the proxy.

## Who may do what

|                            | See it | Terminate, suspend, resume, extend | Open a shell |
| -------------------------- | ------ | ---------------------------------- | ------------ |
| Operator                   | yes    | yes                                | yes          |
| Organisation administrator | yes    | yes                                | **no**       |
| The owner                  | yes    | yes                                | yes          |
| Anybody else               | no     | no                                 | no           |

An agent's sandbox runs its task to the end: it cannot be suspended, and once
it has expired it cannot be resumed.

An administrator can see and terminate every sandbox in their organisation,
because the quota and the bill are theirs. They cannot get inside: a sandbox
holds someone's source code and what they typed into a terminal. An
administrator who needs access can create their own sandbox on the same
repository.

A sandbox the caller may not see answers 404, not 403. And a name is unique per
person, not per organisation, so creating one never fails because a colleague
used the name. So nobody can use this to find out whether a colleague has a
sandbox called `acquisition-model`.

A name finds the caller's own sandbox first. An administrator who can see two
sandboxes of that name, from two people, gets a 409 and names one by its id
(`sbx_…`).

## The catalogue

A class name is an API contract. People type it, commit it into repository
config and get used to it. So an administrator can change its image, isolation
tier or memory without anyone else editing anything.

Each class belongs to one organisation, like a model. Only its people and
agents can ask for it, and two organisations can each have a `standard`.

```yaml
# KEERA_SANDBOXES_FILE
sandboxes:
  - name: standard
    description: The repo toolchain, an editor server, an agent.
    image: registry.internal/keera/sandbox-base:1
    isolation: isolated # standard | isolated | vm
    cpu: "4" # or 4000m
    memory: 16Gi
    disk: 50Gi # empty keeps nothing across a suspend (podman always keeps home)
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

The file is a template, like the model file. A new organisation starts with a
copy of each class it declares. The copies are the organisation's own: its
administrators can change or remove them. Changing the file later does not touch
organisations that already exist.

An administrator adds or changes classes with a file in the same format:

```sh
keera sandbox apply sandboxes.yaml
```

It checks the whole file first, then sets each class it names. Classes the file
does not name are left alone. A class is removed in the panel, or with
`keera sandbox delete-class <name>`. Sandboxes already running on it keep
working. An administrator's commands use their own organisation. An operator
adds `--org <id>` when there is more than one.

A field left out gets a default:

| Field         | Default    | Allowed                               |
| ------------- | ---------- | ------------------------------------- |
| `isolation`   | `isolated` | `standard`, `isolated`, `vm`          |
| `cpu`         | 2 cores    | more than 0                           |
| `memory`      | 4Gi        | more than 0                           |
| `disk`        | none       | none, or more than 0                  |
| `default_ttl` | 4h         | 5m to 28 days                         |
| `max_ttl`     | 24h        | 5m to 28 days, at least `default_ttl` |
| `warm`        | 0          | 0 to 32                               |

**The organisation controls these, not the operator.** Its administrators
choose each class's isolation tier, size, lifetimes and warm pool. That
includes `standard`, which shares the host's kernel with other organisations'
sandboxes on the same nodes, and warm pools, which hold CPU and memory whether
anyone uses them or not. The operator only decides which stronger tiers exist
(`KEERA_SANDBOX_RUNTIME_*`) and whether warm pools are on at all
(`KEERA_SANDBOX_WARM`).

On Kubernetes, a class with no `disk` gets an 8Gi scratch directory
(`emptyDir`) that does not survive a suspend.

Deleting a class does not break running sandboxes. Each one keeps its own copy
of the image, the isolation tier and the resources it was given. The class's
lifetimes are gone with it, so it gets those of a class that sets none: 4h when
resumed or extended without `--ttl`, and at most 24h. The guardrail still
applies.

## What is actually enforced

|                     | Kubernetes driver                                       | podman driver |
| ------------------- | ------------------------------------------------------- | ------------- |
| Network isolation   | a default-deny NetworkPolicy over the sandbox namespace | **none**      |
| Where it comes from | the Helm chart, `networkPolicy.enabled`                 | -             |
| Granularity         | one rule for the whole namespace                        | -             |

**A podman sandbox has unrestricted outbound access.** It joins podman's default
bridge and can reach the internet, the host and the LAN. This is tested:
`curl https://github.com` from inside one returns 200. Use the single-host
driver for development, or on a host the deployment already trusts.

**The Kubernetes rule covers the whole namespace and needs a CNI that enforces
it.** On a network plugin without NetworkPolicy support - flannel without a
policy plugin, for example - the policy does nothing, and nothing warns you.
Check it on the cluster.

## Quotas

Sandbox quotas are fields on the guardrail row. They combine by the usual
restrict-only rule: the minimum for a ceiling, the intersection for a list.

```sh
keera guardrail set project <project-id> \
  --max-sandboxes 6 \
  --max-sandbox-ttl 8h \
  --sandbox-classes standard,small \
  --max-sandbox-cpu 8 \
  --max-sandbox-memory 32768
```

`--max-sandboxes` counts every live sandbox: starting, running, suspended,
expired, and an engineer's that failed. The last two still hold a volume.

A size ceiling **refuses** a request; it does not quietly downgrade it. A scope
capped at four cores that asks for an eight-core class gets an error, not a
smaller machine that cannot build its code.

**Extending is safe to allow.** The new lifetime counts from now, not from what
is left, and the same two ceilings as at creation apply. The sandbox's key
gets the new expiry too. A developer can extend
as often as needed, and the sandbox still cannot live forever.

Quotas attach to the organisation and the project. The organisation's own quota
is set by its administrators, like its classes. Only `allowed_repos` on the
organisation is set by an operator. There is no per-key sandbox
quota: a sandbox's key is minted _for_ that sandbox and dies with it. A key's
guardrail refuses the sandbox fields.

**Only the command line sets them.** The panel's guardrails dialog has no
sandbox fields. The panel's Sandboxes screen shows what is left of the quota and
refuses a class the quota forbids. Saving the guardrails dialog keeps the
sandbox fields unchanged: a guardrail is written whole, and the dialog only
changes the fields it shows.

## Naming a project

`keera sandbox create <name> --project <project>`, or the Project field in the panel's
dialog. The sandbox's key is scoped to that project, which decides:

- which **budget** the agent's inference is charged to,
- which **rate limit** it uses,
- whose **sandbox quota** the machine counts against,
- which **system prompt** every request it makes carries,
- and **which models** its agent may reach, which is what the picker inside the
  sandbox shows.

Without a project, the sandbox goes in the organisation's oldest project.

**The project must belong to the same organisation.** The foreign key only checks
that the project exists, so the control plane checks the owner. Otherwise a sandbox
could use another organisation's system prompt, budget, rate limit and model
allow-list.

Only an administrator may choose a project. A member's sandbox goes in the
organisation's oldest project, just as a member cannot choose the project of a
key.

## What it costs

Sandbox time is recorded beside tokens, so a task's cost includes the machine it
ran on, such as "eleven minutes of a four-core machine".

```sh
keera sandbox usage --by project --since 720h
keera sandbox usage --by user
keera sandbox usage --by class
```

Only an administrator can read it. The report counts sandboxes, how many of
them are live now, how long they ran, and core-seconds, because no single number
tells the whole story. A sandbox is counted whole in the window it was created
in. Forty
sandboxes that each lived ninety seconds is an agent fleet; one that lived a
week is somebody who forgot. Core-seconds is what a chargeback uses.

Running time is summed across runs, not derived from timestamps, because a
sandbox can be suspended and resumed many times. The clock stops as soon as a
sandbox is suspended or terminated, not at the next sweep. A podman sandbox
that stops by itself is only noticed at the next sweep, and is charged until
then.

If the gateway was down, the time it could not observe is still charged,
because the sandbox was running.

Ended sandboxes are deleted after `KEERA_USAGE_RETENTION`, so `--since` reaches
back no further than that.

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

**Only the repositories the guardrail allows.** The deployment's forge
credential can reach every repository it is installed on, including other
organisations'. So a sandbox gets a token only for a repository in
`allowed_repos`, and one whose organisation does not set it gets none:

```sh
keera guardrail set org <org-id> --repos acme            # everything under acme/
keera guardrail set project <project-id> --repos acme/service  # narrows it for one project
keera guardrail set org <org-id> --repos '*'             # any repository, for one organisation
```

An entry is a path on the forge: an owner or group for everything under it, or
one repository. Case does not matter. Only the organisation grants
repositories, and only an operator can set its list. A project's list can only
narrow it, and an administrator can set that. It is checked again on every
token refresh, so taking a repository off the list stops running sandboxes
getting a new token.

**Tokens are refreshed.** A GitHub token lasts an hour; an engineer's sandbox
lasts a working day. Git in the sandbox asks `git-credential-keera`, which keeps
the current token. When it has five minutes left, the helper gets a new one from
`POST /sandbox/v1/git-credential` with the sandbox's own key. That route takes
10 requests a minute per client address, and sandboxes that reach the gateway
from the same address share it. The gateway revokes the replaced token where
the forge allows it. The helper only answers
for the repository's own host, so a submodule on another host never sees the
token. A resumed sandbox keeps the token the helper last got, unless the gateway
handed it a new one, as it does when an expired sandbox is resumed.

**When the sandbox ends, so does access.** Its key is revoked, so it cannot get
another token. A GitLab token is revoked at once. A GitHub token cannot be
revoked without the token itself, which is not kept, so the last one works for
up to an hour more.

**When the owner is disabled** (see [sso.md](sso.md#when-someone-leaves)), their
agent sandboxes and any that have not started yet are terminated, their other
engineer sandboxes are suspended, and every repository credential they held is
revoked.

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

**Out.** Push a branch. For an agent sandbox that is the only egress, where
egress is [enforced](#what-is-actually-enforced). The agent
runner never writes to a default branch: its output is a proposal, and a person
opens it.

## Speed, and what it costs to have

Cold start takes 3–8 seconds on a node that already has the image, a minute or
more on one that does not, plus a second or two for a microVM.

Warm pools fix this. They are opt-in twice - `KEERA_SANDBOX_WARM` for the
deployment, `warm:` per class - because every warm sandbox holds its class's
full CPU and memory all the time, used by nobody. The operator sets the first,
and each organisation's administrators set the second.

Only an engineer's sandbox comes from a pool. A pool member is started before
anybody asks, as an engineer's sandbox that is ready once sshd answers. An agent
runs no sshd, so an agent's sandbox always starts cold.

They need the upstream warm-pool extension (`extensions.agents.x-k8s.io/v1beta1`:
`SandboxTemplate`, `SandboxWarmPool`, `SandboxClaim`) installed next to the
controller. The pool's update strategy is `OnReplenish`, not `Recreate`, so
changing a class in the afternoon does not empty the pool while people use it.
The downside: for a few minutes after a change, some sandboxes get the previous
image. A sandbox records its class's image when it is created, so during those
minutes the recorded image can be the new one while the pod still runs the old.

Whether a sandbox came from a pool is stored on its row, not derived from
configuration. So a deployment that turns warm pools off still knows which
sandboxes came from one.

Pools do not outlive their class. The sweep deletes the pool and template of a
class the organisation removed or set back to `warm: 0`, and a gateway started
with warm pools off deletes every pool it finds. Both need `list` on templates and pools.

## The two drivers

|                    | `kubernetes`                                      | `podman`                           |
| ------------------ | ------------------------------------------------- | ---------------------------------- |
| Where              | a cluster, via agent-sandbox                      | one host                           |
| Isolation          | whatever RuntimeClasses are mapped                | whatever OCI runtimes the host has |
| Expiry enforced by | the controller                                    | **the gateway**                    |
| Network isolation  | a namespace NetworkPolicy, if the CNI enforces it | **none**                           |
| Warm pools         | with the extension                                | no                                 |
| Suspend/resume     | yes                                               | yes (`stop`/`start`)               |
| Volumes            | a PVC, or an emptyDir with no `disk`              | a named volume, always, unsized    |

Expiry is the real difference. On Kubernetes it is `spec.shutdownTime` and the
cluster enforces it, so **a gateway that crashes and never comes back leaves no
sandboxes running.** An engineer's sandbox keeps its volume, as it does when it
expires. On podman there is no controller, so the gateway's sweep
enforces it, from the expiry on the sandbox's row. A gateway that is down over a weekend comes back to containers that
should have ended on Friday. The sweep that runs at start-up ends those.

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
would let the holder reach any pod in the namespace on any port. So the gateway
has to run in the cluster where it creates sandboxes.

The egress rule allows DNS, the gateway, and whatever the deployment adds in
`sandboxes.egress.extra`. **The internet is not on that list**, on purpose.
Cloning and pushing need the forge, so add its host there. A deployment that
needs package downloads points its sandboxes at an internal proxy and adds it
there too. That is also the only way this works on an air-gapped
site.

That rule is the only network control, and it is one rule for the whole
namespace. See [What is actually enforced](#what-is-actually-enforced).

## Configuration

| Variable                           | Default                                       | What it does                                                                                         |
| ---------------------------------- | --------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `KEERA_SANDBOX_DRIVER`             | -                                             | `kubernetes`, `podman`, or unset for no sandboxes, and no Sandboxes screen in the panel              |
| `KEERA_SANDBOXES_FILE`             | -                                             | the classes each new organisation starts with                                                        |
| `KEERA_SANDBOX_NAMESPACE`          | the gateway's, or `default` outside a cluster | where sandboxes run on Kubernetes. Should not be the gateway's - see above                           |
| `KEERA_SANDBOX_RUNTIME_STANDARD`   | -                                             | the runtime for the standard tier, if not the default: a RuntimeClass, or on podman `crun` or `runc` |
| `KEERA_SANDBOX_RUNTIME_ISOLATED`   | -                                             | the gVisor RuntimeClass, or `runsc` on podman. Unset makes that tier unavailable                     |
| `KEERA_SANDBOX_RUNTIME_VM`         | -                                             | the Kata RuntimeClass, or `krun` on podman. Unset makes that tier unavailable                        |
| `KEERA_SANDBOX_PUBLIC_URL`         | `KEERA_PUBLIC_URL`                            | the gateway **as a sandbox reaches it** - the in-cluster Service, not the ingress                    |
| `KEERA_SANDBOX_IDLE_SUSPEND`       | off                                           | how long an engineer's sandbox runs with no connection open before it is suspended. Refused below 5m |
| `KEERA_SANDBOX_STORAGE_CLASS`      | the cluster's                                 | what a home volume is provisioned from                                                               |
| `KEERA_SANDBOX_SERVICE_ACCOUNT`    | the namespace's                               | what a sandbox pod runs as; its token is never mounted                                               |
| `KEERA_SANDBOX_IMAGE_PULL_SECRETS` | -                                             | comma-separated, for a sandbox image in a private registry                                           |
| `KEERA_SANDBOX_WARM`               | off                                           | warm pools, which need the upstream extension                                                        |
| `KEERA_SANDBOX_PODMAN_BINARY`      | `podman`                                      | the single-host driver's command                                                                     |
| `KEERA_SANDBOX_PODMAN_NETWORK`     | podman's default                              | the network a single-host sandbox joins                                                              |
| `KEERA_SANDBOX_GIT_FORGE`          | -                                             | `github`, `gitlab`, or unset for sandboxes without a repository                                      |
| `KEERA_SANDBOX_GIT_URL`            | GitHub.com or GitLab.com                      | GitHub's API (`https://<host>/api/v3` for Enterprise Server), or the GitLab instance                 |
| `KEERA_SANDBOX_GIT_APP_ID`         | -                                             | the GitHub App's id                                                                                  |
| `KEERA_SANDBOX_GIT_APP_KEY_FILE`   | -                                             | a file holding the GitHub App's private key                                                          |
| `KEERA_SANDBOX_GIT_TOKEN_FILE`     | -                                             | a file holding the GitLab token                                                                      |

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
allow-list narrowed by the project's - not just one model the deployment picked.
Listing models the key is refused for would end prompts in refusals. Listing
only one would hide the rest. Embedding models are left out.

**The key is not in that file.** The file names `${KEERA_API_KEY}`, which Pi
expands from the environment.

**The home volume does hold the key.** The entrypoint writes the environment,
key included, to `~/.ssh/environment`, because that is how an ssh session gets
it. The Git helper caches the repository token in `~/.config/keera`. So whoever
can read a sandbox's volume can read both, until they are revoked: when the
sandbox expires or ends, or its owner is disabled. A GitHub token revoked this
way still works for up to an hour.

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
`~/.pi/agent/settings.json`. The model is the first chat model the key may
reach, so the project's allow-list decides it.

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
The file holds the container's whole environment, with the image's `PATH` and
any extra variables the caller set. Left out are `TERM`, values with a line
break, and what only the start uses: the task, the ssh keys, Pi's files and the
first repository token.

The entrypoint prepares the home directory idempotently, writes the gateway's
address into a file every login shell sources, checks out the repository, and
then starts sshd or runs the agent. **sshd starts last**, so a sandbox is ready
only once sshd answers, which also means setup finished. Both drivers check
this: Kubernetes with a readiness probe on the port, podman by connecting to it
from inside the container. An agent sandbox runs no sshd and has no such check:
it is ready once it has started.

The host key is generated at build time, so every sandbox from an image shares
it. It proves nothing about which sandbox you reach, and it does not need to:
the gateway has already authenticated you over TLS and dials the sandbox
itself. So `keera sandbox ssh` and `keera sandbox config` turn host-key
checking off, and nobody learns to click through an unknown-host prompt.

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
