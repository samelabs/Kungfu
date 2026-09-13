-- ============================================================
-- 002: Payment domain — tb_payments
-- Payment owns: order, provider identity/reference, fiat amount,
-- credits amount, status, paid fact.
-- Credits (balance/ledger) stays in tb_bots/tb_transactions and is
-- ONLY mutated via internal/credits.
-- No refund model in this round by design.
-- ============================================================

CREATE TABLE IF NOT EXISTS tb_payments (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code             CHAR(12)    NOT NULL,
    bot_id           INTEGER     NOT NULL,
    provider         VARCHAR(32) NOT NULL,
    provider_order_id VARCHAR(64) DEFAULT NULL,
    amount_minor     BIGINT      NOT NULL,
    currency         CHAR(3)     NOT NULL,
    credits          NUMERIC(20,4) NOT NULL,
    status           VARCHAR(10) NOT NULL DEFAULT 'pending',
    created_at       TIMESTAMP   DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP   DEFAULT CURRENT_TIMESTAMP,
    paid_at          TIMESTAMP   NULL DEFAULT NULL,
    CONSTRAINT uk_payment_code UNIQUE (code),
    CONSTRAINT fk_payments_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT chk_payments_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT chk_payments_credits_positive CHECK (credits > 0),
    CONSTRAINT chk_payments_status CHECK (status IN ('pending', 'paid', 'failed', 'cancelled')),
    -- one provider order can back at most one payment (only when known)
    CONSTRAINT uk_payments_provider_order UNIQUE (provider, provider_order_id)
);

CREATE INDEX IF NOT EXISTS idx_payments_bot_time ON tb_payments (bot_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_payments_status    ON tb_payments (status);
