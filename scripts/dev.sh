#!/usr/bin/env bash
# Local development for Kungfu. Needs only Docker.
#
#   scripts/dev.sh test [pkgs...]   run the CI gate (gofmt, vet, tests on a
#                                   fresh PostgreSQL built from migrations/)
#   scripts/dev.sh up               start a local server on http://127.0.0.1:8090
#                                   (persistent dev database, all migrations)
#   scripts/dev.sh seed-admin NAME  create a superadmin in the dev database
#                                   (password read from the terminal)
#   scripts/dev.sh reset            drop the dev database and start over
#   scripts/dev.sh down             stop the dev server and database
#
# Nothing here touches production. Production is deployed only with
# scripts/deploy.sh from a commit that passed CI on main.
set -euo pipefail
cd "$(dirname "$0")/.."

IMG=kungfu-tools
NET=kungfu-dev
DB=kungfu-dev-pg
APP=kungfu-dev-app
VOL=kungfu-dev-pgdata
CACHE=kungfu-dev-gocache
PG_ENV=(-e POSTGRES_USER=kungfu -e POSTGRES_PASSWORD=kungfu -e POSTGRES_DB=kungfu_md)

tools() {
  docker build -q -t "$IMG" -f scripts/tools.Dockerfile scripts >/dev/null
}

net() { docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null; }

run_tools() { # run a command in the toolchain container with the repo mounted
  docker run --rm --network "$NET" -v "$PWD":/src -v "$CACHE":/go -e GOCACHE=/go/cache -w /src "$@"
}

wait_pg() { # $1 = container
  for _ in $(seq 60); do docker exec "$1" pg_isready -U kungfu -d kungfu_md >/dev/null 2>&1 && return; sleep 1; done
  echo "PostgreSQL did not become ready" >&2; exit 1
}

migrate() { # $1 = db host (container name)
  run_tools -e PGPASSWORD=kungfu "$IMG" sh -c \
    'for f in migrations/*.sql; do psql -q -h '"$1"' -U kungfu -d kungfu_md -v ON_ERROR_STOP=1 -f "$f" >/dev/null || { echo "migration failed: $f"; exit 1; }; done'
}

cmd_test() {
  tools; net
  TEST_PG="kungfu-test-pg-$$"
  local pg="$TEST_PG"
  trap 'docker rm -f "$TEST_PG" >/dev/null 2>&1 || true' EXIT
  docker run -d --name "$pg" --network "$NET" "${PG_ENV[@]}" postgres:16-alpine >/dev/null
  wait_pg "$pg"
  migrate "$pg"
  local pkgs="${*:-./...}"
  run_tools -e KF_TEST_DATABASE_URL="postgres://kungfu:kungfu@$pg:5432/kungfu_md?sslmode=disable" \
    -e DB_PASS=kungfu -e SESSION_SECRET=local-test-session-secret-000000000 "$IMG" sh -c '
      set -e
      u=$(gofmt -l ./cmd ./internal ./web); [ -z "$u" ] || { echo "gofmt needed:"; echo "$u"; exit 1; }
      go vet ./...
      # -p 1: test packages run SERIALLY — they share the one test
      # PostgreSQL, so parallel packages would mutate the fixtures of
      # one another (WO-7e).
      go test -p 1 -count=1 '"$pkgs"' 2>&1 | tee /tmp/test.log
      if grep -q "KF_TEST_DATABASE_URL not set" /tmp/test.log; then echo "integration tests skipped"; exit 1; fi
      grep -q "^FAIL" /tmp/test.log && exit 1 || true'
  echo "gate: PASS"
}

cmd_up() {
  tools; net
  if ! docker ps -q -f name="^$DB$" | grep -q .; then
    local fresh=0
    docker volume inspect "$VOL" >/dev/null 2>&1 || fresh=1
    docker rm -f "$DB" >/dev/null 2>&1 || true
    docker run -d --name "$DB" --network "$NET" -p 127.0.0.1:15432:5432 -v "$VOL":/var/lib/postgresql/data "${PG_ENV[@]}" postgres:16-alpine >/dev/null
    wait_pg "$DB"
    if [ "$fresh" = 1 ]; then echo "fresh dev database: applying migrations"; migrate "$DB"; fi
  fi
  docker rm -f "$APP" >/dev/null 2>&1 || true
  docker run -d --name "$APP" --network "$NET" -p 127.0.0.1:8090:8090 -v "$PWD":/src -v "$CACHE":/go -e GOCACHE=/go/cache -w /src \
    -e DB_HOST="$DB" -e DB_PORT=5432 -e DB_NAME=kungfu_md -e DB_USER=kungfu -e DB_PASS=kungfu -e DB_SSLMODE=disable \
    -e SESSION_SECRET=local-dev-session-secret-00000000000 \
    -e SETTINGS_ENC_KEY=0000000000000000000000000000000000000000000000000000000000000000 \
    -e LISTEN_ADDR=0.0.0.0:8090 "$IMG" go run -ldflags "-X kungfu.md/internal/version.commit=dev" ./cmd/server >/dev/null
  for _ in $(seq 120); do curl -sf http://127.0.0.1:8090/readyz >/dev/null 2>&1 && { echo "up: http://127.0.0.1:8090 (console: /samelabs)"; return; }; sleep 1; done
  echo "server did not start; logs:"; docker logs --tail 30 "$APP"; exit 1
}

cmd_seed_admin() {
  local name="${1:?usage: scripts/dev.sh seed-admin NAME}"
  read -r -s -p "password for $name: " pw; echo
  tools; net
  local hash
  hash=$(printf '%s' "$pw" | run_tools -i "$IMG" go run ./scripts/internal/bcrypt)
  docker exec -i "$DB" psql -q -U kungfu -d kungfu_md -v ON_ERROR_STOP=1 \
    -v name="$name" -v hash="$hash" <<'SQL'
BEGIN;
INSERT INTO tb_admins (username, display_name, password_hash) VALUES (:'name', :'name', :'hash');
INSERT INTO tb_admin_user_roles (admin_id, role_id)
SELECT a.id, r.id FROM tb_admins a, tb_admin_roles r WHERE a.username = :'name' AND r.code = 'superadmin';
COMMIT;
SQL
  echo "superadmin $name created"
}

cmd_down() { docker rm -f "$APP" "$DB" >/dev/null 2>&1 || true; echo "stopped (data kept in volume $VOL)"; }

cmd_reset() { cmd_down; docker volume rm "$VOL" >/dev/null 2>&1 || true; echo "dev database removed"; }

case "${1:-}" in
  test) shift; cmd_test "$@" ;;
  up) cmd_up ;;
  seed-admin) shift; cmd_seed_admin "$@" ;;
  down) cmd_down ;;
  reset) cmd_reset ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
