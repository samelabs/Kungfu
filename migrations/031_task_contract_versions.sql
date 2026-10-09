-- ============================================================
-- 031: Task contract versions (Task 1.1, kungfu.md §7.1) — a
-- contract revision is a NEW VERSION that binds only engagements
-- formed after it; existing engagements are checked and judged
-- against the version they bound.
--
-- tb_task gains contract_version (the current version). Every
-- version's full contract is kept immutably in
-- task_contract_versions (one row per (task, version), written by
-- the same transaction that publishes it). Claims and submissions
-- record the version they bound, so schema validation and delivery
-- always resolve through the bound version, never "whatever the
-- task says today".
--
-- Backfill: every existing task gets version 1 = its current
-- contract (pre-031 there was exactly one contract per task, so
-- every existing claim and submission bound it). Existing claims
-- and submissions default to version 1.
--
-- Backwards compatible: readers that ignore the new columns see
-- the same one-contract behavior as before.
-- ============================================================

BEGIN;

ALTER TABLE tb_task
    ADD COLUMN contract_version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE tb_task
    ADD CONSTRAINT ck_task_contract_version_positive
        CHECK (contract_version >= 1);

CREATE TABLE task_contract_versions (
    task_id    BIGINT      NOT NULL,
    version    INTEGER     NOT NULL,
    contract   JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (task_id, version),
    CONSTRAINT fk_tcv_task FOREIGN KEY (task_id)
        REFERENCES tb_task (id) ON DELETE CASCADE,
    CONSTRAINT ck_tcv_version_positive CHECK (version >= 1)
);

-- version 1 of every existing task is the contract it has today
INSERT INTO task_contract_versions (task_id, version, contract)
SELECT id, 1, contract FROM tb_task
ON CONFLICT (task_id, version) DO NOTHING;

ALTER TABLE tb_task_claim
    ADD COLUMN contract_version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE tb_task_claim
    ADD CONSTRAINT ck_claim_contract_version_positive
        CHECK (contract_version >= 1);

-- a claim binds an existing version of its own task
ALTER TABLE tb_task_claim
    ADD CONSTRAINT fk_claim_contract_version
        FOREIGN KEY (task_id, contract_version)
        REFERENCES task_contract_versions (task_id, version)
        ON DELETE CASCADE;

ALTER TABLE tb_task_submission
    ADD COLUMN contract_version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE tb_task_submission
    ADD CONSTRAINT ck_submission_contract_version_positive
        CHECK (contract_version >= 1);

ALTER TABLE tb_task_submission
    ADD CONSTRAINT fk_submission_contract_version
        FOREIGN KEY (task_id, contract_version)
        REFERENCES task_contract_versions (task_id, version)
        ON DELETE CASCADE;

COMMIT;
