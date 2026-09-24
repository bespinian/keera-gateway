#!/usr/bin/env bash
# Run one coding agent on one task, then push what it did.
#
# This is the whole of an agent sandbox's life. It matters less for what it
# runs - any of three agents, whichever the image has - than for what it does
# afterwards: a branch and nothing else leaves this machine. That is what makes
# the network policy around it tight enough to be worth writing, and what gives
# a reviewer a diff instead of a trust exercise.
set -euo pipefail

WORKDIR="${1:-$HOME/work}"
cd "$WORKDIR"

log() { printf '%s keera-agent: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

[ -n "${KEERA_TASK:-}" ] || die "KEERA_TASK is empty; there is nothing to do"

# The branch. Named after the sandbox, because the sandbox id is the one string
# that ties a branch back to what it cost, which model answered, and the task
# it was carrying out - all of which are on the gateway under that id.
BRANCH="${KEERA_PUSH_BRANCH:-keera/${KEERA_SANDBOX_NAME:-agent}-${KEERA_SANDBOX_ID##*_}}"

if [ -d .git ]; then
  git checkout -b "$BRANCH" 2>/dev/null || git checkout "$BRANCH"
  BASE="$(git rev-parse HEAD)"
else
  log "no repository here; the agent will work on an empty directory"
  BASE=""
fi

# ------------------------------------------------------------------- run it
#
# Whichever agent is installed, in the order a deployment is most likely to
# have meant. Each is given the task on stdin or as an argument in its own
# non-interactive mode; none of them is given a terminal, because there is
# nobody here.
#
# The session header is already set for all three. Claude Code reads
# ANTHROPIC_CUSTOM_HEADERS, which the gateway set; the other two are given
# KEERA_SESSION and send it themselves. That is what makes this task one
# session in `keera sessions` rather than an inference from its opening prompt -
# see docs/sessions.md for why that distinction is worth the trouble.
run_agent() {
  # Pi first, because it is the agent this image installs. The other two are
  # tried after it, so that a deployment which put one into its own image on top
  # of this base still gets the one it chose.
  if command -v pi >/dev/null 2>&1; then
    log "running pi on the task"
    # -p is Pi's headless mode: one prompt, output on stdout, no TUI. -a trusts
    # the project's own files for this run, which an agent sandbox can afford in
    # a way a laptop cannot - the machine exists for this one task and is
    # deleted after it.
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
    opencode run --model "${KEERA_MODEL:-}" "$KEERA_TASK"
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
# Only if something changed, and only ever as a branch. There is no path here
# that writes to anybody's default branch: what comes out of a machine that ran
# a model's output is a proposal, and a human opens it.
if [ ! -d .git ]; then
  log "nothing to push: this sandbox has no repository"
  exit "$STATUS"
fi

git add -A
if git diff --cached --quiet; then
  log "the agent changed nothing; no branch was pushed"
  exit "$STATUS"
fi

# The commit message says what it was asked to do and what did it. The task is
# the developer's own words, and this is the one place it is deliberately
# recorded - in their repository, where they can read it, rather than on the
# gateway, which promises not to keep it.
git -c user.name="${KEERA_GIT_AUTHOR_NAME:-Keera agent}" \
    -c user.email="${KEERA_GIT_AUTHOR_EMAIL:-keera-agent@localhost}" \
    commit --quiet --message "$(printf '%s\n\nCarried out in Keera sandbox %s (%s).\nAgent exit status: %s.\n' \
      "$(printf '%s' "$KEERA_TASK" | head -c 72 | tr '\n' ' ')" \
      "${KEERA_SANDBOX_NAME:-}" "${KEERA_SANDBOX_ID:-}" "$STATUS")"

log "pushing $BRANCH"
git push --set-upstream origin "$BRANCH"

log "pushed $BRANCH ($(git rev-list --count "${BASE:-HEAD}..HEAD") commit(s))"
log "open a merge request from $BRANCH; nothing else left this sandbox"
exit "$STATUS"
