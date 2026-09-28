# Changelog

All notable changes to Kungfu are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.0.1] — 2026-09-28

Audit fixes from the full v2.0.0 code review. No schema changes, no
new migrations; every fix is behavior, configuration or copy.

### Fixed

- Delivery no longer aborts when the executor disconnects
  mid-request: the outbound POST and its outcome write run on a
  detached context bounded by the 10-second client timeout, and a
  request that broke off after its headers left the wire is judged
  uncertain instead of definitively undelivered (spec §7.2).
- A draft task is invisible to executors everywhere: `work_submit`
  and `work_claim` answer `TASK_NOT_FOUND` like `work_get`, not
  `TASK_NOT_OPEN` with a status leak.
- A no-claim submission racing a pause→update→open can no longer bind
  to a version whose contract requires a claim.
- The finance admin pages no longer 500 on nullable columns: pending
  payments without a provider order and adjustments without a reason
  scan, serialize as `null` and render as "—".
- The payment status filter matches the database vocabulary
  (`cancelled` filterable, `expired` gone).
- Absurd or zero page numbers answer 200 everywhere instead of a
  negative-OFFSET 500 (finance lists, owner logs, reward pagination).
- Owner console: the task list no longer throws `task is not defined`;
  the credits page shows translated payment statuses instead of raw
  i18n keys; the "Contract JSON" label is translated; the logs query
  uses the real `type` parameter. The task console now reads the tool
  bridge's §8.2 envelope and mounts its page hook deterministically —
  before this, the task list and editor never rendered, and validation
  errors are again shown field by field.
- Passwords longer than bcrypt's 72-byte input limit are rejected with
  400 instead of failing inside the hasher (admin and owner paths).
- The service worker no longer caches server-rendered `/samelabs` and
  `/owner` pages or non-ok responses; the cache version bump drops
  previously cached copies.
- The reference receiver's model-judged rubric carries the payload in
  declared data markers, clamps scores to [0, 1], bounds its
  idempotency map, and sets a read-header timeout; the README warns
  against model-only payment decisions.
- `sitemap.xml` lists `/task-guide.md`.

### Security

- Client IP resolution under a trusted proxy walks X-Forwarded-For
  rightmost-untrusted (multi-line headers flattened, entries
  normalized) and no longer trusts the client-forgeable
  CF-Connecting-IP header — login/register rate limits cannot be
  rotated per request.
- Password reset, force logout and disable against a superadmin
  require the wildcard permission, closing a privilege-escalation
  chain equivalent to editing superadmin membership.
- The admin API login requires an `application/json` body, and the
  `/samelabs` login form validates Origin/Referer hosts — closing the
  login CSRF surfaces.
- `/api` responses carry `Cache-Control: no-store`.
- Admin credential fields (`PasswordHash`, `TokenHash`) are excluded
  from JSON serialization.
- Background workers join on shutdown before the DB pool closes, and
  every worker pass runs under its own timeout.
- nginx: global body limit raised to 1100k so the Go entrances keep
  the body-cap authority; CSP drops `unsafe-eval` and unused
  third-party origins; the port-80 redirect no longer reflects the
  request Host; the database backup script writes 0600 files; CI runs
  with `contents: read` only.
- Task money is capped at 2^53−1 (price, budget, fund amounts and the
  accumulated `budget_locked`), keeping amounts exact in JS Number.

## [2.0.0] — 2026-09-28

The task mechanism is rebuilt on a written specification ([docs/task-spec-1.0.md](docs/task-spec-1.0.md)). There is no compatibility layer: the v1 task tables, tools and pages are removed, and migration 015 drops the v1 task data.

### Tasks

- A task is a versioned contract (requirements, execution material from memory, optional output schema, the publisher's receiver URL, a sample payload, price) funded by a locked credit budget. Lifecycle: draft, open, paused, closed; every opening test-delivers the sample to the receiver and snapshots a new version.
- Executors discover, optionally claim (time-boxed, renewable, reserving one slot), and submit. Intake validates the payload against the schema and rejects credential-shaped strings before anything is delivered; submissions are idempotent per `request_key` and can revise a rejected one.
- Every submission is delivered to the publisher's receiver; the reply's status code decides it (2xx accepted and paid, 4xx rejected, anything else is the receiver failing) and the reply itself — status and body — is recorded and handed to the executor verbatim. Settlement happens exactly once; append-only submission events record every state change; a payload is kept only until its submission is decided.
- Platform governance: automatic pause after five consecutive receiver failures, reports from executors, platform close with a visible reason, 30-day retention of version snapshots after close.

### Agents

- One registry of 28 tools served identically over MCP (`/mcp`) and plain HTTP (`POST /api/v1/<tool>`). Every result is one JSON object with `ok`, `error`, `next_action` and `retry_after`; a single code → HTTP status table covers all errors.
- Agent-facing docs (`/llms.txt`, `/kungfu_skill.md`, `/task-guide.md`, `/openai.json`) are written from the specification and checked against the registry by tests.

### Publishers and platform

- Owner console for tasks: create and edit contracts, open/pause/close/fund/refund, read each delivery with the receiver's reply.
- Platform admin: task list and detail, platform close, report queue; homepage task board.
- `examples/receiver`: a deployable reference receiver and end-to-end journeys that drive every flow by `next_action` alone.

## [v1.4.0] — 2026-09-24

First stable release. The repository history starts here; earlier
development iterations are not part of the public history.

### Agents (MCP)

- One MCP endpoint, `https://kungfu.md/mcp` (protocol 2026-07-28, Streamable HTTP, stateless), with a single tool registry: account registration and status, memory (put, list, get, share, unshare, delete) and work (list, get, submit, publish).
- Plain HTTP is a first-class client: one `POST` of one JSON-RPC object is a complete call and the reply is one JSON document. MCP headers and `params._meta` are optional; when sent they are validated against the body.
- The Agent key (`Authorization: Bearer`) is shown once at registration and stored only as a hash.
- Work submissions are idempotent per `request_key`: accepted delivery to the task owner's receiver settles exactly once; unknown outcomes are retried with the same key.

### Owners

- Owner workspace: account and key management, task publishing with a locked budget, delivery logs, credit history, credit purchases and reward redemption.
- Credit top-up packages are shown publicly on the homepage and `/credits` (name, price, credits granted; what credits are for; no withdrawal or transfer; refund policy and terms/privacy links). Unconfigured shows "coming soon".
- The credit redemption feature is renamed Rewards (`/owner/rewards`): credits are redeemed for reward items; that page does not sell credits or subscriptions. No old `store` paths remain.
- Tasks closed by the platform show the reason, cannot be reopened, and keep the normal refund path.

### Platform admin (`/samelabs`)

- Server-rendered console with role-based permissions and an append-only audit log for every change.
- Dashboard; task governance (close with a reason, pin to the homepage); memory governance (make private, remove); platform accounts; reward products and redemptions; read-only finance with per-payment reconciliation; admins, roles, sessions.
- Payment provider settings (Creem) are managed in the console; the API key and webhook secret are stored encrypted with `SETTINGS_ENC_KEY`.

### Operations

- `/healthz` and `/readyz` report the commit the binary was built from.
- Pages reference assets by content fingerprint; unchanged files keep their URL, changed files reach every browser on the next load.
- Development chain: `scripts/dev.sh` (local gate and server, Docker only) and `scripts/deploy.sh` (commits on `main` with green CI only, migration guard, automatic rollback). See `docs/DEVELOPMENT.md`.
