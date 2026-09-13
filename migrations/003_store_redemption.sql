-- ============================================================
-- 003: Store / Redemption — tb_store_products, tb_redemptions
-- Store owns: catalog, credits price, redemption orders, review state,
-- fulfillment state, price/title snapshots.
-- Credits (balance/ledger) stays in tb_bots/tb_transactions and is ONLY
-- mutated via internal/credits.
-- Virtual goods only: no inventory, shipping, categories, promos, SKU.
-- Products are never physically deleted; inactive hides them from the
-- active catalog while historical redemptions stay fully traceable.
-- ============================================================

CREATE TABLE IF NOT EXISTS tb_store_products (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code          CHAR(12)     NOT NULL,
    title         VARCHAR(128) NOT NULL,
    description   VARCHAR(500) DEFAULT NULL,
    credits_price NUMERIC(20,4) NOT NULL,
    status        VARCHAR(10)  NOT NULL DEFAULT 'active',
    created_at    TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_store_product_code UNIQUE (code),
    CONSTRAINT chk_store_price_positive CHECK (credits_price > 0),
    CONSTRAINT chk_store_product_status CHECK (status IN ('active', 'inactive'))
);

CREATE INDEX IF NOT EXISTS idx_store_products_status ON tb_store_products (status);

CREATE TABLE IF NOT EXISTS tb_redemptions (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code             CHAR(12)     NOT NULL,
    bot_id           INTEGER      NOT NULL,
    product_id       BIGINT       NOT NULL,
    product_title    VARCHAR(128) NOT NULL,
    credits_cost     NUMERIC(20,4) NOT NULL,
    request_key      VARCHAR(64)  NOT NULL,
    status           VARCHAR(20)  NOT NULL DEFAULT 'pending_review',
    review_note      VARCHAR(500) DEFAULT NULL,
    fulfillment_note VARCHAR(500) DEFAULT NULL,
    created_at       TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
    reviewed_at      TIMESTAMP    NULL DEFAULT NULL,
    fulfilled_at     TIMESTAMP    NULL DEFAULT NULL,
    cancelled_at     TIMESTAMP    NULL DEFAULT NULL,
    CONSTRAINT uk_redemption_code UNIQUE (code),
    -- idempotency: one redemption per (bot, request_key), DB-enforced
    CONSTRAINT uk_redemption_bot_request UNIQUE (bot_id, request_key),
    CONSTRAINT fk_redemption_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT fk_redemption_product FOREIGN KEY (product_id) REFERENCES tb_store_products (id),
    CONSTRAINT chk_redemption_cost_positive CHECK (credits_cost > 0),
    CONSTRAINT chk_redemption_status CHECK (status IN
        ('pending_review', 'approved', 'rejected', 'fulfilled', 'cancelled'))
);

CREATE INDEX IF NOT EXISTS idx_redemptions_bot_time ON tb_redemptions (bot_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_redemptions_status   ON tb_redemptions (status);
