#!/usr/bin/env bash
# Run one coding agent on one task, then push what it did.
#
# This is an agent sandbox's whole life. Only a branch leaves this machine,
# which keeps the network policy tight and gives a reviewer a diff to read.
set -euo pipefail

WORKDIR="${1:-$HOME/work}"
cd "$WORKDIR"

log() { printf '%s keera-agent: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

[ -n "${KEERA_TASK:-}" ] || die "KEERA_TASK is empty; there is nothing to do"

# Named after the sandbox, because its id ties the branch to its cost, model
# and task on the gateway.
BRANCH="keera/${KEERA_SANDBOX_NAME:-agent}-${KEERA_SANDBOX_ID##*_}"

if [ -d .git ]; then
  git checkout -b "$BRANCH" 2>/dev/null || git checkout "$BRANCH"
  BASE="$(git rev-parse HEAD)"
else
  log "no repository here; the agent will work on an empty directory"
fi

# ------------------------------------------------------------------- run it
#
# Whichever agent is installed, in its non-interactive mode: there is nobody
# at a terminal.
#
# Pi's configuration and Claude Code's ANTHROPIC_CUSTOM_HEADERS, both written
# by the gateway, carry the session header, so this task is one session in
# `keera session list` (see docs/sessions.md). An image that adds OpenCode
# brings its own configuration, which decides whether it sends KEERA_SESSION.
run_agent() {
  # Pi first, because this image installs it. An image built on this base
  # that adds another agent still gets it.
  if command -v pi >/dev/null 2>&1; then
    log "running pi on the task"
    # -p is Pi's headless mode. -a trusts the project's files, which is safe
    # here: the machine exists for this one task and is deleted after it.
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
# Only if something changed, and only as a branch, never to a default branch:
# what comes out of a machine that ran a model's output is a proposal for a
# human to review.
if [ ! -d .git ]; then
  log "nothing to push: this sandbox has no repository"
  exit "$STATUS"
fi

git add -A
if git diff --cached --quiet; then
  log "the agent changed nothing; no branch was pushed"
  exit "$STATUS"
fi

# The commit message records the task and what carried it out. This is the one
# place the task is kept: in the developer's repository, not on the gateway,
# which promises not to keep it. So the subject is only its start, and the
# body holds all of it when the subject does not. The cut counts characters,
# not bytes, so it never splits a letter.
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
