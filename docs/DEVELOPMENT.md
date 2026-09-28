# Development chain

One path from an idea to production. Nothing is edited on the server.

```
local change ──► scripts/dev.sh test ──► PR ──► CI green ──► merge to main ──► scripts/deploy.sh
```

## 1. Work locally

Tests need Go 1.25+ and Node on the host (`brew install go node`) and Docker for a throwaway PostgreSQL (tmpfs, removed with its volumes after the run). `up`, `seed-admin`, `down` and `reset` need only Docker.

| Command | What it does |
|---|---|
| `scripts/dev.sh test` | The CI gate: gofmt, vet and every test against a fresh PostgreSQL built from `migrations/`. `scripts/dev.sh test ./internal/mcpserver/` runs one package; with package arguments vet covers only those packages. Test packages run serially (`go test -p 1`) because they share the one test database. |
| `scripts/dev.sh test -run TestName ./internal/service/` | One test: `-run` (and other `go test` flags) pass through; vet covers only the named packages. |
| `scripts/dev.sh up` | Local server on http://127.0.0.1:8090 with a persistent dev database (all migrations applied on first start). |
| `scripts/dev.sh seed-admin NAME` | Create a superadmin for `/samelabs` in the dev database (password read from the terminal). |
| `scripts/dev.sh down` / `reset` | Stop the dev server and database / also delete the dev data. |

Tests compile natively with the host Go build cache; nothing but PostgreSQL runs in the Docker VM. The toolchain image (`scripts/tools.Dockerfile`) is used only by `up` and `scripts/deploy.sh` and pins the Go version from `go.mod`.

**Where tests run.** The full gate runs in CI on every PR (free for this public repository) — do not run `scripts/dev.sh test` without arguments locally. Locally run only what you are changing: one test (`-run`) or one package, then push and let CI run everything.

## 2. Branch, PR, CI

- Branch from `main`, open a PR. CI (`.github/workflows/ci.yml`) runs the same gate plus a container build and smoke test.
- `main` is protected for everyone, admins included: changes land only through a PR whose `test` check passed on top of the latest `main`; no direct pushes, no force pushes, no deletion.
- PRs are squash-merged (one commit per PR on `main`) and their branch is deleted on merge. `main` is the only long-lived branch and the only deployable one.

Rules the tests and the deploy script enforce:

- **Migrations are append-only.** Add `migrations/NNN_description.sql`; never edit or delete a merged one. Every migration must apply on a fresh database (CI starts from zero) and leave objects owned by the application role.
- **Assets are fingerprinted automatically.** Pages reference `/assets/...?v=<content hash>`; never add a version query by hand.
- **The binary knows its commit.** `/healthz` and `/readyz` report `data.commit`.

## 3. Deploy

```
export KUNGFU_DEPLOY_HOST=<ssh host>   # production host (ssh config name)
scripts/deploy.sh                      # deploy origin/main
scripts/deploy.sh <sha> --dry-run      # build and show the plan, change nothing
scripts/deploy.sh --apply-migrations   # required when the release adds migrations
scripts/deploy.sh config-diff          # compare server config with deploy/
```

The script refuses anything not on `origin/main` or without green CI. It builds the exact commit from `git archive` with the commit stamped in, backs up the database before applying new migrations (as the application role), keeps the previous binary, moves the server checkout to the same commit, restarts, and rolls back automatically unless `/readyz` reports the new commit within 30 seconds. Every deploy is logged in `/var/log/kungfu-deploy.log` on the server.

Rollback = deploy an earlier commit of `main`: `scripts/deploy.sh <older-sha>` (migrations are not reverted; they are written to be backward compatible).

## 4. Production layout

| What | Where |
|---|---|
| Checkout (always the deployed commit, never edited) | `/var/www/kungfu.md` |
| Binary / service | `/var/www/kungfu.md/kungfu-server`, systemd `kungfu-go` |
| Environment | `/etc/kungfu-go.env` (see README for variables) |
| Previous binaries (5 newest) | `/var/backups/kungfu/binaries/` |
| Daily DB backups (14 days, 03:30) | `/var/backups/kungfu/daily/`, log `/var/log/kungfu-db-backup.log` |
| Deploy log | `/var/log/kungfu-deploy.log` |

Reference copies of the server configuration live in `deploy/` (nginx site and headers, systemd unit, backup script and cron). Change them in the repo first, apply on the server by hand, then check with `scripts/deploy.sh config-diff`. The nginx edge overwrites `X-Forwarded-For` (rate limits key on it), leaves JS/CSS caching to the app, and allows request bodies up to 1100k on every route (the Go entrances enforce their own 1 MiB caps).

PostgreSQL must run with timezone = UTC (`SHOW timezone;`). The admin session and other pre-2.0 tables use TIMESTAMP WITHOUT TIME ZONE with database-side defaults, while expiry checks use Go's clock; a non-UTC database shifts session idle/expiry checks by the offset.

Payment settings (Creem) are data, not configuration: `/samelabs/settings/payment`.

## 5. First admin in a new environment

There is no bootstrap endpoint or default password: the first platform admin is seeded as data, once, in one transaction. Generate the bcrypt hash without the password touching argv, the environment or shell history (the helper reads it from stdin):

```bash
docker run --rm -i -v "$PWD":/src -w /src kungfu-tools go run ./scripts/internal/bcrypt
```

```sql
BEGIN;
INSERT INTO tb_admins (username, display_name, password_hash, status)
VALUES ('<username>', '<display name>', '<bcrypt hash>', 'active');
INSERT INTO tb_admin_user_roles (admin_id, role_id)
SELECT a.id, r.id FROM tb_admins a, tb_admin_roles r
WHERE a.username = '<username>' AND r.code = 'superadmin';
COMMIT;
```

Further admins, roles and permissions are managed in `/samelabs`. For the local dev database, `scripts/dev.sh seed-admin NAME` does all of this.
