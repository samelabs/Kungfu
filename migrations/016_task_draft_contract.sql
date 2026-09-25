-- 016: Task publisher lifecycle (WO-2b).
--
-- tb_task gains draft_contract: the current contract JSON while the
-- task is draft or paused (the material UpdateTask edits and OpenTask
-- validates, snapshots into tb_task_version, and delivers as the test
-- payload). Budget stays in budget_locked, written only by the money
-- primitives — no other columns.

BEGIN;

ALTER TABLE tb_task ADD COLUMN IF NOT EXISTS draft_contract JSONB;

-- Backfill any pre-016 rows (dev databases that ran 015) so the
-- column can become NOT NULL; new rows always carry an explicit
-- contract written by CreateTask.
UPDATE tb_task SET draft_contract = '{}'::jsonb WHERE draft_contract IS NULL;

ALTER TABLE tb_task ALTER COLUMN draft_contract SET NOT NULL;

COMMIT;
