-- ============================================================
-- 013: Payment provider settings live in the database
--
-- Creem configuration moves from CREEM_* environment variables to a
-- single settings row managed from the platform admin. The API key and
-- webhook secret are stored ONLY as AES-256-GCM ciphertext (key from
-- SETTINGS_ENC_KEY, never stored here); mode, success URL and the fixed
-- credit packages are plain operator data.
--
-- One row per provider; 'creem' is the only provider.
-- ============================================================

CREATE TABLE IF NOT EXISTS tb_payment_provider_settings (
    provider            TEXT PRIMARY KEY CHECK (provider = 'creem'),
    enabled             BOOLEAN NOT NULL DEFAULT FALSE,
    mode                TEXT NOT NULL CHECK (mode IN ('test', 'prod')),
    success_url         TEXT NOT NULL,
    packages            JSONB NOT NULL,
    provider_key_enc    BYTEA NOT NULL,
    webhook_secret_enc  BYTEA NOT NULL,
    updated_by          BIGINT REFERENCES tb_admins(id) ON DELETE SET NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('settings.payment.manage', 'View and change payment provider settings (Creem mode, packages, credentials)')
ON CONFLICT (code) DO NOTHING;
