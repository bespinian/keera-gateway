#!/usr/bin/env bash
# Run one coding agent on one task, then push what it did.
#
# Only a branch leaves this machine, which keeps the network policy tight and
# gives a reviewer a diff to read.
set -euo pipefail

WORKDIR="${1:-$HOME/work}"
cd "$WORKDIR"

log() { printf '%s keera-agent: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

[ -n "${KEERA_TASK:-}" ] || die "KEERA_TASK is empty; there is nothing to do"

# Named after the sandbox, whose id ties the branch to its cost on the gateway.
BRANCH="keera/${KEERA_SANDBOX_NAME:-agent}-${KEERA_SANDBOX_ID##*_}"

if [ -d .git ]; then
  git checkout -b "$BRANCH" 2>/dev/null || git checkout "$BRANCH"
  BASE="$(git rev-parse HEAD)"
else
  log "no repository here; the agent will work on an empty directory"
fi

# ------------------------------------------------------------------- run it
#
# Whichever agent is installed, in its non-interactive mode.
#
# Pi's configuration and Claude Code's ANTHROPIC_CUSTOM_HEADERS carry the
# session header, so this task is one session (see docs/sessions.md). An image
# that adds OpenCode decides itself whether it sends KEERA_SESSION.
run_agent() {
  # Pi first, because this image installs it.
  if command -v pi >/dev/null 2>&1; then
    log "running pi on the task"
    # -p is Pi's headless mode. -a trusts the project's files, which is safe
    # on a machine deleted after the task.
    pi -p -a ${KEERA_MODEL:+--model "$KEERA_MODEL"} -- "$KEERA_TASK"
    return
  fi
  if command -v claude >/dev/null 2>&1; then
    log "running claude on the task"
    claude --print --permission-mode acceptEdits "$KEERA_TASK"
    return
  fi
  if command -v opencode >/dev/null 2>&1; then
    log "running opencode on the task"
    opencode run ${KEERA_MODEL:+--model "$KEERA_MODEL"} "$KEERA_TASK"
    return
  fi
  die "no coding agent is on PATH. The base image installs Pi; an image built on \
top of it that removed Pi is expected to add its own (see sandbox/Containerfile)"
}

STATUS=0
run_agent || STATUS=$?
log "the agent finished with status $STATUS"

# ------------------------------------------------------------------ push it
#
# Only if something changed, and only as a branch: the output is a proposal
# for a human to review.
if [ ! -d .git ]; then
  log "nothing to push: this sandbox has no repository"
  exit "$STATUS"
fi

git add -A
if git diff --cached --quiet; then
  log "the agent changed nothing; no branch was pushed"
  exit "$STATUS"
fi

# The commit message records the task and what carried it out. It is the only
# place the task is kept, as the gateway does not keep it. The subject is its
# start, and the body holds all of it when needed. The cut counts characters,
# so it never splits a letter.
LC_ALL=C.UTF-8
subject=${KEERA_TASK//$'\n'/ }
subject=${subject:0:72}
task=
[ "$subject" = "$KEERA_TASK" ] || task="$KEERA_TASK"$'\n\n'
git -c user.name="${KEERA_GIT_AUTHOR_NAME:-Keera agent}" \
    -c user.email="${KEERA_GIT_AUTHOR_EMAIL:-keera-agent@localhost}" \
    commit --quiet --message "$(printf '%s\n\n%sCarried out in Keera sandbox %s (%s).\nAgent exit status: %s.\n' \
      "$subject" "$task" "${KEERA_SANDBOX_NAME:-}" "${KEERA_SANDBOX_ID:-}" "$STATUS")"

log "pushing $BRANCH"
git push --set-upstream origin "$BRANCH"

log "pushed $BRANCH ($(git rev-list --count "$BASE..HEAD") commit(s))"
log "open a merge request from $BRANCH; nothing else left this sandbox"
exit "$STATUS"
