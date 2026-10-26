#!/usr/bin/env bash
# The first thing that runs inside a Keera sandbox.
#
# It prepares the home directory, checks out the repository if there is one,
# points the coding agent at the gateway, and then either starts sshd for a
# person or runs the agent on its task and stops.
#
# Everything comes in the environment from the gateway, so it works air-gapped.
set -euo pipefail

log() { printf '%s keera-sandbox: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

# hook adds a line to each startup file given, unless the marker is already
# there, as the home survives a suspend.
hook() {
  local marker="$1" line="$2" rc
  shift 2
  for rc in "$@"; do
    touch "$rc"
    grep -qF "$marker" "$rc" || printf '%s\n' "$line" >> "$rc"
  done
}

: "${HOME:=/home/keera}"

# zsh reads only its own startup file, so interactive hooks go into both.
interactive=("$HOME/.bashrc" "$HOME/.zshrc")

: "${KEERA_SANDBOX_PURPOSE:=engineer}"

# ---------------------------------------------------------------- the home
#
# The home survives a suspend, so this is idempotent and keeps the owner's
# changes.
mkdir -p "$HOME/.ssh" "$HOME/.config" "$HOME/work"
chmod 700 "$HOME/.ssh"

# The keys that may open a shell in here.
#
# The gateway already checked the caller owns this sandbox. This is a second
# lock, as a pod IP is reachable by anything the network policy admits.
if [ -n "${KEERA_AUTHORIZED_KEYS:-}" ]; then
  printf '%s\n' "$KEERA_AUTHORIZED_KEYS" > "$HOME/.ssh/authorized_keys"
  chmod 600 "$HOME/.ssh/authorized_keys"
fi

# ------------------------------------------------------------- the gateway
#
# Every exported variable but those skipped, written once to
# ~/.ssh/environment (see PermitUserEnvironment in sshd_config). The format has
# no quoting, so multi-line values are skipped.
#
# Skipped: the shell's own, TERM (the ssh client sets it), the task (not kept
# on the volume), what gets its own file below, and the git token, which the
# helper fetches fresh.
skip=' PWD OLDPWD SHLVL TERM KEERA_TASK KEERA_AUTHORIZED_KEYS KEERA_PI_CONFIG KEERA_PI_SETTINGS KEERA_GIT_USERNAME KEERA_GIT_TOKEN KEERA_GIT_TOKEN_EXPIRES '
: > "$HOME/.ssh/environment"
chmod 600 "$HOME/.ssh/environment"
for name in $(compgen -e | sort); do
  case "$skip" in *" $name "*) continue ;; esac
  value="${!name}"
  case "$value" in *$'\n'*) continue ;; esac
  printf '%s=%s\n' "$name" "$value" >> "$HOME/.ssh/environment"
done

# The same set for a shell that did not come through sshd, such as
# `podman exec` or the agent runner. Each line is exported, not run.
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

# A banner, mainly to say when this machine goes away.
cat > "$HOME/.keera-motd" <<MOTD
  ${KEERA_SANDBOX_NAME:-sandbox}  (${KEERA_SANDBOX_CLASS:-unknown class})
  started with an expiry of ${KEERA_SANDBOX_EXPIRES:-unknown}. From your laptop,
  'keera sandbox show ${KEERA_SANDBOX_NAME:-}' says when it expires now, and
  'keera sandbox extend ${KEERA_SANDBOX_NAME:-}' gives it longer.
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
# Pi reads ~/.pi/agent/models.json, which the gateway renders with every model
# the key may use. It names ${KEERA_API_KEY}, which Pi expands, so an edited
# copy never holds a stale key.
#
# keep_ours writes a file the gateway rendered, unless its owner changed it.
# The copy beside it shows whether it changed.
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
  # Which provider pi starts on, rather than a built-in one an image set up.
  if [ -n "${KEERA_PI_SETTINGS:-}" ]; then
    keep_ours "$HOME/.pi/agent/settings.json" "$KEERA_PI_SETTINGS" \
      "pi's default provider"
  fi
fi

# ------------------------------------------------------------------- git
#
# The credential is this sandbox's own: one repository, short-lived, minted by
# the gateway, and revoked where the forge allows when the sandbox ends. It is
# never the developer's, and no ssh agent is forwarded: this machine may run a
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
    # A helper keeps the token out of .git/config and the process list, and
    # fetches a fresh one when it runs out.
    git config --global --unset-all credential.helper || true
    git config --global credential.helper keera
    rm -f "$HOME/.git-credentials"
    local cache="$HOME/.config/keera/git-credential" given
    mkdir -p "${cache%/*}"
    # A new token from the gateway wins, as the old one is revoked. A plain
    # resume hands over the same token, so a newer one the helper fetched is
    # kept. A hash of the token tells the two apart.
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
  # Shallow: one task needs only the tip. `git fetch --unshallow` gets the
  # rest. set -e does not apply inside the $(...) that calls this, so failures
  # return explicitly. git's output goes to stderr: stdout is the directory.
  git clone --depth 1 ${KEERA_BRANCH:+--branch "$KEERA_BRANCH"} "$KEERA_REPO" "$dir" >&2 || return 1
  echo "$dir"
}

setup_git
# An agent stops without its repository. An engineer can still clone by hand.
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
# An agent sandbox runs its task and stops, with no sshd.
if [ "$KEERA_SANDBOX_PURPOSE" = "agent" ]; then
  exec /usr/local/bin/keera-agent "${WORKDIR:-$HOME/work}"
fi

# ------------------------------------------------------------------- sshd
#
# Started last: a sandbox is ready once this port answers.
log "ready; sshd is listening on 2222"
exec /usr/sbin/sshd -D -e -f /etc/ssh/keera/sshd_config
