-- ============================================================
-- 017: Task governance permissions (WO-8b)
-- Additive Admin RBAC seed only. superadmin holds '*' and gets these
-- automatically. Objects belong to the application role (no role name
-- is hardcoded; SET ROLE is never used).
--
-- tasks.read and tasks.manage were first seeded by 014 and are
-- re-asserted here unchanged (ON CONFLICT (code) DO NOTHING); the new
-- code is reports.manage: triaging task reports — dismissing them, or
-- closing the reported task through the platform close. Closing a
-- task never moves Credits; the owner keeps the normal refund path.
-- ============================================================

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('tasks.read',     'View all tasks, their owners and submissions'),
    ('tasks.manage',   'Close tasks with a reason and pin/unpin them on the homepage (no Credits changes)'),
    ('reports.manage', 'Review task reports: dismiss them or close the reported task with a platform reason')
ON CONFLICT (code) DO NOTHING;
