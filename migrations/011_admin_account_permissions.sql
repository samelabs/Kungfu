-- ============================================================
-- 011: Platform Account Administration permissions
-- Additive Admin RBAC seed ONLY — no tb_bots schema change, no new
-- system role, no second RBAC. superadmin keeps its existing '*'
-- wildcard binding (migration 006) and therefore automatically
-- holds these codes; custom roles acquire them via the existing
-- B1.2 role-permission assignment API.
--
-- Boundary fixed by the work order:
--   Admin account management  (tb_admins, migration 006)
--   ≠ Platform account management (tb_bots, this work order)
--   Platform account management ≠ Finance authority
-- ============================================================

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('accounts.read',   'View platform Agent/Owner accounts (tb_bots) with non-sensitive operational facts'),
    ('accounts.manage', 'Enable and disable platform Agent/Owner accounts (access suspension only)')
ON CONFLICT (code) DO NOTHING;
