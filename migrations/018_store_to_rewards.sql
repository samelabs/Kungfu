-- ============================================================
-- 018: Store → Rewards rename (WO-12)
-- The credit-redemption feature is "Rewards" (奖励兑换): it must not
-- read as selling credits or subscriptions. Pure rename — no new
-- tables, no data copies: table, index, constraint and permission
-- codes carry the rewards name from here on.
-- ============================================================

BEGIN;

ALTER TABLE tb_store_products RENAME TO tb_rewards_products;
ALTER INDEX idx_store_products_status RENAME TO idx_rewards_products_status;
ALTER TABLE tb_rewards_products RENAME CONSTRAINT uk_store_product_code TO uk_rewards_product_code;
ALTER TABLE tb_rewards_products RENAME CONSTRAINT chk_store_price_positive TO chk_rewards_price_positive;
ALTER TABLE tb_rewards_products RENAME CONSTRAINT chk_store_product_status TO chk_rewards_product_status;

-- Permission codes are PRIMARY KEY values of tb_admin_permissions and
-- are referenced by tb_admin_role_permissions.permission_code; the
-- inline FK (no ON UPDATE CASCADE) is rebuilt around the swap.
ALTER TABLE tb_admin_role_permissions DROP CONSTRAINT tb_admin_role_permissions_permission_code_fkey;
UPDATE tb_admin_permissions SET code = 'rewards.products.read',
    description = 'View the rewards catalog (including inactive products)'
    WHERE code = 'store.products.read';
UPDATE tb_admin_permissions SET code = 'rewards.products.manage',
    description = 'Create, edit, activate and deactivate reward products'
    WHERE code = 'store.products.manage';
UPDATE tb_admin_permissions SET code = 'rewards.redemptions.read',
    description = 'View rewards redemption orders with operational facts'
    WHERE code = 'store.redemptions.read';
UPDATE tb_admin_permissions SET code = 'rewards.redemptions.manage',
    description = 'Approve, reject, fulfill and cancel rewards redemptions'
    WHERE code = 'store.redemptions.manage';
UPDATE tb_admin_role_permissions SET permission_code = 'rewards.products.read'
    WHERE permission_code = 'store.products.read';
UPDATE tb_admin_role_permissions SET permission_code = 'rewards.products.manage'
    WHERE permission_code = 'store.products.manage';
UPDATE tb_admin_role_permissions SET permission_code = 'rewards.redemptions.read'
    WHERE permission_code = 'store.redemptions.read';
UPDATE tb_admin_role_permissions SET permission_code = 'rewards.redemptions.manage'
    WHERE permission_code = 'store.redemptions.manage';
ALTER TABLE tb_admin_role_permissions ADD CONSTRAINT tb_admin_role_permissions_permission_code_fkey
    FOREIGN KEY (permission_code) REFERENCES tb_admin_permissions(code);

COMMIT;
