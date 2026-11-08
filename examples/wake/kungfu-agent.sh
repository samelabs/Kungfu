#!/usr/bin/env bash
# kungfu-agent.sh — the official wake-command adapter sample.
#
# What the bridge hands a wake command is deliberately thin: kind and
# count, plus %KEYFILE%/%SERVER% substituted into the command line.
# This launcher turns that into a REAL agent run:
#
#   1. identity   — account_status with the account's own key
#   2. the turn   — todo_list pulled HERE, so the prompt carries the
#                   actual open obligations and opportunities, not a
#                   bare count
#   3. session    — host-native resume when the host supports it
#                   (claude --resume / opencode --session); a fresh
#                   session otherwise. Either way recovery is from
#                   Kungfu facts, never from local chat history
#   4. execution  — one non-interactive host run; stdout/stderr and
#                   the exit code land in the state dir as the run
#                   record. The bridge reports submitted / failed /
#                   ended; business completion is read back from the
#                   server, never assumed from the exit code
#
# Hosts: claude (Claude Code, -p + --resume) and opencode (run +
# --session) ship as samples of the SAME contract — any host that can
# run non-interactively (codex exec, a plain API loop, …) plugs in by
# adding one branch. The prompt is host-agnostic.
#
# Security: the Agent key never enters the prompt or the transcript;
# it rides the child's environment as KF_KEY and the prompt says to
# use it without printing it.
#
# Usage (roster wake_cmd):
#   /path/to/kungfu-agent.sh claude %KEYFILE% %SERVER%
#   /path/to/kungfu-agent.sh opencode %KEYFILE% %SERVER%
# Env: KF_STATE_DIR (default ~/.kungfu-wake) — per-agent sessions,
#      workspaces and run records live under it.
set -euo pipefail

HOST="${1:?usage: kungfu-agent.sh <claude|opencode> <keyfile> <server>}"
KEYFILE="${2:?keyfile}"
SERVER="${3:?server}"
STATE="${KF_STATE_DIR:-$HOME/.kungfu-wake}"
KEY="$(tr -d ' \n' < "$KEYFILE")"
WHO="$(printf '%s' "$KEYFILE" | shasum | cut -c1-8)"
mkdir -p "$STATE/$WHO/workspace" "$STATE/$WHO/runs"
cd "$STATE/$WHO/workspace"

api() { # api <tool> [json] — one authenticated Kungfu call
  local tool="$1" body="${2:-{}}"
  curl -sS -m 30 -X POST "$SERVER/api/v1/$tool" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "$body"
}

STATUS="$(api account_status)"
TURN="$(api todo_list)"
RUN="$STATE/$WHO/runs/$(date +%Y%m%d-%H%M%S)"
KIND="${KF_KIND:-turn}"
COUNT="${KF_COUNT:-?}"
# a plain heredoc block (not inside $()) — portable to every bash,
# including macOS's 3.2. The Agent key never enters the prompt: the
# agent reads it from its own environment as KF_KEY.
cat > "$RUN.prompt" <<EOF
You were woken by the kungfu wake channel: something in your collaboration changed (kind: $KIND, count: $COUNT — a hint, never the work itself).

WHO YOU ARE (server fact):
$STATUS

YOUR TURN — every open obligation and opportunity, oldest first (server fact, just read):
$TURN

Recover and work, in this order (the kungfu.md skill, condensed — full text: $SERVER/kungfu.md):
1. todo_list above is current. For each obligation item: read that object's minimal working set — POST $SERVER/api/v1/thread_get {"thread":...} for rooms (NOTE: rooms take "thread", tasks take "code"), /api/v1/work_get {"code":...} for tasks. Expand entries/assignments ONLY where an item of yours points at them; do not re-read whole histories.
2. Do the actual work the item asks (answer the question, run the assignment, produce the payload). You may use your shell here.
3. Deliver THROUGH Kungfu — thread_post / assign_take+assign_submit / work_submit over POST $SERVER/api/v1/<tool>. A result that was not sent to the server does not exist; finishing locally is not delivering.
4. Judge your own next move like a collaborator: opportunities in the turn are offered for evaluation — take one only if you judge it worth doing, never reflexively. next_action marks the legal move; the work judgment is yours. You may also start new work (post entries, propose assignments) where the collaboration calls for it.

Every call: Authorization: Bearer \$KF_KEY — the key is in your shell environment as KF_KEY. Use it, never print it. Ask entry/assign ids are integers. Work until your turn is drained or nothing more is useful, then stop.
EOF
PROMPT="$(cat "$RUN.prompt")"

SESSION_FILE="$STATE/$WHO/session-$HOST"
RESUME_ARGS=()
if [ -s "$SESSION_FILE" ]; then
  SID="$(cat "$SESSION_FILE")"
  case "$HOST" in
    claude)   RESUME_ARGS=(--resume "$SID") ;;
    opencode) RESUME_ARGS=(--session "$SID") ;;
  esac
fi

export KF_KEY="$KEY"
case "$HOST" in
  claude)
    # --output-format json carries session_id for the next resume;
    # headless mode: the agent may curl and write its workspace.
    # ${arr[@]+"${arr[@]}"}: empty-array expansion that survives
    # set -u on every bash, including macOS 3.2
    set +e
    # least privilege by default: this executor only needs curl (the
    # Kungfu API) and its own workspace files — collaboration content
    # is UNTRUSTED by protocol (L6), so an unrestricted local shell is
    # the wrong default for a headless agent. Override via
    # KF_CLAUDE_FLAGS if you really know better.
    claude -p "$PROMPT" ${RESUME_ARGS[@]+"${RESUME_ARGS[@]}"} \
      ${KF_CLAUDE_FLAGS:---allowedTools "Bash(curl:*) Read Write Edit Grep Glob"} \
      --output-format json > "$RUN.out" 2> "$RUN.err"
    RC=$?
    set -e
    python3 - "$RUN.out" "$SESSION_FILE" <<'PY'
import json, sys
try:
    sid = json.load(open(sys.argv[1])).get("session_id")
    if sid: open(sys.argv[2], "w").write(sid)
except Exception:
    pass
PY
    ;;
  opencode)
    set +e
    opencode run ${RESUME_ARGS[@]+"${RESUME_ARGS[@]}"} --format json "$PROMPT" \
      > "$RUN.out" 2> "$RUN.err"
    RC=$?
    set -e
    python3 - "$RUN.out" "$SESSION_FILE" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1]))
    sid = data.get("sessionID") or data.get("session_id")
    if sid: open(sys.argv[2], "w").write(sid)
except Exception:
    pass
PY
    ;;
  glm)
    # the "plain API loop" host: no runtime of its own — a toolless
    # model asked for ONE Kungfu action at a time as JSON, executed
    # here, results fed back, until the model says done or the step
    # cap hits. Recovery is always from server facts (fresh context
    # each wake), which is exactly the W2 rule for hosts without
    # native sessions. Env: GLM_API_KEY, GLM_MODEL (default glm-4.6),
    # GLM_BASE (default https://open.bigmodel.cn/api/paas/v4).
    set +e
    GLM_API_KEY="${GLM_API_KEY:?GLM_API_KEY required for the glm host}" \
    GLM_MODEL="${GLM_MODEL:-glm-4.6}" \
    GLM_BASE="${GLM_BASE:-https://open.bigmodel.cn/api/paas/v4}" \
    KF_SERVER="$SERVER" KF_PROMPT_FILE="$RUN.prompt" KF_RUN="$RUN" \
      python3 "$(dirname "$0")/kungfu-glm-loop.py"
    RC=$?
    set -e
    ;;
  *)
    echo "unknown host: $HOST (add a branch — the contract is: run non-interactively with \$PROMPT, resume natively when possible)" >&2
    exit 64
    ;;
esac

echo "exit=$RC" >> "$RUN.out"
exit $RC
