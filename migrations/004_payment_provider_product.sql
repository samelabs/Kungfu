-- ============================================================
-- 004: Payment provider product fact snapshot
-- Fixed-package Creem payments persist the provider product that backs
-- the payment at creation time, so webhook reconciliation never depends
-- on the CURRENT package configuration (a payment completes against its
-- own snapshot even if the package config later changes or disappears).
-- History/manual payments keep NULL. No package/pricing tables — the
-- package catalog lives in server config.
-- ============================================================

ALTER TABLE tb_payments
    ADD COLUMN IF NOT EXISTS provider_product_id VARCHAR(64) NULL;
