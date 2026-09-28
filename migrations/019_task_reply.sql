-- ============================================================
-- 019: Task reply record (WO-13)
-- The receiver's reply is the verdict: its status code decides the
-- outcome and its body is handed to the executor verbatim. The
-- submission row records the reply (response_code, response_body)
-- instead of a platform-built verdict. The platform no longer holds
-- submissions for review: the under_review state, the verdict and the
-- review deadline are gone. The payload is kept only until the
-- submission is terminal (redelivery needs it), then cleared.
--
-- Applies on a database with no under_review submissions (the 1.0
-- task tables have never carried production data).
-- ============================================================

BEGIN;

ALTER TABLE tb_task_submission
    ADD COLUMN response_code INTEGER DEFAULT NULL,
    ADD COLUMN response_body TEXT    DEFAULT NULL,
    DROP COLUMN verdict,
    DROP COLUMN review_deadline;

ALTER TABLE tb_task_submission DROP CONSTRAINT ck_submission_state;
ALTER TABLE tb_task_submission ADD CONSTRAINT ck_submission_state
    CHECK (state IN ('delivering', 'uncertain', 'settled', 'rejected', 'failed'));

ALTER TABLE tb_task_submission_event DROP CONSTRAINT ck_event_from_state;
ALTER TABLE tb_task_submission_event ADD CONSTRAINT ck_event_from_state
    CHECK (from_state IS NULL OR from_state IN
        ('delivering', 'uncertain', 'settled', 'rejected', 'failed'));
ALTER TABLE tb_task_submission_event DROP CONSTRAINT ck_event_to_state;
ALTER TABLE tb_task_submission_event ADD CONSTRAINT ck_event_to_state
    CHECK (to_state IN ('delivering', 'uncertain', 'settled', 'rejected', 'failed'));

-- Terminal submissions no longer keep their payload.
UPDATE tb_task_submission SET payload = NULL
    WHERE state IN ('settled', 'rejected', 'failed') AND payload IS NOT NULL;

-- The recovery pass scans in-flight submissions every 30 seconds.
CREATE INDEX idx_submission_inflight ON tb_task_submission (updated_at)
    WHERE state IN ('delivering', 'uncertain');

COMMIT;
