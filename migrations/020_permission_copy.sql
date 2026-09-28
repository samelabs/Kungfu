-- ============================================================
-- 020: Permission copy correction (WO-17b D6)
-- tasks.manage's description still advertises homepage pin/unpin,
-- a capability that no longer exists. Only the copy changes: the
-- permission code, role grants and assignments are untouched, and
-- the guard keeps the migration idempotent.
-- ============================================================

UPDATE tb_admin_permissions
SET description = 'Close tasks with a reason (no Credits changes)'
WHERE code = 'tasks.manage'
  AND description = 'Close tasks with a reason and pin/unpin them on the homepage (no Credits changes)';
