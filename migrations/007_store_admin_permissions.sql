-- ============================================================
-- 007: Store Administration permissions (B2)
-- Additive Admin RBAC seed ONLY — no Store schema change, no new
-- system role. superadmin keeps its existing '*' wildcard binding
-- (migration 006) and therefore automatically holds these codes;
-- custom roles acquire them via the existing B1.2 role-permission
-- assignment API.
-- ============================================================

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('store.products.read',    'View the store product catalog (including inactive products)'),
    ('store.products.manage',  'Create, edit, activate and deactivate store products'),
    ('store.redemptions.read', 'View store redemption orders with operational facts'),
    ('store.redemptions.manage', 'Approve, reject, fulfill and cancel store redemptions')
ON CONFLICT (code) DO NOTHING;
