-- ============================================================
-- 036: a 4xx rejection carries a readable failure reason of its
-- own. ck_submission_failure (015) whitelisted the receiver-FAULT
-- reasons; a rejection is not a fault — REJECTED_BY_RECEIVER names
-- the outcome so an executor reading state+failure alone (an
-- empty-body 4xx carries nothing else) still knows what happened.
-- The raw reply stays recorded verbatim beside it (§7.2).
-- ============================================================

BEGIN;

ALTER TABLE tb_task_submission DROP CONSTRAINT ck_submission_failure;
ALTER TABLE tb_task_submission ADD CONSTRAINT ck_submission_failure
    CHECK (failure IS NULL OR failure IN
        ('RECEIVER_UNREACHABLE', 'RECEIVER_FAULT', 'RECEIVER_PROTOCOL',
         'DELIVERY_UNRESOLVED', 'REJECTED_BY_RECEIVER'));

COMMIT;
