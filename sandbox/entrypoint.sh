#!/usr/bin/env bash
# The first thing that runs inside a Keera sandbox.
#
# It prepares the home directory, checks out the repository if there is one,
# points whichever coding agent is installed at the gateway, and then either
# starts sshd - for a machine somebody works in - or runs the agent on its task
# and stops.
#
# Everything it needs arrives in the environment, put there by the gateway when
# the sandbox was created. Nothing here reaches out to ask anybody anything,
# which is what lets a sandbox come up in an air-gapped cluster.
set -euo pipefail

log() { printf '%s keera-sandbox: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

: "${HOME:=/home/keera}"
: "${KEERA_SANDBOX_PURPOSE:=engineer}"

# ---------------------------------------------------------------- the home
#
# The home directory is a volume that survives a suspend, so everything here is
# written idempotently: the second start of a sandbox finds most of it already
# done, and must not clobber what its owner has since changed.
mkdir -p "$HOME/.ssh" "$HOME/.config" "$HOME/work"
chmod 700 "$HOME/.ssh"

# The keys that may open a shell in here.
#
# The connection has already been authenticated by the gateway before a byte
# reaches this port - the attach surface checks who the caller is and whether
# the sandbox is theirs - so this is the second lock rather than the only one.
# It is worth having: a pod IP inside a cluster is reachable by anything the
# network policy admits, and "the gateway is the only thing that can reach it"
# should not be the whole of the argument.
if [ -n "${KEERA_AUTHORIZED_KEYS:-}" ]; then
  printf '%s\n' "$KEERA_AUTHORIZED_KEYS" > "$HOME/.ssh/authorized_keys"
  chmod 600 "$HOME/.ssh/authorized_keys"
fi

# ------------------------------------------------------------- the gateway
#
# Written to a file as well as being in the environment, because an ssh session
# does not inherit the pod's environment: a developer who opens a shell would
# otherwise find none of this set, and their agent would have no idea where the
# gateway is.
# Everything the gateway set that a person or an agent in here needs.
#
# It has to be the whole set rather than the interesting half: a variable that is
# present in the container's environment and absent from a shell is worse than
# one missing from both, because a script written against it works when the
# entrypoint runs it and fails when somebody runs it by hand.
#
# It is written once, in sshd's own format, and everything else is derived from
# that file. ~/.ssh/environment is what makes these reach a session at all:
# an ssh session inherits nothing from the container, and the shell startup
# files cannot be relied on - a non-interactive `ssh host cmd`, which is what
# scp and every editor's remote server use, reads ~/.bashrc only on some builds
# of bash and not on Alpine's. sshd reads this for every session, whatever the
# shell and whatever the kind. See PermitUserEnvironment in sshd_config.
#
# The format is strict: NAME=value, one per line, no quoting and no expansion.
cat > "$HOME/.ssh/environment" <<PROFILE
KEERA_SANDBOX_ID=${KEERA_SANDBOX_ID:-}
KEERA_SANDBOX_NAME=${KEERA_SANDBOX_NAME:-}
KEERA_SANDBOX_CLASS=${KEERA_SANDBOX_CLASS:-}
KEERA_SANDBOX_PURPOSE=${KEERA_SANDBOX_PURPOSE:-}
KEERA_SANDBOX_EXPIRES=${KEERA_SANDBOX_EXPIRES:-}
KEERA_BASE_URL=${KEERA_BASE_URL:-}
KEERA_API_KEY=${KEERA_API_KEY:-}
KEERA_MODEL=${KEERA_MODEL:-}
KEERA_SESSION=${KEERA_SESSION:-}
KEERA_REPO=${KEERA_REPO:-}
KEERA_BRANCH=${KEERA_BRANCH:-}
KEERA_GIT_CREDENTIAL_URL=${KEERA_GIT_CREDENTIAL_URL:-}
OPENAI_BASE_URL=${OPENAI_BASE_URL:-}
ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL:-}
ANTHROPIC_CUSTOM_HEADERS=${ANTHROPIC_CUSTOM_HEADERS:-}
ANTHROPIC_MODEL=${ANTHROPIC_MODEL:-}
PROFILE
chmod 600 "$HOME/.ssh/environment"

# The same set as something a person can source, for a shell that did not come
# through sshd - `podman exec`, a script, the agent runner. Derived from the one
# file above rather than written twice, because two lists drift.
cat > "$HOME/.keera-env" <<'PROFILE'
# Written by keera-sandbox at start, from ~/.ssh/environment. Sourced by the
# shell startup files below; ssh sessions already have these from sshd.
[ -f "$HOME/.ssh/environment" ] && set -a && . "$HOME/.ssh/environment" && set +a
PROFILE
chmod 600 "$HOME/.keera-env"
for rc in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.profile"; do
  touch "$rc"
  grep -qF '.keera-env' "$rc" || echo '[ -f "$HOME/.keera-env" ] && . "$HOME/.keera-env"' >> "$rc"
done

# A banner, because the single most useful thing to tell somebody who has just
# opened a shell in an ephemeral machine is when it goes away.
cat > "$HOME/.keera-motd" <<MOTD
  ${KEERA_SANDBOX_NAME:-sandbox}  (${KEERA_SANDBOX_CLASS:-unknown class})
  expires ${KEERA_SANDBOX_EXPIRES:-unknown} - 'keera sandbox extend ${KEERA_SANDBOX_NAME:-}' from your laptop
  /home/keera survives a suspend. Nothing outside it does.

  pi is configured for the models this organisation may use, and for no
  others. No vendor credential is exported: setting one would fill pi's
  model picker with that vendor's whole catalogue and send this key to them.
  For a script that wants the OpenAI SDK's own variable, one line:
      export OPENAI_API_KEY=\$KEERA_API_KEY  OPENAI_BASE_URL=\$KEERA_BASE_URL/v1
MOTD
grep -qF '.keera-motd' "$HOME/.bashrc" || \
  echo '[ -n "$PS1" ] && [ -f "$HOME/.keera-motd" ] && cat "$HOME/.keera-motd"' >> "$HOME/.bashrc"

# ------------------------------------------------------------- the agent's config
#
# Pi reads ~/.pi/agent/models.json, and the gateway renders it: one deployment,
# one address, one model, written by the same package that prints the block
# `keera connect pi` gives somebody for their laptop.
#
# The credential is not in the file. The rendered config names ${KEERA_API_KEY}
# and Pi expands it from the environment, so the key lives in exactly one place
# and a home volume that outlives a suspend never has a copy of it.
#
# Written unless somebody has changed it. The home directory survives a suspend,
# so a developer who tuned their own models.json would otherwise find it
# replaced every time their sandbox came back - and losing somebody's
# configuration to a restart is the kind of thing that stops people trusting a
# machine. The copy beside it is how "unchanged" is decided.
# keep_ours writes a file the gateway rendered, unless somebody has changed it.
#
# The home directory survives a suspend, so a developer who tuned one of these
# would otherwise find it replaced every time their sandbox came back - and
# losing somebody's configuration to a restart is the kind of thing that stops
# people trusting a machine. The copy beside it is how "unchanged" is decided.
keep_ours() {
  local live="$1" content="$2" what="$3"
  local ours="${live%/*}/.keera-$(basename "$live")"
  if [ ! -f "$live" ] || { [ -f "$ours" ] && cmp -s "$ours" "$live"; }; then
    printf '%s\n' "$content" > "$live"
    cp "$live" "$ours"
    log "wrote $what"
  else
    log "leaving your own $live alone"
  fi
}

# What pi is configured with when the gateway rendered nothing - because no
# KEERA_SANDBOX_MODEL is set, or the alias it names is not in the catalogue.
#
# Written verbatim, with one substitution: the base URL. localhost inside a
# container is the container, so a sandbox configured for localhost:8080 finds
# nothing listening; the gateway passes its own address in KEERA_BASE_URL and
# that is used when it is there. Everything else - the provider name, the api
# type, and the key as ${KEERA_API_KEY} for pi to expand from the environment -
# is as written.
pi_fallback() {
  cat <<FALLBACK
{
  "providers": {
    "keera": {
      "baseUrl": "${KEERA_BASE_URL:-http://localhost:8080/api}/v1",
      "api": "openai-completions",
      "apiKey": "\${KEERA_API_KEY}",
      "models": [
        {
          "id": "${KEERA_MODEL:-keera-speed}",
          "name": "Keera Speed",
          "contextWindow": 16384
        }
      ]
    }
  }
}
FALLBACK
}

: "${KEERA_PI_CONFIG:=$(pi_fallback)}"
: "${KEERA_PI_SETTINGS:={\"defaultProvider\":\"keera\",\"defaultModel\":\"${KEERA_MODEL:-keera-speed}\"}}"

if [ -n "${KEERA_PI_CONFIG:-}" ]; then
  mkdir -p "$HOME/.pi/agent"
  keep_ours "$HOME/.pi/agent/models.json" "$KEERA_PI_CONFIG" \
    "pi's model configuration for ${KEERA_MODEL:-this deployment}"
  # Which provider pi starts on. Without it a bare `pi` falls through to a
  # built-in provider, finds ANTHROPIC_API_KEY in the environment - which is
  # this gateway's key, set for Claude Code - and sends it to api.anthropic.com.
  if [ -n "${KEERA_PI_SETTINGS:-}" ]; then
    keep_ours "$HOME/.pi/agent/settings.json" "$KEERA_PI_SETTINGS" \
      "pi's default provider"
  fi
fi

# ------------------------------------------------------------------- git
#
# The credential is this sandbox's own: one repository, expiring no later than
# the sandbox does, minted by the gateway when it was created. It is never the
# developer's own credential and there is no forwarded ssh agent, which is the
# whole point - this machine may be about to run a model's output.
setup_git() {
  git config --global init.defaultBranch main
  git config --global advice.detachedHead false
  git config --global safe.directory '*'
  if [ -n "${KEERA_GIT_AUTHOR_NAME:-}" ]; then
    git config --global user.name "$KEERA_GIT_AUTHOR_NAME"
  fi
  if [ -n "${KEERA_GIT_AUTHOR_EMAIL:-}" ]; then
    git config --global user.email "$KEERA_GIT_AUTHOR_EMAIL"
  fi

  if [ -n "${KEERA_GIT_SSH_KEY:-}" ]; then
    printf '%s\n' "$KEERA_GIT_SSH_KEY" > "$HOME/.ssh/keera_git"
    chmod 600 "$HOME/.ssh/keera_git"
    git config --global core.sshCommand \
      "ssh -i $HOME/.ssh/keera_git -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
    return
  fi
  if [ -n "${KEERA_GIT_TOKEN:-}" ]; then
    # A helper rather than an https://user:token@host remote, so the token is
    # not in .git/config where every later `git remote -v` would print it - and
    # not in the process list of whatever ran the clone. The helper also gets
    # a fresh token from the gateway when this one runs out.
    git config --global --unset-all credential.helper || true
    git config --global credential.helper keera
    rm -f "$HOME/.git-credentials"
    local cache="$HOME/.config/keera/git-credential" cached=""
    mkdir -p "${cache%/*}"
    [ -f "$cache" ] && cached="$(sed -n 's/^password_expiry_utc=//p' "$cache")"
    # A resumed sandbox starts with the token it was created with. The helper
    # may have fetched a newer one since, which is kept.
    if [ ! -s "$cache" ] || [ "${cached:-0}" -lt "${KEERA_GIT_TOKEN_EXPIRES:-0}" ]; then
      {
        printf 'username=%s\npassword=%s\n' "${KEERA_GIT_USERNAME:-keera}" "$KEERA_GIT_TOKEN"
        if [ -n "${KEERA_GIT_TOKEN_EXPIRES:-}" ]; then
          printf 'password_expiry_utc=%s\n' "$KEERA_GIT_TOKEN_EXPIRES"
        fi
      } > "$cache"
      chmod 600 "$cache"
    fi
  fi
}

checkout() {
  [ -n "${KEERA_REPO:-}" ] || return 0
  local dir="$HOME/work/$(basename "${KEERA_REPO%.git}")"
  if [ -d "$dir/.git" ]; then
    log "repository already checked out at $dir"
    echo "$dir"
    return 0
  fi
  log "cloning ${KEERA_REPO}"
  # A shallow clone by default. An agent working on one task needs the tip and
  # not five years of history, and on a large repository the difference is
  # minutes of somebody's time every single sandbox. KEERA_GIT_DEPTH=0 asks for
  # the whole history, which is what a bisect or a blame-heavy task wants.
  local depth=()
  case "${KEERA_GIT_DEPTH:-1}" in
    0) ;;
    *) depth=(--depth "${KEERA_GIT_DEPTH:-1}") ;;
  esac
  if [ -n "${KEERA_BRANCH:-}" ]; then
    git clone "${depth[@]}" --branch "$KEERA_BRANCH" "$KEERA_REPO" "$dir"
  else
    git clone "${depth[@]}" "$KEERA_REPO" "$dir"
  fi
  echo "$dir"
}

setup_git
WORKDIR="$(checkout || true)"
if [ -n "${WORKDIR:-}" ]; then
  ln -sfn "$WORKDIR" "$HOME/repo"
  echo "cd $HOME/repo" > "$HOME/.keera-cd"
  grep -qF '.keera-cd' "$HOME/.bashrc" || \
    echo '[ -n "$PS1" ] && [ -f "$HOME/.keera-cd" ] && . "$HOME/.keera-cd"' >> "$HOME/.bashrc"
fi

# ---------------------------------------------------------------- the agent
#
# An agent sandbox runs its task and stops. Nothing attaches to it, so sshd is
# not started at all: the pod's readiness is the agent having started, and its
# exit is the sandbox's.
if [ "$KEERA_SANDBOX_PURPOSE" = "agent" ]; then
  [ -n "${KEERA_TASK:-}" ] || die "this is an agent sandbox and KEERA_TASK is empty"
  exec /usr/local/bin/keera-agent "${WORKDIR:-$HOME/work}"
fi

# ------------------------------------------------------------------- sshd
#
# Started last, which is deliberate: the readiness probe is a TCP connection to
# this port, so everything above has finished by the time the gateway reports
# the sandbox ready. "Ready" then means what a developer means by it rather
# than "the container started".
log "ready; sshd is listening on 2222"
exec /usr/sbin/sshd -D -e -f /etc/ssh/keera/sshd_config
