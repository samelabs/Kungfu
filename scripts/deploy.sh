#!/usr/bin/env bash
# Deploy a commit to production (kungfu.md). Run from a developer machine
# with Docker, ssh access to the server, and the GitHub CLI.
#
#   scripts/deploy.sh [REF] [--apply-migrations] [--dry-run]
#   scripts/deploy.sh config-diff        compare server config with deploy/
#
# REF defaults to origin/main. The rules are fixed:
#   * only commits on origin/main whose GitHub checks all passed;
#   * the binary is built from `git archive REF` (never a working tree)
#     with the commit stamped in, so /readyz reports exactly what runs;
#   * migrations are append-only: a changed existing migration aborts,
#     new ones abort unless --apply-migrations (a DB backup runs first,
#     and they are applied as the application role);
#   * the server checkout under /var/www/kungfu.md follows the binary;
#   * the new binary must answer /readyz with its commit within 30 s,
#     otherwise the previous binary and checkout are restored.
# Nobody edits code on the server; every change goes through this path.
set -euo pipefail
cd "$(dirname "$0")/.."

HOST=${KUNGFU_DEPLOY_HOST:-cas_server}
REPO=samelabs/Kungfu
APP_DIR=/var/www/kungfu.md
PUBLIC=https://kungfu.md
IMG=kungfu-tools

die() { echo "deploy: $*" >&2; exit 1; }

config_diff() {
  local rc=0
  while read -r local remote; do
    if ssh -n -o BatchMode=yes "$HOST" "sudo -n cat '$remote'" | diff -u --label "server:$remote" --label "repo:$local" - "$local"; then
      echo "same: $remote"
    else
      rc=1
    fi
  done <<EOF
deploy/nginx/kungfu.md.conf /etc/nginx/sites-enabled/kungfu.md.conf
deploy/nginx/kungfu-edge-headers.conf /etc/nginx/snippets/kungfu-edge-headers.conf
deploy/systemd/kungfu-go.service /etc/systemd/system/kungfu-go.service
deploy/backup/kungfu-db-backup /usr/local/bin/kungfu-db-backup
deploy/backup/kungfu-db-backup.cron /etc/cron.d/kungfu-db-backup
EOF
  return $rc
}

if [ "${1:-}" = config-diff ]; then config_diff; exit $?; fi

REF=origin/main APPLY=0 DRY=0
for a in "$@"; do
  case "$a" in
    --apply-migrations) APPLY=1 ;;
    --dry-run) DRY=1 ;;
    -*) die "unknown option $a" ;;
    *) REF=$a ;;
  esac
done

# 1. Which commit, and is it allowed?
git fetch -q origin
SHA=$(git rev-parse --verify "$REF^{commit}") || die "unknown ref $REF"
SHORT=${SHA:0:12}
git merge-base --is-ancestor "$SHA" origin/main || die "$SHORT is not on origin/main — merge it through a PR first"

checks=$(gh api "repos/$REPO/commits/$SHA/check-runs" --jq '[.check_runs[] | "\(.status)/\(.conclusion)"] | join(" ")')
[ -n "$checks" ] || die "no CI checks found for $SHORT"
for c in $checks; do [ "$c" = completed/success ] || die "CI for $SHORT is not green: $checks"; done

# 2. What runs now, and what changes?
CUR=$(ssh -o BatchMode=yes "$HOST" "git -C $APP_DIR rev-parse HEAD")
echo "deployed: ${CUR:0:12}   target: $SHORT"
[ "$CUR" != "$SHA" ] || { echo "already deployed"; exit 0; }
git cat-file -e "$CUR^{commit}" 2>/dev/null || die "deployed commit ${CUR:0:12} is unknown locally"

changed=$(git diff --name-only --diff-filter=MDR "$CUR" "$SHA" -- migrations/)
[ -z "$changed" ] || die "existing migrations were modified or removed (migrations are append-only): $changed"
NEWMIG=$(git diff --name-only --diff-filter=A "$CUR" "$SHA" -- migrations/ | sort | tr '\n' ' ')
if [ -n "$NEWMIG" ]; then
  echo "new migrations: $NEWMIG"
  [ "$APPLY" = 1 ] || die "re-run with --apply-migrations to back up the database and apply them"
fi
git --no-pager log --oneline "$CUR..$SHA" | sed 's/^/  /'

# 3. Build exactly this commit.
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
git archive "$SHA" | tar -x -C "$OUT"
docker build -q -t "$IMG" -f scripts/tools.Dockerfile scripts >/dev/null
docker run --rm -v "$OUT":/src -v kungfu-dev-gocache:/go -w /src \
  -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 "$IMG" \
  go build -trimpath -buildvcs=false -ldflags "-s -w -X kungfu.md/internal/version.commit=$SHORT" \
  -o /src/kungfu-server ./cmd/server
BIN="$OUT/kungfu-server"
SUM=$(shasum -a 256 "$BIN" | cut -d' ' -f1)
echo "built $SHORT ($(du -h "$BIN" | cut -f1), sha256 ${SUM:0:16})"
[ "$DRY" = 0 ] || { echo "dry run: stopping before upload"; exit 0; }

# 4. Upload and switch over on the server.
scp -q "$BIN" "$HOST:/tmp/kungfu-server.$SHORT"
ssh -o BatchMode=yes "$HOST" "sudo -n env SHA=$SHA SHORT=$SHORT SUM=$SUM NEWMIG='$NEWMIG' bash -s" <<'REMOTE'
set -euo pipefail
APP=/var/www/kungfu.md BIN=/var/www/kungfu.md/kungfu-server BK=/var/backups/kungfu/binaries
LOG=/var/log/kungfu-deploy.log NEW=/tmp/kungfu-server.$SHORT
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$LOG"; }
[ "$(sha256sum "$NEW" | cut -d' ' -f1)" = "$SUM" ] || { echo "upload checksum mismatch"; exit 1; }
PREV=$(sudo -u ubuntu git -C "$APP" rev-parse HEAD); P12=${PREV:0:12}

sudo -u ubuntu git -C "$APP" fetch -q origin
sudo -u ubuntu git -C "$APP" checkout -q --detach "$SHA"

if [ -n "${NEWMIG// /}" ]; then
  (cd / && sudo -u postgres /usr/local/bin/kungfu-db-backup) | tee -a "$LOG"
  set -a; . /etc/kungfu-go.env; set +a
  for f in $NEWMIG; do
    if ! PGPASSWORD="$DB_PASS" psql -q -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 -f "$APP/$f"; then
      sudo -u ubuntu git -C "$APP" checkout -q --detach "$PREV"
      log "FAILED migration $f for $SHORT; binary unchanged (still $P12); restore from the backup above if needed"
      exit 1
    fi
    log "applied $f"
  done
fi

mkdir -p "$BK"
[ -e "$BK/kungfu-server.$P12.previous" ] || cp -p "$BIN" "$BK/kungfu-server.$P12.previous"
install -o ubuntu -g ubuntu -m 755 "$NEW" "$BIN.new" && mv -f "$BIN.new" "$BIN"
systemctl restart kungfu-go

ok=0
for _ in $(seq 30); do
  sleep 1
  curl -sf localhost:8090/readyz | grep -q "\"commit\": *\"$SHORT\"" && { ok=1; break; }
done
if [ "$ok" != 1 ]; then
  cp -p "$BK/kungfu-server.$P12.previous" "$BIN.new" && mv -f "$BIN.new" "$BIN"
  sudo -u ubuntu git -C "$APP" checkout -q --detach "$PREV"
  systemctl restart kungfu-go
  log "FAILED $P12 -> $SHORT: not ready within 30s; rolled back to $P12"
  journalctl -u kungfu-go -n 20 --no-pager
  exit 1
fi
# keep the five newest previous binaries
ls -1t "$BK"/kungfu-server.*.previous 2>/dev/null | tail -n +6 | xargs -r rm -f
rm -f "$NEW"
log "OK $P12 -> $SHORT${NEWMIG:+ (migrations: $NEWMIG)}"
REMOTE

# 5. Verify from the outside.
curl -sf "$PUBLIC/readyz" | grep -q "\"commit\": *\"$SHORT\"" || die "public /readyz does not report $SHORT"
code=$(curl -s -o /dev/null -w '%{http_code}' "$PUBLIC/mcp" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' -H 'Mcp-Method: tools/list' -H 'Mcp-Protocol-Version: 2026-07-28' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"deploy","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}')
[ "$code" = 200 ] || die "MCP tools/list returned $code"
echo "deployed $SHORT: $PUBLIC/readyz and MCP tools/list OK"
