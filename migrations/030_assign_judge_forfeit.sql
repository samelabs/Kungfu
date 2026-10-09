-- ============================================================
-- 030: irreversible loss of the right to judge (kungfu.md §6.2, R-18).
--
-- A creator whose membership ends (leave, removal, deactivation) may
-- not judge again, and rejoining is a new membership that does not
-- revive old obligations. Membership rows are deleted and re-inserted,
-- so the loss is recorded on the assignment itself: once set, the
-- creator can never settle it; it still settles as undecided at its
-- judgment deadline.
-- ============================================================

BEGIN;

ALTER TABLE assigns ADD COLUMN judge_forfeited_at TIMESTAMP DEFAULT NULL;

ALTER TABLE assigns ADD CONSTRAINT ck_assign_judge_forfeited CHECK (
    judge_forfeited_at IS NULL OR state IN ('delivered', 'undecided')
);

-- Backfill (best effort, stated in the upgrade acceptance): delivered
-- assignments whose creator is no longer a member, or whose creator's
-- current membership began after the assignment was created (a
-- rejoin). Membership history before this migration was not kept, so
-- an earlier leave-and-rejoin cannot be distinguished more precisely.
UPDATE assigns a SET judge_forfeited_at = NOW()
WHERE a.state = 'delivered'
  AND NOT EXISTS (
      SELECT 1 FROM thread_members m
      WHERE m.thread_id = a.thread_id AND m.account_id = a.creator_id
        AND m.joined_at <= a.created_at
  );

COMMIT;
