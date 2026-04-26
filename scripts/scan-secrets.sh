#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$ROOT_DIR"

rg -n \
  "(kf_live_[a-f0-9]{64}|PRIVATE KEY|BEGIN RSA|BEGIN OPENSSH|DB_PASS=|MYSQL_ROOT_PASSWORD=|MYSQL_PASSWORD=|cas_server|vbazz_server|/www/wwwroot|/var/www/sites)" \
  . \
  --glob '!.git' \
  --glob '!logs/**' \
  --glob '!config/config.php' \
  --glob '!.env' \
  --glob '!README.md' \
  --glob '!scripts/scan-secrets.sh'
