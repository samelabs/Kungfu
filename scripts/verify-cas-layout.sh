#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST="${KUNGFU_DEPLOY_HOST:-}"
REMOTE_ROOT="${KUNGFU_DEPLOY_ROOT:-}"
LOCAL_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "$REMOTE_HOST" || -z "$REMOTE_ROOT" ]]; then
  echo "Usage: KUNGFU_DEPLOY_HOST=host KUNGFU_DEPLOY_ROOT=/path scripts/verify-cas-layout.sh" >&2
  exit 2
fi

echo "local:  $LOCAL_ROOT"
echo "remote: $REMOTE_HOST:$REMOTE_ROOT"

ssh -o ConnectTimeout=10 "$REMOTE_HOST" "test -d '$REMOTE_ROOT' && find '$REMOTE_ROOT' -maxdepth 2 -type d | sort"
