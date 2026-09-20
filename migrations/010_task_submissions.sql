-- 010: Task durable submissions — durable submission identity, budget
-- reservation, delivery outcome state machine, and settlement facts.
--
-- Base mechanism (fixed by PM):
--   1. short tx: accept submission (snapshot + reserve budget) -> COMMIT
--   2. HTTP delivery happens OUTSIDE any DB transaction
--   3. durable remote outcome record (delivered / rejected / uncertain)
--   4. settlement in its own single atomic transaction
--
-- Invariants:
--   available_budget = budget - reserved_budget  (admission basis)
--   reserved_budget  = sum(reserved_amount) over non-terminal submissions
--   one settlement   = task budget mutation + earn_task + state=settled
--                      in a single COMMIT, at most once per submission.
--
-- Fresh DB: applies after 001..009. Existing DB: additive only.

BEGIN;

-- tb_tasks: track budget reserved by accepted-but-not-terminal submissions.
ALTER TABLE tb_tasks
    ADD COLUMN reserved_budget BIGINT NOT NULL DEFAULT 0;

ALTER TABLE tb_tasks
    ADD CONSTRAINT ck_tasks_reserved_nonneg CHECK (reserved_budget >= 0);

-- reserved_budget must never exceed budget: enforced by CHECK. All writers
-- (reserve / settle / release) hold the task row lock, so the constraint
-- is maintained transactionally.
ALTER TABLE tb_tasks
    ADD CONSTRAINT ck_tasks_reserved_within_budget CHECK (reserved_budget <= budget);

-- Durable submission rows: the single source of truth for a submission's
-- identity, snapshot, delivery state, and settlement facts.
CREATE TABLE IF NOT EXISTS tb_task_submissions (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code               CHAR(12)  NOT NULL,

    task_id            BIGINT      NOT NULL,
    task_code          CHAR(12)    NOT NULL,
    bot_id             INTEGER     NOT NULL,
    kind               VARCHAR(16) NOT NULL,

    -- client -> Kungfu idempotency identity (client-stable request key)
    client_request_key VARCHAR(128) NOT NULL,

    -- canonical identity of the exact outbound payload
    payload_hash       CHAR(64)     NOT NULL,

    -- exact outbound JSON bytes; cleared on terminal settle/reject when no
    -- longer needed for recovery (sensitive-data hygiene), hash retained.
    payload_body       TEXT,

    -- accepted-at snapshots (never re-read from a later task row)
    postapi_snapshot   VARCHAR(2048) NOT NULL,
    price_snapshot     BIGINT        NOT NULL,

    -- reservation fact: always equals price_snapshot (CHECK below)
    reserved_amount    BIGINT        NOT NULL,

    -- reserved -> delivering -> (delivered -> settled) | rejected | uncertain
    state              VARCHAR(16)   NOT NULL,

    attempt_count      INTEGER       NOT NULL DEFAULT 0,
    lease_until        TIMESTAMPTZ,
    next_attempt_at    TIMESTAMPTZ,

    response_code      INTEGER,
    response_preview   TEXT,

    -- settlement replay facts (filled at settle time)
    settled_balance    BIGINT,
    budget_after       BIGINT,
    task_status_after  VARCHAR(10),

    last_error_code    VARCHAR(64),
    last_error_message VARCHAR(500),

    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    delivered_at       TIMESTAMPTZ,
    settled_at         TIMESTAMPTZ,

    CONSTRAINT uk_submission_identity
        UNIQUE (task_id, bot_id, kind, client_request_key),
    CONSTRAINT uk_submission_code UNIQUE (code),
    CONSTRAINT fk_submission_task
        FOREIGN KEY (task_id) REFERENCES tb_tasks (id) ON DELETE RESTRICT,
    CONSTRAINT fk_submission_bot
        FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_submission_state
        CHECK (state IN ('reserved','delivering','delivered','settled','rejected','uncertain')),
    CONSTRAINT ck_submission_kind
        CHECK (kind IN ('agent','owner_test')),
    CONSTRAINT ck_submission_reserved_matches_price
        CHECK (reserved_amount = price_snapshot),
    CONSTRAINT ck_submission_price_positive
        CHECK (price_snapshot > 0)
);

-- recovery / duplicate lookups
CREATE INDEX IF NOT EXISTS idx_submission_task_state
    ON tb_task_submissions (task_id, state);
CREATE INDEX IF NOT EXISTS idx_submission_recovery
    ON tb_task_submissions (state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_submission_bot
    ON tb_task_submissions (bot_id, created_at DESC);

COMMIT;
