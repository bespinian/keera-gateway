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

# hook adds a line to each of the shell startup files given, unless one with
# the marker is there already: the home directory survives a suspend, so the
# second start finds it.
hook() {
  local marker="$1" line="$2" rc
  shift 2
  for rc in "$@"; do
    touch "$rc"
    grep -qF "$marker" "$rc" || printf '%s\n' "$line" >> "$rc"
  done
}

: "${HOME:=/home/keera}"

# The startup files of the interactive shells in the image. zsh reads only its
# own, so a hook for a person at a prompt goes into both.
interactive=("$HOME/.bashrc" "$HOME/.zshrc")

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
# Everything the gateway set that a person or an agent in here needs.
#
# It has to be the whole set rather than the interesting half: a variable that is
# present in the container's environment and absent from a shell is worse than
# one missing from both, because a script written against it works when the
# entrypoint runs it and fails when somebody runs it by hand. So it is every
# exported variable, the image's PATH and whatever extra the caller asked for
# included, less the ones below.
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
# So a value with a line break cannot go in.
#
# Left out: the shell's own (PWD, OLDPWD, SHLVL), TERM, which is the ssh
# client's to set, and what only this start uses. The task is the developer's
# prose and is not kept on the volume. The keys, pi's files and the git
# credential are written to their own files below, and the first git token goes
# stale: the helper is the way to a current one.
skip=' PWD OLDPWD SHLVL TERM KEERA_TASK KEERA_AUTHORIZED_KEYS KEERA_PI_CONFIG KEERA_PI_SETTINGS KEERA_GIT_USERNAME KEERA_GIT_TOKEN KEERA_GIT_TOKEN_EXPIRES '
: > "$HOME/.ssh/environment"
chmod 600 "$HOME/.ssh/environment"
for name in $(compgen -e | sort); do
  case "$skip" in *" $name "*) continue ;; esac
  value="${!name}"
  case "$value" in *$'\n'*) continue ;; esac
  printf '%s=%s\n' "$name" "$value" >> "$HOME/.ssh/environment"
done

# The same set as something a person can source, for a shell that did not come
# through sshd - `podman exec`, a script, the agent runner. Derived from the one
# file above rather than written twice, because two lists drift. Each line is
# exported as it is, not run: a value is not shell code.
cat > "$HOME/.keera-env" <<'PROFILE'
# Written by keera-sandbox at start, from ~/.ssh/environment. Sourced by the
# shell startup files below; ssh sessions already have these from sshd.
if [ -f "$HOME/.ssh/environment" ]; then
  while IFS= read -r kv; do export "$kv"; done < "$HOME/.ssh/environment"
fi
PROFILE
chmod 600 "$HOME/.keera-env"
# shellcheck disable=SC2016 # expanded by the shell that reads the line
hook .keera-env '[ -f "$HOME/.keera-env" ] && . "$HOME/.keera-env"' \
  "${interactive[@]}" "$HOME/.profile"

# A banner, because the single most useful thing to tell somebody who has just
# opened a shell in an ephemeral machine is when it goes away.
cat > "$HOME/.keera-motd" <<MOTD
  ${KEERA_SANDBOX_NAME:-sandbox}  (${KEERA_SANDBOX_CLASS:-unknown class})
  expires ${KEERA_SANDBOX_EXPIRES:-unknown} unless extended - 'keera sandbox extend ${KEERA_SANDBOX_NAME:-}' from your laptop
  Keep your work in /home/keera. Nothing else is sure to survive a suspend,
  and on a class with no disk, not even that.

  pi is configured for the models this project may use, and for no
  others. No vendor credential is exported: setting one would fill pi's
  model picker with that vendor's whole catalogue and send this key to them.
  For a script that wants the OpenAI SDK's own variable, one line:
      export OPENAI_API_KEY=\$KEERA_API_KEY
MOTD
# shellcheck disable=SC2016 # expanded by the shell that reads the line
hook .keera-motd '[ -n "$PS1" ] && [ -f "$HOME/.keera-motd" ] && cat "$HOME/.keera-motd"' \
  "${interactive[@]}"

# ------------------------------------------------------------- the agent's config
#
# Pi reads ~/.pi/agent/models.json. The gateway renders it with every model the
# key may use. A test keeps it in step with the block `keera connect pi` prints
# for a laptop.
#
# The credential is not in the file. The rendered config names ${KEERA_API_KEY}
# and Pi expands it from the environment, so a developer's edited copy never
# holds a stale key. ~/.ssh/environment above does hold it, for ssh sessions.
#
# keep_ours writes a file the gateway rendered, unless somebody has changed it.
#
# The home directory survives a suspend, so a developer who tuned one of these
# would otherwise find it replaced every time their sandbox came back - and
# losing somebody's configuration to a restart is the kind of thing that stops
# people trusting a machine. The copy beside it is how "unchanged" is decided.
keep_ours() {
  local live="$1" content="$2" what="$3"
  local ours
  ours="${live%/*}/.keera-$(basename "$live")"
  if [ ! -f "$live" ] || { [ -f "$ours" ] && cmp -s "$ours" "$live"; }; then
    printf '%s\n' "$content" > "$live"
    cp "$live" "$ours"
    log "wrote $what"
  else
    log "leaving your own $live alone"
  fi
}

if [ -n "${KEERA_PI_CONFIG:-}" ]; then
  mkdir -p "$HOME/.pi/agent"
  keep_ours "$HOME/.pi/agent/models.json" "$KEERA_PI_CONFIG" \
    "pi's model configuration"
  # Which provider pi starts on. Without it, pi would prefer a built-in
  # provider that an image armed with a vendor variable.
  if [ -n "${KEERA_PI_SETTINGS:-}" ]; then
    keep_ours "$HOME/.pi/agent/settings.json" "$KEERA_PI_SETTINGS" \
      "pi's default provider"
  fi
fi

# ------------------------------------------------------------------- git
#
# The credential is this sandbox's own: one repository, short-lived, minted by
# the gateway when it was created. Its own expiry can be later than the
# sandbox's (a GitHub token's by up to an hour, a GitLab one's until the next
# midnight UTC), so the gateway revokes what the forge lets it when the sandbox
# ends. It is never the developer's own credential and there is no forwarded
# ssh agent, which is the whole point - this machine may be about to run a
# model's output.
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

  if [ -n "${KEERA_GIT_TOKEN:-}" ]; then
    # A helper rather than an https://user:token@host remote, so the token is
    # not in .git/config where every later `git remote -v` would print it - and
    # not in the process list of whatever ran the clone. The helper also gets
    # a fresh token from the gateway when this one runs out.
    git config --global --unset-all credential.helper || true
    git config --global credential.helper keera
    rm -f "$HOME/.git-credentials"
    local cache="$HOME/.config/keera/git-credential" given
    mkdir -p "${cache%/*}"
    # A token the gateway has not handed over before always wins: it comes
    # with a new sandbox or an expired one resumed, and the one before it is
    # revoked. A plain resume hands over the same token again, and the helper
    # may have fetched a newer one since, which is kept. Only a hash of the
    # token is kept to tell the two apart.
    given="$(printf '%s' "$KEERA_GIT_TOKEN" | sha256sum | cut -d' ' -f1)"
    if [ ! -s "$cache" ] || [ "$(cat "$cache.given" 2>/dev/null)" != "$given" ]; then
      (
        umask 077
        {
          printf 'username=%s\npassword=%s\n' "${KEERA_GIT_USERNAME:-keera}" "$KEERA_GIT_TOKEN"
          if [ -n "${KEERA_GIT_TOKEN_EXPIRES:-}" ]; then
            printf 'password_expiry_utc=%s\n' "$KEERA_GIT_TOKEN_EXPIRES"
          fi
        } > "$cache"
        printf '%s\n' "$given" > "$cache.given"
      )
    fi
  fi
}

checkout() {
  [ -n "${KEERA_REPO:-}" ] || return 0
  local dir
  dir="$HOME/work/$(basename "${KEERA_REPO%.git}")"
  if [ -d "$dir/.git" ]; then
    log "repository already checked out at $dir"
    echo "$dir"
    return 0
  fi
  log "cloning ${KEERA_REPO}"
  # A shallow clone. An agent working on one task needs the tip and not five
  # years of history, and on a large repository the difference is minutes of
  # somebody's time every single sandbox. `git fetch --unshallow` gets the rest.
  # set -e does not apply inside the $(...) that calls this, so each failure
  # returns on its own. git's output goes to stderr: stdout is the directory.
  if [ -n "${KEERA_BRANCH:-}" ]; then
    git clone --depth 1 --branch "$KEERA_BRANCH" "$KEERA_REPO" "$dir" >&2 || return 1
  else
    git clone --depth 1 "$KEERA_REPO" "$dir" >&2 || return 1
  fi
  echo "$dir"
}

setup_git
# An agent's task is about its repository, so it stops without one. Somebody
# working in an engineer sandbox can still clone it by hand.
if ! WORKDIR="$(checkout)"; then
  WORKDIR=""
  if [ "$KEERA_SANDBOX_PURPOSE" = "agent" ]; then
    die "cloning ${KEERA_REPO} failed; the agent has no repository to work on"
  fi
  log "warning: cloning ${KEERA_REPO} failed; the sandbox starts without it"
fi
if [ -n "${WORKDIR:-}" ]; then
  ln -sfn "$WORKDIR" "$HOME/repo"
  echo "cd $HOME/repo" > "$HOME/.keera-cd"
  # shellcheck disable=SC2016 # expanded by the shell that reads the line
  hook .keera-cd '[ -n "$PS1" ] && [ -f "$HOME/.keera-cd" ] && . "$HOME/.keera-cd"' \
    "${interactive[@]}"
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
# Started last, which is deliberate: both drivers call a sandbox ready only
# once this port answers, so everything above has finished by the time the
# gateway reports the sandbox ready. "Ready" then means what a developer means
# by it rather than "the container started".
log "ready; sshd is listening on 2222"
exec /usr/sbin/sshd -D -e -f /etc/ssh/keera/sshd_config
