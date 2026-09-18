-- ============================================================
-- 008: Agent API key at-rest hardening (S6.1)
-- Replaces the recoverable plaintext tb_bots.api_key column with
-- api_key_hash (SHA-256, 32 bytes) + display-only api_key_last4.
-- PostgreSQL 16 core crypto only (sha256 + convert_to) — no
-- extensions. Single transaction: any failure rolls back and leaves
-- the legacy plaintext schema intact.
-- ============================================================

BEGIN;

-- Fail closed if any existing plaintext key does not match the
-- agent-key format (case-insensitive hex, mirroring runtime
-- validation). Such rows must be resolved by the operator first;
-- they are never silently migrated.
DO $$
DECLARE
    bad_count INTEGER;
BEGIN
    SELECT COUNT(*) INTO bad_count FROM tb_bots
    WHERE api_key IS NOT NULL
      AND api_key !~ '^kf_live_[a-fA-F0-9]{64}$';
    IF bad_count > 0 THEN
        RAISE EXCEPTION '008_agent_key_hash: % tb_bots row(s) have api_key values outside the agent-key format; refusing to migrate', bad_count;
    END IF;
END
$$;

ALTER TABLE tb_bots ADD COLUMN api_key_hash BYTEA;
ALTER TABLE tb_bots ADD COLUMN api_key_last4 VARCHAR(4);

-- One-time backfill with PostgreSQL core crypto over the exact
-- legacy bytes. This is the ONLY place the plaintext column is
-- referenced; it is dropped below in the same transaction.
UPDATE tb_bots
SET api_key_hash  = sha256(convert_to(api_key, 'UTF8')),
    api_key_last4 = RIGHT(api_key, 4);

ALTER TABLE tb_bots ALTER COLUMN api_key_hash SET NOT NULL;
ALTER TABLE tb_bots ALTER COLUMN api_key_last4 SET NOT NULL;

ALTER TABLE tb_bots ADD CONSTRAINT ck_bots_api_key_hash_len CHECK (LENGTH(api_key_hash) = 32);
ALTER TABLE tb_bots ADD CONSTRAINT ck_bots_api_key_last4_len CHECK (LENGTH(api_key_last4) = 4);

ALTER TABLE tb_bots ADD CONSTRAINT uk_api_key_hash UNIQUE (api_key_hash);

ALTER TABLE tb_bots DROP CONSTRAINT uk_api_key;
ALTER TABLE tb_bots DROP COLUMN api_key;

COMMIT;
