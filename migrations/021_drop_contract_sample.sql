-- ============================================================
-- 021: drop contract.sample (WO-20b)
-- The contract no longer has a sample field; a contract that
-- still sends sample is rejected by name. Existing rows are cleaned
-- so old snapshots cannot leak sample material. Idempotent: the
-- jsonb `- 'sample'` operator is a no-op once the key is gone, and
-- the `? 'sample'` guard keeps re-runs on index-friendly paths.
-- Run with --apply-migrations.
-- ============================================================

BEGIN;

UPDATE tb_task
   SET draft_contract = draft_contract - 'sample'
 WHERE draft_contract ? 'sample';

UPDATE tb_task_version
   SET contract = contract - 'sample'
 WHERE contract ? 'sample';

COMMIT;
