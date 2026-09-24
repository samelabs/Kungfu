-- ============================================================
-- 014: Platform operations permissions (tasks + memories)
-- Additive Admin RBAC seed only. superadmin holds '*' and gets these
-- automatically.
--
-- Task governance is status-only (close with a reason, pin/unpin for
-- the homepage) and never moves Credits; memory governance can make a
-- memory private or remove it (soft delete).
-- ============================================================

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('tasks.read',      'View all tasks, their owners and submissions'),
    ('tasks.manage',    'Close tasks with a reason and pin/unpin them on the homepage (no Credits changes)'),
    ('memories.read',   'View all memories including content'),
    ('memories.manage', 'Make memories private or remove them')
ON CONFLICT (code) DO NOTHING;
