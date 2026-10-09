-- ============================================================
-- 032: Claim harness pinning (Task 1.1, kungfu.md §7.1/§7.3) — a
-- confirmed engagement binds the required input versions, not
-- "whatever the memory says today".
--
-- claim_harness_revisions freezes, at work_claim time, the revision
-- of every memory the bound contract's harness_refs resolves to.
-- work_harness under that claim serves the pinned revision — even
-- after the publisher edits or withdraws the memory (§9: the input
-- versions bound by an engagement stay readable to the engaged
-- agent). Claims formed before this migration carry no pins and
-- keep their previous live-read behavior (backward compatible).
--
-- tb_task_submission.harness_json records, on claim-LESS
-- submissions, the harness revisions current at intake — the same
-- fact for the engagements that never materialized a claim row
-- (claim.required = false). NULL on claim-carried submissions,
-- whose pins live on the claim.
-- ============================================================

BEGIN;

CREATE TABLE claim_harness_revisions (
    claim_id  BIGINT  NOT NULL,
    memory_id INTEGER NOT NULL,
    revision  BIGINT  NOT NULL,

    PRIMARY KEY (claim_id, memory_id),
    CONSTRAINT fk_chr_claim FOREIGN KEY (claim_id)
        REFERENCES tb_task_claim (claim_id) ON DELETE CASCADE,
    CONSTRAINT fk_chr_memory FOREIGN KEY (memory_id)
        REFERENCES tb_kungfus (id) ON DELETE RESTRICT,
    CONSTRAINT ck_chr_revision_positive CHECK (revision >= 1)
);

CREATE INDEX idx_chr_memory ON claim_harness_revisions (memory_id);

ALTER TABLE tb_task_submission
    ADD COLUMN harness_json JSONB;

COMMIT;
