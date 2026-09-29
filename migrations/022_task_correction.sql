-- ============================================================
-- 022: Task correction (WO-22) — no versions, drafts, snapshots
-- or open gate. The task IS its contract: one row, the current
-- content, three statuses (open, paused, closed).
--
-- Breaking: version columns and tb_task_version are gone; draft
-- status folds into paused; draft_contract becomes the one contract
-- column. Run with --apply-migrations.
-- ============================================================

-- the one contract column (was the draft copy beside the version
-- snapshot that is being removed)
ALTER TABLE tb_task RENAME COLUMN draft_contract TO contract;

-- draft no longer exists: those tasks are paused
UPDATE tb_task SET status = 'paused' WHERE status = 'draft';

-- status domain: open / paused / closed, default paused
ALTER TABLE tb_task DROP CONSTRAINT IF EXISTS ck_task_status;
ALTER TABLE tb_task ALTER COLUMN status SET DEFAULT 'paused';
ALTER TABLE tb_task ADD CONSTRAINT ck_task_status
    CHECK (status IN ('open', 'paused', 'closed'));

-- no versions anywhere
ALTER TABLE tb_task DROP COLUMN IF EXISTS version;
ALTER TABLE tb_task_claim DROP COLUMN IF EXISTS version;
ALTER TABLE tb_task_submission DROP COLUMN IF EXISTS version;
DROP TABLE IF EXISTS tb_task_version;
