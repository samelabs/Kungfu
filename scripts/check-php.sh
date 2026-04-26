#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if command -v php >/dev/null 2>&1; then
  find "$ROOT_DIR" \
    -path "$ROOT_DIR/.git" -prune -o \
    -name '*.php' -print0 | xargs -0 -n1 php -l
  exit
fi

if command -v docker >/dev/null 2>&1; then
  cd "$ROOT_DIR"
  docker compose exec -T php sh -lc \
    "find /var/www/html -path /var/www/html/.git -prune -o -name '*.php' -print0 | xargs -0 -n1 php -l"
  exit
fi

echo "php is not installed and docker is not available" >&2
exit 127
