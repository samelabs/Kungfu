-- ============================================================
-- 034: notify kind 'test' for notify_test (3.1.0).
-- The self-test tool queues a kind="test" doorbell so an account can
-- verify its webhook wiring end to end without a collaborator
-- creating a real obligation. Same doorbell discipline as every
-- other kind: content-free, best-effort, loss-harmless.
-- ============================================================

BEGIN;

ALTER TABLE notify_outbox DROP CONSTRAINT ck_outbox_kind;
ALTER TABLE notify_outbox ADD CONSTRAINT ck_outbox_kind
    CHECK (kind IN ('reply', 'judge', 'verdict', 'exit', 'assign', 'test'));

COMMIT;
