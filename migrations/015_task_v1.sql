-- 015: Task mechanism 1.0 (WO-1) — data model kernel.
--
-- Spec: docs/task-spec-1.0.md §2 (entities), §3 (Contract), §5.4 (submission
-- states), §8.4 (failure reasons), §10 (invariants). The old task tables are
-- dropped in the same migration (no compatibility, per the development plan).
--
-- Money columns are BIGINT whole credits. Status columns are CHECK-constrained
-- to the spec enums. All objects are owned by the role executing this
-- migration (the application role); no role name is hardcoded and SET ROLE is
-- never used.
--
-- tb_task_submission_event is append-only: row-level triggers reject UPDATE
-- and DELETE (spec §10 item 9: "事件只追加").

BEGIN;

-- ============================================================
-- Drop the old task model (v1.x). CASCADE removes the objects
-- that exist only to serve these tables (FKs, indexes).
-- ============================================================

DROP TABLE IF EXISTS tb_task_logs CASCADE;
DROP TABLE IF EXISTS tb_task_submissions CASCADE;
DROP TABLE IF EXISTS tb_tasks CASCADE;

-- ============================================================
-- tb_task — spec §2 Task
--   code, publisher, status, version (current effective version;
--   0 = none yet), budget_locked, settled, reserved, refunded,
--   paused_reason, closed_reason.
--
--   Derived, not stored: available = budget_locked − settled −
--   reserved − refunded (spec §4). Invariant §10.1 (available ≥ 0)
--   is enforced by ck_task_budget_closed.
-- ============================================================

CREATE TABLE tb_task (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code          VARCHAR(32)  NOT NULL,
    publisher_id  BIGINT       NOT NULL,
    status        VARCHAR(8)   NOT NULL DEFAULT 'draft',
    version       INTEGER      NOT NULL DEFAULT 0,

    budget_locked BIGINT       NOT NULL DEFAULT 0,
    settled       BIGINT       NOT NULL DEFAULT 0,
    reserved      BIGINT       NOT NULL DEFAULT 0,
    refunded      BIGINT       NOT NULL DEFAULT 0,

    paused_reason VARCHAR(64)  DEFAULT NULL,
    closed_reason VARCHAR(500) DEFAULT NULL,

    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT uk_task_code UNIQUE (code),
    CONSTRAINT fk_task_publisher FOREIGN KEY (publisher_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_task_status
        CHECK (status IN ('draft', 'open', 'paused', 'closed')),
    CONSTRAINT ck_task_budget_locked_nonneg CHECK (budget_locked >= 0),
    CONSTRAINT ck_task_settled_nonneg       CHECK (settled >= 0),
    CONSTRAINT ck_task_reserved_nonneg      CHECK (reserved >= 0),
    CONSTRAINT ck_task_refunded_nonneg      CHECK (refunded >= 0),
    CONSTRAINT ck_task_budget_closed
        CHECK (budget_locked >= settled + reserved + refunded),
    CONSTRAINT ck_task_version_nonneg CHECK (version >= 0)
);

CREATE INDEX idx_task_publisher ON tb_task (publisher_id);
CREATE INDEX idx_task_status   ON tb_task (status);

-- ============================================================
-- tb_task_version — spec §2 TaskVersion
--   Immutable snapshot of the Contract (§3, whole JSONB) plus the
--   Harness snapshot, effective for one task version.
-- ============================================================

CREATE TABLE tb_task_version (
    task_id    BIGINT       NOT NULL,
    version    INTEGER      NOT NULL,
    contract   JSONB        NOT NULL,
    harness    JSONB        NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    PRIMARY KEY (task_id, version),
    CONSTRAINT fk_task_version_task FOREIGN KEY (task_id)
        REFERENCES tb_task (id) ON DELETE CASCADE,
    CONSTRAINT ck_task_version_positive CHECK (version >= 1)
);

-- ============================================================
-- tb_task_claim — spec §2 Claim
--   claim_id, task, agent, version, expires_at, deadline (renewal
--   cap), amount, status ∈ {active, used, expired, released}.
--
--   At most one active claim per (task, agent) (spec §5.2) is
--   enforced by a partial unique index.
-- ============================================================

CREATE TABLE tb_task_claim (
    claim_id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    BIGINT       NOT NULL,
    agent_id   BIGINT       NOT NULL,
    version    INTEGER      NOT NULL,
    expires_at TIMESTAMPTZ  NOT NULL,
    deadline   TIMESTAMPTZ  NOT NULL,
    amount     BIGINT       NOT NULL,
    status     VARCHAR(8)   NOT NULL DEFAULT 'active',

    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT fk_claim_task FOREIGN KEY (task_id)
        REFERENCES tb_task (id) ON DELETE CASCADE,
    CONSTRAINT fk_claim_agent FOREIGN KEY (agent_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_claim_status
        CHECK (status IN ('active', 'used', 'expired', 'released')),
    CONSTRAINT ck_claim_amount_positive CHECK (amount > 0),
    CONSTRAINT ck_claim_version_positive CHECK (version >= 1)
);

CREATE UNIQUE INDEX uk_claim_one_active_per_agent
    ON tb_task_claim (task_id, agent_id) WHERE status = 'active';
CREATE INDEX idx_claim_agent ON tb_task_claim (agent_id);
CREATE INDEX idx_claim_task  ON tb_task_claim (task_id, status);

-- ============================================================
-- tb_task_submission — spec §2 Submission
--   submission_id, task, version, agent, request_key, payload,
--   payload_hash, amount (unit price at acceptance), state,
--   verdict, failure, revises, claim_id, timestamps.
--
--   Unique (agent_id, task_id, request_key) — spec §10 item 6.
-- ============================================================

CREATE TABLE tb_task_submission (
    submission_id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id         BIGINT       NOT NULL,
    version         INTEGER      NOT NULL,
    agent_id        BIGINT       NOT NULL,
    request_key     VARCHAR(128) NOT NULL,

    payload         JSONB        DEFAULT NULL,
    payload_hash    CHAR(64)     NOT NULL,
    amount          BIGINT       NOT NULL,

    state           VARCHAR(16)  NOT NULL,
    verdict         JSONB        DEFAULT NULL,
    failure         VARCHAR(32)  DEFAULT NULL,

    revises         BIGINT       DEFAULT NULL,
    claim_id        BIGINT       DEFAULT NULL,
    review_deadline TIMESTAMPTZ  DEFAULT NULL,

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ  DEFAULT NULL,

    CONSTRAINT uk_submission_agent_task_key UNIQUE (agent_id, task_id, request_key),
    CONSTRAINT fk_submission_task FOREIGN KEY (task_id)
        REFERENCES tb_task (id) ON DELETE CASCADE,
    CONSTRAINT fk_submission_agent FOREIGN KEY (agent_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_submission_revises FOREIGN KEY (revises)
        REFERENCES tb_task_submission (submission_id) ON DELETE RESTRICT,
    CONSTRAINT fk_submission_claim FOREIGN KEY (claim_id)
        REFERENCES tb_task_claim (claim_id) ON DELETE RESTRICT,
    CONSTRAINT ck_submission_state
        CHECK (state IN ('delivering', 'uncertain', 'under_review',
                         'settled', 'rejected', 'failed')),
    CONSTRAINT ck_submission_amount_positive CHECK (amount > 0),
    CONSTRAINT ck_submission_version_positive CHECK (version >= 1),
    CONSTRAINT ck_submission_failure
        CHECK (failure IS NULL OR failure IN
            ('RECEIVER_UNREACHABLE', 'RECEIVER_FAULT', 'RECEIVER_PROTOCOL',
             'DELIVERY_UNRESOLVED'))
);

CREATE INDEX idx_submission_task_state ON tb_task_submission (task_id, state);
CREATE INDEX idx_submission_agent      ON tb_task_submission (agent_id, created_at DESC);

-- ============================================================
-- tb_task_submission_event — spec §2 SubmissionEvent (append-only)
--   submission_id, seq, from_state, to_state, cause, at.
--   The first event (submission creation) has from_state NULL.
-- ============================================================

CREATE TABLE tb_task_submission_event (
    submission_id BIGINT      NOT NULL,
    seq           INTEGER     NOT NULL,
    from_state    VARCHAR(16) DEFAULT NULL,
    to_state      VARCHAR(16) NOT NULL,
    cause         VARCHAR(32) NOT NULL,
    at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (submission_id, seq),
    CONSTRAINT fk_submission_event_submission FOREIGN KEY (submission_id)
        REFERENCES tb_task_submission (submission_id) ON DELETE RESTRICT,
    CONSTRAINT ck_event_from_state
        CHECK (from_state IS NULL OR from_state IN
            ('delivering', 'uncertain', 'under_review',
             'settled', 'rejected', 'failed')),
    CONSTRAINT ck_event_to_state
        CHECK (to_state IN ('delivering', 'uncertain', 'under_review',
                            'settled', 'rejected', 'failed')),
    CONSTRAINT ck_event_seq_positive CHECK (seq >= 1)
);

CREATE INDEX idx_submission_event_submission
    ON tb_task_submission_event (submission_id, seq);

-- Append-only enforcement: reject every UPDATE and DELETE.
CREATE OR REPLACE FUNCTION kungfu_submission_event_append_only()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'tb_task_submission_event is append-only (submission_id=%, seq=%)',
        OLD.submission_id, OLD.seq
        USING ERRCODE = 'P0001';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tg_submission_event_no_update
    BEFORE UPDATE OR DELETE ON tb_task_submission_event
    FOR EACH ROW EXECUTE FUNCTION kungfu_submission_event_append_only();

-- ============================================================
-- tb_task_report — spec §2 Report
--   task, reporter, reason, status ∈ {open, dismissed, actioned}.
-- ============================================================

CREATE TABLE tb_task_report (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    BIGINT       NOT NULL,
    reporter_id BIGINT      NOT NULL,
    reason     VARCHAR(2000) NOT NULL,
    status     VARCHAR(16)  NOT NULL DEFAULT 'open',

    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT fk_report_task FOREIGN KEY (task_id)
        REFERENCES tb_task (id) ON DELETE CASCADE,
    CONSTRAINT fk_report_reporter FOREIGN KEY (reporter_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_report_status
        CHECK (status IN ('open', 'dismissed', 'actioned'))
);

CREATE INDEX idx_report_status ON tb_task_report (status);
CREATE INDEX idx_report_task   ON tb_task_report (task_id);

COMMIT;
