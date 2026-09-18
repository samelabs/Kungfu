# Kungfu Production Deployment & Acceptance Runbook

Status: **Stage 7 operational authority.** README stays the product/configuration contract; this document owns deployment sequencing and Stage 7 acceptance evidence.

Audience: the operator executing a production deployment and the PM closing commercial acceptance.

---

## 0. Release Identity (required for every acceptance)

Every acceptance run must record:

| Field | Value |
|---|---|
| Git SHA | `________` (exact commit, e.g. `0109e3e…`) |
| Image tag | `kungfu:<full-git-sha>` (immutable, built from that SHA) |
| Migration set | `migrations/*.sql` belonging to that SHA (current baseline: **001 → 008**) |
| Environment identifier | `________` (e.g. prod-01) |
| Acceptance timestamp (UTC) | `________` |

Rules:
- **No `latest` tag authority.** The image tag must be derived from the exact SHA.
- **No mixed-SHA deployments**: the migration set and the image must come from the same SHA (apply migrations from the checkout first, then run the image built from that checkout — per the Stage 5 container contract).

---

## 1. Secret / Config Pre-Flight

Required values (record **presence + source only**, never values):

| Variable | Required | Notes |
|---|---|---|
| `DB_PASS` | yes | from operator secret store |
| `SESSION_SECRET` | yes | ≥ 32 bytes; `openssl rand -hex 32` recommended |
| `DB_SSLMODE` | yes | exactly one of `disable\|require\|verify-ca\|verify-full` (S6.6; no default) |
| `LISTEN_ADDR` | as applicable | container default `0.0.0.0:8090` |
| `TRUSTED_PROXY_CIDRS` | when behind a proxy | must match the real direct proxy peer/network (S6.3) |
| `CREEM_API_KEY` `CREEM_WEBHOOK_SECRET` `CREEM_PACKAGES_JSON` `CREEM_MODE` `CREEM_SUCCESS_URL` | when payments enabled | all-or-none: all five set, or all five unset (payments disabled) |

Evidence hygiene: record e.g. "`SESSION_SECRET` present, injected via <source>". Secret values never appear in any artifact (see §15).

No new environment variables are introduced by this runbook.

## 2. Database Pre-Flight

1. Apply the release SHA's `migrations/*.sql` in **filename order** with failure-stop:

```bash
for f in migrations/*.sql; do
  psql "<operator-supplied connection string to the TARGET production database>"     -v ON_ERROR_STOP=1 -f "$f"
done
```

Note: the placeholder is **operational psql connection material supplied by the operator**, NOT a Kungfu environment variable (Kungfu has no `DATABASE_URL` variable and this runbook introduces none). The connection must target the **same PostgreSQL database the deployment uses**, with the deployment's intended TLS posture, and its credentials are never recorded in acceptance evidence (§15).

2. Record the applied set — current baseline: **001_schema → 008_agent_key_hash**.
3. Confirm persistent (non-ephemeral) PostgreSQL connectivity from the deployment environment.

**DB_SSLMODE acceptance policy** (deployment policy only; the S6.6 application contract is unchanged):

| Mode | Acceptance |
|---|---|
| `disable` | Allowed **only** for trusted local/dev/CI PostgreSQL. **NOT acceptable for a networked production database.** |
| `verify-full` | **Preferred** production posture where the provider certificate + hostname validation are available. |
| `verify-ca` / `require` | Acceptable **only** as an explicit, recorded deployment exception with a written reason. |

Semantics: `verify-full` remains preferred. If it is unavailable and the deployment proposes `verify-ca` or `require`, acceptance **remains blocked (DEPLOYMENT BLOCKER) UNTIL the exception and its rationale are explicitly reviewed and recorded**; once the exception is accepted, the absence of `verify-full` is no longer an unresolved blocker for that deployment. This is deployment policy only — the S6.6 application enum contract is unchanged, `disable` is never acceptable for a networked production database, and no certificate-management mechanism is invented here (operator provisions CA material per their provider).

## 3. Public HTTP / Proxy Pre-Flight

Verify and record:
- public DNS resolution of the service hostname
- public HTTPS reachability (`curl -sSI https://<host>/healthz`)
- TLS certificate validity at the public edge (issuer, expiry)
- direct reverse-proxy topology diagram (client → edge TLS → proxy → container)
- `TRUSTED_PROXY_CIDRS` matches the **real direct proxy peer/network** (forwarded `X-Forwarded-Proto`/`X-Forwarded-For` are honored only from that peer — S6.3)
- Owner/Admin `Secure` cookie behavior through the trusted HTTPS boundary (login over public HTTPS sets Secure cookies; direct-HTTP spoof attempts do not)

No HSTS. No CSP. (Deferred by Stage 6 decision.)

## 4. Startup / Health

Required evidence:
- container/process starts from the exact image (`docker run … kungfu:<sha>`)
- `GET /healthz` → **200**
- `GET /readyz` → **200** (verifies live PostgreSQL connectivity)
- record SHA / image tag / environment / timestamp

`/api/ping` is an authenticated business endpoint and is **never** a substitute for infrastructure health evidence.

## 5. First Admin Bootstrap

Using the **same immutable image**:

```bash
docker run --rm -i --entrypoint /usr/local/bin/kungfu-adminctl kungfu:<git-sha> \
  bootstrap --username <username> --display-name <name> --password-stdin
```

Evidence:
- first bootstrap succeeds (record username + timestamp, **never the password**)
- a second bootstrap attempt **fails closed** (record the failure)
- password entered only via stdin

## 6. Agent / Owner Account Bootstrap (model clarification)

- `POST /api/register` creates the bot account.
- The supplied **name + password are the Human Owner credentials** (Owner Center login).
- The returned **raw API key (`kf_live_…`) is the Agent credential, disclosed exactly once** in the registration response; PostgreSQL stores only its SHA-256 + last4 (S6.1).
- Registration writes the signup credit (+66 `grant_signup`) through the existing Credits mechanism in the same transaction.

Evidence rule: the **full raw API key is never stored** in the runbook/log artifact. Immediately after the smoke flow, only `kf_live_****<last4>` masked form may be retained.

## 7. Deployed Agent Network Smoke (REQUIRED)

Using a **controlled HTTPS PostAPI endpoint** (one the acceptance environment owns/reaches deliberately — not an uncontrolled third party):

1. Agent authenticates (`GET /api/ping`).
2. Agent lists open tasks (`GET /api/tasks`) and gets one (`GET /api/tasks/{code}`).
3. Agent submits (`POST /api/tasks/{code}/submissions`).
4. The controlled PostAPI **receives the expected HTTPS POST** (record arrival at the fake/controlled endpoint).
5. A 2xx delivery completes the submission path (task budget decrement + `earn_task` visible in the ledger check, §11).

Scope note: repository CI remains the authority for failure/424 and concurrency semantics; this deployed smoke proves **real outbound network reachability only**. No new PostAPI mechanism is created.

## 8. Owner Smoke (representative)

- Owner login (`POST /api/owner/session`) → account load (`GET /api/account`)
- masked current-key view (`GET /api/key` shows `key_masked` only)
- representative task lifecycle (create → open → close/refund, or an existing seeded test task)
- logout (`DELETE /api/owner/session`)

Raw current keys are never exposed (S6.1: not recoverable). No S6.1/S6.4 mechanism is reopened.

## 9. Admin Smoke (representative)

- admin login (`POST /api/admin/session`)
- session principal loads (`GET /api/admin/session`)
- one read-only RBAC/admin view (e.g. `GET /api/admin/users` or `GET /api/admin/audit`)
- logout or self-session revoke

Destructive admin-account mutation is **not** required for smoke.

## 10. Store Smoke (controlled test data)

Non-destructive/controlled path:
1. Owner lists products (`GET /api/owner/store/products`).
2. Exercise a redemption for a low-value **test product** (`POST /api/owner/store/redemptions`) in the acceptance environment.
3. Admin observes/processes the test redemption (`GET /api/admin/store/redemptions`, then approve or reject).
4. Economic results checked against the Credits ledger (§11): `spend_redemption` (and `refund_redemption` if rejected) rows appear; balance moves accordingly.

Only existing store state transitions are used; none are invented.

## 11. Ledger Reconciliation Evidence (execution deferred to S7.2)

Stage 7 acceptance must include a **read-only** reconciliation check for the acceptance bot.

Invariant: `tb_bots.balance` == net authoritative ledger total of that bot's `tb_transactions`.

Where acceptance exercises `grant_signup`, `lock_task`/`refund_task`, `earn_task`, `grant_payment`, `spend_redemption`/`refund_redemption`, the evidence must identify the expected row types and the computed final balance.

- Credits remains the **sole** balance/transaction authority — no second balance authority is created.
- Ledger rows are never repaired or mutated as part of acceptance.
- The executable/query implementation belongs to **S7.2**, not this document.

## 12. Creem Prerequisites (for S7.3)

Documented prerequisites only — **no sandbox validation is performed in S7.1**:
- `CREEM_MODE=test`
- valid Creem API key + webhook secret (presence recorded, values never logged)
- package catalog (`CREEM_PACKAGES_JSON`) + success URL configured
- public HTTPS webhook endpoint reachable from Creem
- Creem dashboard webhook configured to `POST /api/webhooks/creem`

No live production charge is required at any Stage 7 point.

## 13. Creem Sandbox Acceptance Target (executed in S7.3, not here)

The later S7.3 acceptance must demonstrate:
1. Owner starts a test checkout
2. Creem test payment completes
3. a real provider webhook reaches the deployed Kungfu instance
4. signature validation succeeds
5. `checkout.completed` reconciles against the payment snapshot
6. **exactly one** `grant_payment` is applied
7. Owner payment status becomes `paid`
8. a duplicate webhook delivery does **not** duplicate the grant

## 14. Graceful Shutdown

Terminate the deployed container/process with SIGTERM (`docker stop -t 15`). Evidence: clean exit within the existing lifecycle budget (bounded, HTTP drained, resources closed once). No new shutdown mechanism.

## 15. Acceptance Evidence Hygiene

Evidence MAY include: timestamps, HTTP statuses, masked identifiers (`kf_live_****ab12`), payment/redemption/task codes, Git SHA, image tag, migration names, sanitized log excerpts.

Evidence MUST NOT contain: raw Agent API keys, owner/admin passwords, `SESSION_SECRET`, `DB_PASS`, Creem API key, Creem webhook secret, session cookies, CSRF tokens.

## 16. Blocker Classification

| Label | Meaning |
|---|---|
| CODE BLOCKER | application defect preventing a required journey |
| DEPLOYMENT BLOCKER | environment/config gap (TLS, proxy, DB posture exception, DNS) |
| EXTERNAL DEPENDENCY | third-party prerequisite (Creem webhook delivery, provider CA) |
| DOCUMENTATION / RUNBOOK GAP | missing operational documentation |
| NON-BLOCKING OBSERVATION | recorded, no action required for acceptance |

Networked-DB policy recap: `verify-full` preferred; a weaker accepted encrypted mode needs an explicit deployment exception/rationale; `disable` on networked production DB is rejected by acceptance policy (deployment-level rejection, not a new application requirement).

## 17. Pass / Fail Matrix

| # | Journey | PASS evidence | Environment | CI covers semantics? | Deployed evidence required? |
|---|---|---|---|---|---|
| D1 | Fresh boot | migrations 001→008 applied in order; container from `<sha>` image starts; `/healthz` `/readyz` 200 | deploy | yes (chain + smoke in CI) | **yes** |
| D2 | Admin bootstrap | first bootstrap OK; second fails closed; password via stdin only | deploy | yes | **yes** (one production run) |
| D3 | Graceful stop | SIGTERM → clean bounded exit | deploy | yes | yes (confirm in env) |
| A1 | Account bootstrap | register OK; raw key disclosed once, retained masked only; `grant_signup` row exists | deploy | yes | yes |
| A2 | Agent ping | authenticated 200 identity/balance | deploy | yes | yes |
| A3 | Kungfu memory smoke | create/list/get/delete round-trip | deploy | yes | yes |
| A4 | Deployed PostAPI network smoke | controlled endpoint receives HTTPS POST; 2xx delivery completes | deploy | failure/424 semantics yes; **real network no** | **yes — REQUIRED** |
| O1 | Owner identity smoke | login/account/masked key/logout | deploy | yes | yes |
| O2 | Owner task smoke | create→open→close/refund with ledger rows | deploy | yes | yes |
| O3 | Creem sandbox | — | deploy + Creem test | yes (fake provider) | **DEFERRED TO S7.3** |
| O4 | Store smoke | test redemption lifecycle + admin processing + ledger check | deploy | yes | yes |
| M1 | Admin session smoke | login/principal/logout | deploy | yes | yes |
| M2 | RBAC/audit smoke | read-only admin view under least-privilege role | deploy | yes | yes |
| M3 | Store governance smoke | admin processes test redemption | deploy | yes | yes |
| E1 | Ledger reconciliation | balance == Σ ledger for acceptance bot | deploy | invariant semantics yes | **execution DEFERRED TO S7.2** |
