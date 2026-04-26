#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST="${KUNGFU_DEPLOY_HOST:-}"
REMOTE_ROOT="${KUNGFU_DEPLOY_ROOT:-}"
LOCAL_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

mode="${1:---dry-run}"
if [[ "$mode" != "--dry-run" && "$mode" != "--apply" ]]; then
  echo "Usage: KUNGFU_DEPLOY_HOST=host KUNGFU_DEPLOY_ROOT=/path scripts/deploy-cas.sh [--dry-run|--apply]" >&2
  exit 2
fi
if [[ -z "$REMOTE_HOST" || -z "$REMOTE_ROOT" ]]; then
  echo "KUNGFU_DEPLOY_HOST and KUNGFU_DEPLOY_ROOT are required" >&2
  exit 2
fi

rsync_args=(
  -rtz
  --delete
  --omit-dir-times
  --exclude '.git/'
  --exclude '.gitignore'
  --exclude '.DS_Store'
  --exclude '.env'
  --exclude '.env.example'
  --exclude 'AGENT.md'
  --exclude 'README.md'
  --exclude 'docs/'
  --exclude 'docker/'
  --exclude 'docker-compose.yml'
  --exclude 'init.sql'
  --exclude 'logs/'
  --exclude 'scripts/'
  --exclude '*.bak'
  --exclude '*.bak-*'
)

if [[ "$mode" == "--dry-run" ]]; then
  rsync_args+=(--dry-run --itemize-changes)
fi

rsync "${rsync_args[@]}" "$LOCAL_ROOT/" "$REMOTE_HOST:$REMOTE_ROOT/"

if [[ "$mode" == "--apply" ]]; then
  ssh -o ConnectTimeout=10 "$REMOTE_HOST" "php -l '$REMOTE_ROOT/index.php' >/dev/null && php -l '$REMOTE_ROOT/core/TaskSubmissionService.php' >/dev/null"
fi
