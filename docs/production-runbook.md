# Kungfu Production Deployment & Acceptance Runbook

Status: **Operational authority for the current release.** README stays the product/configuration contract; this document owns deployment sequencing, cutover, backup, rollback policy, and acceptance evidence.

Audience: the operator executing a production deployment and the PM closing commercial acceptance.

**Routine releases** use `scripts/deploy.sh` (see [DEVELOPMENT.md](DEVELOPMENT.md)): it builds the exact `main` commit with the commit stamped into the binary, backs up the database before new migrations, applies them as the application role, and rolls back automatically if `/readyz` does not report the new commit. Production runs the binary under systemd (`kungfu-go`); the container image is built and smoke-tested by CI only. This runbook remains the authority for first-time cutovers and commercial acceptance evidence.

---

## 0. Release Identity (required for every acceptance)

Every acceptance run must record:

| Field | Value |
|---|---|
| Git SHA | `________` (exact commit, e.g. `0109e3e…`) |
| Image tag | `kungfu:<full-git-sha>` (immutable, built from that SHA) |
| Migration set | `migrations/*.sql` belonging to that SHA (current baseline: **001 → 010**) |
| Environment identifier | `________` (e.g. prod-01) |
| Acceptance timestamp (UTC) | `________` |

Current release migration set (application order):

```
001_schema.sql
002_payments.sql
003_store_redemption.sql
004_payment_provider_product.sql
005_payment_adjustments.sql
006_admin_foundation.sql
007_store_admin_permissions.sql
008_agent_key_hash.sql
009_integer_credits.sql
010_task_submissions.sql
011_admin_account_permissions.sql
012_admin_finance_permission.sql
013_payment_settings.sql
014_admin_operations_permissions.sql
```

Rules:
- **No `latest` tag authority.** The image tag must be derived from the exact SHA.
- **No mixed-SHA deployments**: the migration set and the image must come from the same SHA (apply migrations from the checkout first, then run the image built from that checkout).
- **No new migrations are introduced by this runbook.**

---

## 1. Secret / Config Pre-Flight

Required values (record **presence + source only**, never values):

| Variable | Required | Notes |
|---|---|---|
| `DB_PASS` | yes | from operator secret store |
| `SESSION_SECRET` | yes | ≥ 32 bytes; `openssl rand -hex 32` recommended |
| `DB_SSLMODE` | yes | exactly one of `disable\|require\|verify-ca\|verify-full` (no default) |
| `LISTEN_ADDR` | as applicable | container default `0.0.0.0:8090` |
| `TRUSTED_PROXY_CIDRS` | when behind a proxy | must match the real direct proxy peer/network |
| `SETTINGS_ENC_KEY` | when payments enabled | 64 hex chars; encrypts the Creem secrets stored in the database. Must stay stable across deploys. Any leftover `CREEM_*` variable fails startup — Creem settings now live in `/samelabs/settings/payment` |

Evidence hygiene: record e.g. "`SESSION_SECRET` present, injected via <source>". Secret values never appear in any artifact (see §17).

No new environment variables are introduced by this runbook.

## 2. Production Cutover Sequence (authoritative order)

The cutover MUST proceed in exactly this order. Each gate is recorded as PASS before the next step starts. Any gate failing → **STOP** (see §4b rollback/failure semantics).

```
preflight (secret/config + public HTTP/proxy checks)
  → stop/drain OLD application (SIGTERM; confirm write path is down)
  → verify production DB identity — FIRST check (backup target)
  → fresh immediate pre-release backup
  → verify backup (SHA256 + pg_restore -l readable)
  → verify production DB identity — SECOND check (migration target)
  → migration 009 strict preflight
  → apply 009 (integer Credits)
  → verify 009 (schema/invariants)
  → apply 010 (task submissions)
  → verify 010 (schema/invariants)
  → runtime-role privilege verification (§4c)
  → start EXACT accepted new binary/image
  → readiness (/healthz /readyz)
  → controlled acceptance (§6–§15)
```

The first DB identity check (`SHOW port;` / `SELECT current_database();`) MUST
execute BEFORE the backup command, so the backed-up database is exactly the
intended production target. After backup verification completes and BEFORE the
009 preflight, the SAME identity check is executed a second time, confirming
the migration target. A mismatch at EITHER check → **STOP**.

Hard rules:

- **Stop-before-009**: migration 009 MUST NOT execute while the old application can still write the production database. Stop/drain the old application first (graceful SIGTERM per §14, then confirm the process/container is terminated and no worker remains connected) BEFORE any migration command. The old application write path must be fully down before 009 runs — no exceptions.
- **DB identity gate (twice)**: the identity check (`SHOW port;` + `SELECT current_database();`) runs BEFORE the backup command and AGAIN after backup verification, before the 009 preflight. The backed-up database and the migrated database must both be exactly the intended production target. A mismatch at either check → STOP.
- **No test/production mixing**: test environments must always address PostgreSQL explicitly at `127.0.0.1:55432`; never rely on an implicit default port for test DB commands, and never point test tooling at the production database. Production connection material is operator-supplied and recorded by identity (port + database name), never by credential in evidence.
- **Order is unique**: 009 is applied and verified BEFORE 010; 010 is applied only after 009's verification passes; the new application starts only after both migrations are applied and verified.

### 2.1 Production migration execution path (existing production: applied prefix 001–008)

Current production already carries migrations **001–008 applied**. This
cutover executes EXACTLY the unapplied tail, one file at a time, with
verification between the two:

```bash
# 009 first (after strict preflight §3 and both identity checks §2.2)
psql "<operator-supplied production connection material>" -v ON_ERROR_STOP=1 -f migrations/009_integer_credits.sql
# → verify 009 (§3 post-verification) — must PASS before 010
psql "<operator-supplied production connection material>" -v ON_ERROR_STOP=1 -f migrations/010_task_submissions.sql
# → verify 010 (§4 post-verification)
```

- Every command uses `-v ON_ERROR_STOP=1` and operator-supplied **production**
  connection material.
- **The current production cutover MUST NOT re-execute 001–008** and MUST NOT
  loop over `migrations/*.sql` as a single batch command.

Fresh installation (separate path, never mixed with this cutover): a brand-new
database installs the full set **001→010 in filename order** with the same
failure-stop discipline. This fresh-install note does not apply to the
existing-production cutover above.

Note: the placeholder is **operational psql connection material supplied by the operator**, NOT a Kungfu environment variable (Kungfu has no `DATABASE_URL` variable and this runbook introduces none). The connection must target the **same PostgreSQL database the deployment uses**, with the deployment's intended TLS posture, and its credentials are never recorded in acceptance evidence (§17).

Record the applied set — current baseline: **001_schema → 010_task_submissions** (existing production: 001–008 pre-applied; 009 + 010 applied by this cutover).

### 2.2 Production DB identity gate (executed TWICE)

Execute against the TARGET production database and record the output — once
before the backup command (backup target), and again after backup
verification, before the 009 preflight (migration target):

```sql
SHOW port;
SELECT current_database();
```

Both executions must match the operator's intended production target. A
mismatch at either point → **STOP**.

### 2.3 Backup gate (immediate pre-release)

With the FIRST identity check passed, create a **fresh immediate pre-release
backup** of the production database. Historical backups are NOT a substitute
for this release's restore point.

Record:
- backup file path
- creation timestamp (UTC)
- SHA256 checksum of the backup file
- verification that `pg_restore -l <backup>` reads the archive successfully (catalog listing captured as evidence)

Backup verification is followed by the SECOND identity check (§2.2) before any
migration command runs.

## 3. Migration 009 — Integer Credits (strict)

009 is the **integer Credits strict migration**: Credits become whole integer units stored as BIGINT (fiat minor units untouched). The migration file owns its own transaction; any failure at any point rolls back the entire file atomically.

**Strict preflight** (the migration itself fails closed on the same conditions — the preflight is run and recorded first):

- every affected credits column holds only integral values; **a single fractional historical value aborts the migration**. The preflight covers the complete set of all 8 Credits economic columns:
  - `tb_bots.balance`
  - `tb_tasks.budget`, `tb_tasks.price`
  - `tb_transactions.amount`, `tb_transactions.balance_after`
  - `tb_payments.credits`
  - `tb_store_products.credits_price`
  - `tb_redemptions.credits_cost`
- to make the migration pass it is **forbidden** to: round, truncate, rewrite, or silently coerce existing economic data
- preflight failure → **STOP**. The migration is not applied. The data condition is escalated to the PM; no coercion workaround exists.

**Post-009 verification** (record as evidence):

- ALL 8 Credits economic columns are now BIGINT (none omitted, Store included):
  - `tb_bots.balance`
  - `tb_tasks.budget`, `tb_tasks.price`
  - `tb_transactions.amount`, `tb_transactions.balance_after`
  - `tb_payments.credits`
  - `tb_store_products.credits_price`
  - `tb_redemptions.credits_cost`
- economic invariants hold: row counts and relationships preserved across the conversion; signs preserved (negative ledger rows / authoritative negative balances stay negative); the ledger reconciliation invariant (§13) still computes.

## 4. Migration 010 — Task Submissions

010 is applied **only after 009 verification passed**.

**Post-010 verification** (record as evidence):

- task reserved-budget schema: `tb_tasks.reserved_budget` exists (BIGINT NOT NULL DEFAULT 0) with its CHECK constraints (`reserved_budget >= 0`, `reserved_budget <= budget`)
- durable task submissions: `tb_task_submissions` table exists with its state/recovery/bot indexes
- required constraints and foreign keys present (`task_id → tb_tasks ON DELETE RESTRICT`, `bot_id → tb_bots ON DELETE RESTRICT`)
- admission basis: `available_budget = budget - reserved_budget`

**The new application MUST NOT be started before 009 AND 010 are applied and verified.**

**The new application MUST NOT be started before the §4c runtime-role
privilege verification passes** — schema verification alone (and `/readyz`
connectivity alone) does not prove the runtime `DB_USER` can operate the
new/changed objects.

## 4c. Runtime-Role Privilege Verification Gate (between migration verification and application start)

Migration schema verification proves the schema exists — it does NOT prove
the runtime database role can actually operate on new/changed objects. This
gate closes that gap and MUST pass before the new application starts.

Position in the sequence (authoritative):

```
migration schema verification (§3/§4)
  → runtime-role privilege verification (this section)
  → only then start the new application
```

`/readyz` only proves database connectivity; it can NEVER substitute for
business-table privilege verification.

Mechanism:

- The runtime role is the **actually configured `DB_USER`** (from the
  deployment's environment/secret store). Never hardcode a role name into
  this gate as a general mechanism.
- When migrations are executed by an operator/schema-owner role (a different
  role than `DB_USER`), objects created by the migration are owned by that
  operator role, and the runtime role may lack privileges on them. After the
  migration and before starting the new application, the operator MUST
  explicitly verify the runtime role holds the privileges the application
  actually needs on new/changed objects.
- Verify ACTUAL required privileges only — least privilege, derived from the
  application's real operations and PostgreSQL authority. Do NOT grant
  ALL PRIVILEGES as a standard, and do not over-grant just because a prior
  incident fix once granted a wider set.
- If a privilege is missing: the operator grants exactly the required
  least-privilege set, then re-runs this verification. Only a passing
  re-verification proceeds to application start. Failure → STOP.

Example — current release's new table `tb_task_submissions` (application
operations: SELECT / INSERT / UPDATE; the application never DELETEs or
TRUNCATEs submissions):

Invocation (psql variable interpolation — never hand-concatenate `DB_USER`
into SQL text):

```sh
psql "<operator production connection material>" \
  -v ON_ERROR_STOP=1 \
  -v runtime_role="$DB_USER" \
  -f runtime_privilege_check.sql
```

```sql
-- runtime_privilege_check.sql — run as a superuser/operator role;
-- :'runtime_role' is psql SQL-literal-safe interpolation of the configured DB_USER
SELECT has_table_privilege(:'runtime_role', 'tb_task_submissions', 'SELECT') AS sel,
       has_table_privilege(:'runtime_role', 'tb_task_submissions', 'INSERT') AS ins,
       has_table_privilege(:'runtime_role', 'tb_task_submissions', 'UPDATE') AS upd;
-- all three must be true
```

Dependent objects (identity columns / sequences):

- Tables with `GENERATED ALWAYS AS IDENTITY` or `SERIAL` columns may also
  require sequence privileges (`USAGE`) for the runtime role, depending on
  PostgreSQL version/authority semantics. Check dependent sequences with the
  same psql SQL-literal-safe interpolation — never a bare `:runtime_role` as a
  function string/name argument:
  `has_sequence_privilege(:'runtime_role', '<actual_sequence_name>', 'USAGE')`.
  The sequence name MUST come from the actual catalog / identity dependency
  (e.g. `pg_depend` / `pg_get_serial_sequence('tb_task_submissions','id')`),
  not from a guessed `<table>_id_seq` convention — the catalog is the
  authority. Also verify with a controlled runtime-role INSERT (or defer to
  the actual PostgreSQL error authority) and grant sequence privileges ONLY
  if the application truly needs them. Do not mechanically widen grants for
  objects the application never touches.

Evidence: record the runtime role name (by name only, never its credential),
the verified object/privilege matrix, and PASS/FAIL per gate.

## 4b. Rollback / Failure Semantics

If 009/010 have been forward-applied and the new application fails to start:

- **Do NOT simply restart the old binary.** A binary built on old schema assumptions must never run blind against a forward-migrated database.
- Choose, with the PM, one of:
  1. **Coordinated DB restore + old binary**: stop everything, restore the §2.3 immediate pre-release backup, verify restore, then start the old binary against the restored old-schema database; or
  2. **Fix-forward**: diagnose and fix the new application, build a new exact-SHA image, re-run readiness + acceptance.
- Record the decision, the evidence, and the final state. Partial states (009 applied, 010 not yet) follow the same rule: no old binary against a forward-migrated schema.

## 5. Startup / Health

Required evidence:
- container/process starts from the exact image (`docker run … kungfu:<sha>`)
- `GET /healthz` → **200**
- `GET /readyz` → **200** (verifies live PostgreSQL connectivity)
- record SHA / image tag / environment / timestamp

`/healthz` and `/readyz` are the infrastructure health evidence; there is no other health surface.

## 6. First Admin Seed (operator data initialization)

There is NO application bootstrap mechanism: the server exposes no
"empty database → create first admin" path, and no bootstrap binary
ships in the image. The first admin is plain operator-owned seed data:

```sql
-- operator console, production database, one-time:
-- The whole seed (admin row + superadmin binding) is ONE transaction:
-- either both commit, or a failure leaves nothing behind (ROLLBACK).
BEGIN;

INSERT INTO tb_admins (username, display_name, password_hash, status)
VALUES ('<username>', '<display name>', '<bcrypt hash>', 'active');

INSERT INTO tb_admin_user_roles (admin_id, role_id)
SELECT a.id, r.id FROM tb_admins a, tb_admin_roles r
WHERE a.username = '<username>' AND r.code = 'superadmin';

COMMIT;  -- on any error: ROLLBACK; and re-seed — no partial admin remains
```

The bcrypt hash MUST be generated offline WITHOUT the plaintext
password ever entering argv, environment, or logs. Read it from stdin
instead — e.g. on the operator workstation:

```bash
python3 - <<'PY'
import bcrypt, getpass
pw = getpass.getpass("admin password: ")          # stdin, not argv/env
print(bcrypt.hashpw(pw.encode(), bcrypt.gensalt(rounds=10)).decode())
PY
```

The printed hash is then pasted into the seed transaction above. The
plaintext password never appears in argv, environment, command
history, or logs.

Evidence:
- exactly one admin row exists after seeding (record username + timestamp, **never the password**)
- the seeded admin can log in (`POST /api/samelabs/session`) and holds the superadmin role

## 7. Agent / Owner Account Bootstrap (model clarification)

- `POST /api/owner/register` creates the bot account (Owner browser registration path).
- The supplied **name + password are the Human Owner credentials** (Owner Center login).
- The returned **raw Agent key (`kf_live_…`) is the Agent credential, disclosed exactly once** in the registration response; PostgreSQL stores only its SHA-256 + last4.
- Registration writes the signup credit (+66 `grant_signup`) through the existing Credits mechanism in the same transaction.

Evidence rule: the **full raw Agent key is never stored** in the runbook/log artifact. Immediately after the smoke flow, only `kf_live_****<last4>` masked form may be retained.

## 8. Deployed Agent Network Smoke (REQUIRED — MCP only)

Using a **controlled HTTPS PostAPI endpoint** (one the acceptance environment owns/reaches deliberately — not an uncontrolled third party):

1. Agent calls the MCP endpoint `POST /mcp` (`tools/call account_status`, authenticated with `Authorization: Bearer <Agent key>`).
2. Agent lists open work (`work_list`) and gets one (`work_get`).
3. Agent submits (`work_submit`).
4. The controlled PostAPI **receives the expected HTTPS POST** (record arrival at the fake/controlled endpoint).
5. A 2xx delivery completes the submission path (task budget decrement + `earn_task` visible in the ledger check, §13).

MCP is the single Agent execution interface: `account_status`, `memory_put`/`memory_list`/`memory_get`/`memory_delete`, `work_list`/`work_get`/`work_submit`. No Agent REST, `/api/ping`, or X-Bot-Key Agent execution flow exists or may be reintroduced in acceptance.

Scope note: repository CI remains the authority for failure/424 and concurrency semantics; this deployed smoke proves **real outbound network reachability only**. No new PostAPI mechanism is created.

## 9. Owner Smoke (representative)

- Owner login (`POST /api/owner/session`) → account load (`GET /api/account`)
- masked current-key view (`GET /api/key` shows `key_masked` only)
- representative task lifecycle (create → open → close/refund, or an existing seeded test task)
- logout (`DELETE /api/owner/session`)

Raw current keys are never exposed (not recoverable). No key-security mechanism is reopened.

## 10. Admin Smoke (representative)

- admin login (`POST /api/samelabs/session`)
- session principal loads (`GET /api/samelabs/session`)
- one read-only RBAC/admin view (e.g. `GET /api/samelabs/users` or `GET /api/samelabs/audit`)
- logout or self-session revoke

Destructive admin-account mutation is **not** required for smoke.

## 11. Store Smoke (controlled test data)

Non-destructive/controlled path:
1. Owner lists products (`GET /api/owner/store/products`).
2. Exercise a redemption for a low-value **test product** (`POST /api/owner/store/redemptions`) in the acceptance environment.
3. Admin observes/processes the test redemption (`GET /api/samelabs/store/redemptions`, then approve or reject).
4. Economic results checked against the Credits ledger (§13): `spend_redemption` (and `refund_redemption` if rejected) rows appear; balance moves accordingly.

Only existing store state transitions are used; none are invented.

## 12. Creem Prerequisites (documented; sandbox acceptance is a separately scheduled gate)

Documented prerequisites only — no sandbox validation is performed by this document:
- `SETTINGS_ENC_KEY` set on the server
- in `/samelabs/settings/payment`: mode `test`, valid Creem API key + webhook secret (stored encrypted, shown masked), package catalog + success URL, checkout switched on
- public HTTPS webhook endpoint reachable from Creem
- Creem dashboard webhook configured to `POST /api/webhooks/creem`

No live production charge is required at any point in this runbook.

## 12b. Creem Sandbox Acceptance Target (separately scheduled gate, not this runbook)

The separately scheduled Creem sandbox acceptance must demonstrate:
1. Owner starts a test checkout
2. Creem test payment completes
3. a real provider webhook reaches the deployed Kungfu instance
4. signature validation succeeds
5. `checkout.completed` reconciles against the payment snapshot
6. **exactly one** `grant_payment` is applied
7. Owner payment status becomes `paid`
8. a duplicate webhook delivery does **not** duplicate the grant

## 13. Ledger Reconciliation Evidence (read-only check in this runbook)

Acceptance must include a **read-only** reconciliation check for the acceptance bot.

Invariant: `tb_bots.balance` == net authoritative ledger total of that bot's `tb_transactions`.

Where acceptance exercises `grant_signup`, `lock_task`/`refund_task`, `earn_task`, `grant_payment`, `spend_redemption`/`refund_redemption`, the evidence must identify the expected row types and the computed final balance.

- Credits remains the **sole** balance/transaction authority — no second balance authority is created.
- Ledger rows are never repaired or mutated as part of acceptance.

## 14. Graceful Shutdown

Terminate the deployed container/process with SIGTERM (`docker stop -t 15`). Evidence: clean exit within the existing lifecycle budget (bounded, HTTP drained, resources closed once). No new shutdown mechanism.

## 15. Public HTTP / Proxy Checks

Verify and record:
- public DNS resolution of the service hostname
- public HTTPS reachability (`curl -sSI https://<host>/healthz`)
- TLS certificate validity at the public edge (issuer, expiry)
- direct reverse-proxy topology diagram (client → edge TLS → proxy → container)
- `TRUSTED_PROXY_CIDRS` matches the **real direct proxy peer/network** (forwarded `X-Forwarded-Proto`/`X-Forwarded-For` are honored only from that peer)
- Owner/Admin `Secure` cookie behavior through the trusted HTTPS boundary (login over public HTTPS sets Secure cookies; direct-HTTP spoof attempts do not)

No HSTS. No CSP. (Deferred by prior product decision.)

## 16. Blocker Classification

| Label | Meaning |
|---|---|
| CODE BLOCKER | application defect preventing a required journey |
| DEPLOYMENT BLOCKER | environment/config gap (TLS, proxy, DB posture exception, DNS) |
| EXTERNAL DEPENDENCY | third-party prerequisite (Creem webhook delivery, provider CA) |
| DOCUMENTATION / RUNBOOK GAP | missing operational documentation |
| NON-BLOCKING OBSERVATION | recorded, no action required for acceptance |

Networked-DB policy recap: `verify-full` preferred; a weaker accepted encrypted mode needs an explicit deployment exception/rationale; `disable` on networked production DB is rejected by acceptance policy (deployment-level rejection, not a new application requirement).

**DB_SSLMODE acceptance policy** (deployment policy only; the application enum contract is unchanged):

| Mode | Acceptance |
|---|---|
| `disable` | Allowed **only** for trusted local/dev/CI PostgreSQL. **NOT acceptable for a networked production database.** |
| `verify-full` | **Preferred** production posture where the provider certificate + hostname validation are available. |
| `verify-ca` / `require` | Acceptable **only** as an explicit, recorded deployment exception with a written reason. |

Semantics: `verify-full` remains preferred. If it is unavailable and the deployment proposes `verify-ca` or `require`, acceptance **remains blocked (DEPLOYMENT BLOCKER) UNTIL the exception and its rationale are explicitly reviewed and recorded**; once the exception is accepted, the absence of `verify-full` is no longer an unresolved blocker for that deployment.

## 17. Acceptance Evidence Hygiene

Evidence MAY include: timestamps, HTTP statuses, masked identifiers (`kf_live_****ab12`), payment/redemption/task codes, Git SHA, image tag, migration names, backup path + SHA256, sanitized log excerpts.

Evidence MUST NOT contain: raw Agent keys, owner/admin passwords, `SESSION_SECRET`, `DB_PASS`, Creem API key, Creem webhook secret, session cookies, CSRF tokens, or any database credential.

## 18. Pass / Fail Matrix

| # | Journey | PASS evidence | Environment | CI covers semantics? | Deployed evidence required? |
|---|---|---|---|---|---|
| D1 | Release identity & boot | exact accepted SHA/image deployed; existing production migration prefix 001–008 confirmed; required unapplied tail 009/010 completed per D3/D4 (no 001–008 re-run); container from `<sha>` image starts; `/healthz` `/readyz` 200 | deploy | fresh-install 001→010 chain yes (CI); existing-production cutover per D2–D5 | **yes** |
| D2 | Cutover gates | stop-old-app-before-009 recorded; backup path + timestamp + SHA256 + `pg_restore -l` readable recorded; DB identity (`SHOW port`, `current_database()`) recorded and matching | deploy | no (operational) | **yes** |
| D3 | Migration 009 | strict preflight PASS (no fractional data, no coercion); ALL 8 Credits economic columns BIGINT; invariants verified | deploy | conversion semantics yes | **yes** |
| D4 | Migration 010 | applied after 009 verified; reserved-budget schema + constraints + indexes + FKs verified | deploy | schema semantics yes | **yes** |
| D4b | Runtime-role privileges | §4c gate: runtime role (configured DB_USER) verified to hold application-required privileges (e.g. SELECT/INSERT/UPDATE on new tables; sequences only if truly needed) on all new/changed objects; missing privileges granted least-privilege by operator then re-verified | deploy | no (operational) | **yes** |
| D5 | New binary start | exact accepted SHA image starts AFTER 009+010 verified AND §4c privilege gate passed; `/healthz` `/readyz` 200 | deploy | yes (smoke in CI) | **yes** |
| D6 | Rollback readiness | failure semantics acknowledged; no old binary against forward-migrated DB | deploy | no (policy) | yes (recorded acknowledgment) |
| D7 | Graceful stop | SIGTERM → clean bounded exit | deploy | yes | yes (confirm in env) |
| A1 | Account bootstrap | register OK; raw key disclosed once, retained masked only; `grant_signup` row exists | deploy | yes | yes |
| A2 | MCP account status smoke | authenticated `account_status` tool call returns identity/balance (`Authorization: Bearer <Agent key>` on `/mcp`) | deploy | yes | yes |
| A3 | MCP memory smoke | `memory_put` / `memory_list` / `memory_get` / `memory_delete` round-trip via `/mcp` | deploy | yes | yes |
| A4 | Deployed PostAPI network smoke | controlled endpoint receives HTTPS POST; 2xx delivery completes | deploy | failure/424 semantics yes; **real network no** | **yes — REQUIRED** |
| O1 | Owner identity smoke | login/account/masked key/logout | deploy | yes | yes |
| O2 | Owner task smoke | create→open→close/refund with ledger rows | deploy | yes | yes |
| O3 | Creem sandbox | — | deploy + Creem test | yes (fake provider) | **separately scheduled gate** |
| O4 | Store smoke | test redemption lifecycle + admin processing + ledger check | deploy | yes | yes |
| M1 | Admin session smoke | login/principal/logout | deploy | yes | yes |
| M2 | RBAC/audit smoke | read-only admin view under least-privilege role | deploy | yes | yes |
| M3 | Store governance smoke | admin processes test redemption | deploy | yes | yes |
| E1 | Ledger reconciliation | balance == Σ ledger for acceptance bot | deploy | invariant semantics yes | yes (read-only execution per §13) |
