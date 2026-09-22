-- ============================================================
-- 012: Finance Admin permission (read-only finance control plane)
-- Additive Admin RBAC seed ONLY — no schema change, no new system
-- role, no second RBAC. superadmin keeps its existing '*' wildcard
-- (migration 006) and automatically holds this code.
--
-- Boundary fixed by the work order:
--   Provider      = payment facts authority
--   Payment domain = payment policy authority
--   Credits       = balance/ledger mutation authority
--   Finance Admin = observation / reconciliation authority ONLY
--
-- finance.read is intentionally the ONLY finance permission: there
-- is no legitimate Admin finance mutation, so no finance.manage /
-- credits.manage / payments.manage / ledger.manage is created.
-- ============================================================

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('finance.read', 'View finance facts: payments, payment adjustments, credits ledger, local reconciliation (read-only)')
ON CONFLICT (code) DO NOTHING;
