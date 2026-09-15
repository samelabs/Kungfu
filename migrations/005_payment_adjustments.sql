-- ============================================================
-- 005: Payment adjustment facts (refund / dispute)
-- Persists provider-side refund.created / dispute.created events as
-- durable, auditable economic facts in the Payment domain.
--
-- Scope is deliberately FACTS ONLY:
--   - no credits_delta / balance_after / reversal execution
--   - no refund status machine, debt, or account hold
--   - no raw webhook payloads or customer data
-- tb_payments.status stays "paid" forever: paid means the payment once
-- succeeded; adjustments are independent follow-up facts.
-- The next round implements Credits reversal on top of these facts.
-- ============================================================

CREATE TABLE IF NOT EXISTS tb_payment_adjustments (
    id                        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id                BIGINT       NOT NULL REFERENCES tb_payments(id),
    provider                  VARCHAR(32)  NOT NULL,
    provider_event_id         VARCHAR(64)  NOT NULL,
    provider_object_id        VARCHAR(64)  NOT NULL,
    kind                      VARCHAR(16)  NOT NULL,
    provider_transaction_id   VARCHAR(64)  NOT NULL,
    provider_order_id         VARCHAR(64)  NOT NULL,

    amount_minor              BIGINT       NOT NULL,
    currency                  CHAR(3)      NOT NULL,

    transaction_amount_minor  BIGINT       NOT NULL,
    amount_paid_minor         BIGINT       NOT NULL,
    refunded_amount_minor     BIGINT       NULL,

    object_status             VARCHAR(32)  NULL,
    transaction_status        VARCHAR(32)  NULL,
    reason                    VARCHAR(64)  NULL,

    provider_created_at       BIGINT       NOT NULL,
    created_at                TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT ck_payment_adjustments_kind CHECK (kind IN ('refund', 'dispute')),
    CONSTRAINT ck_payment_adjustments_amount CHECK (amount_minor > 0),
    CONSTRAINT ck_payment_adjustments_txn_amount CHECK (transaction_amount_minor > 0),
    CONSTRAINT ck_payment_adjustments_amount_paid CHECK (amount_paid_minor > 0),
    CONSTRAINT ck_payment_adjustments_refunded CHECK (refunded_amount_minor IS NULL OR refunded_amount_minor >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS uk_payment_adjustments_event
    ON tb_payment_adjustments (provider, provider_event_id);

CREATE UNIQUE INDEX IF NOT EXISTS uk_payment_adjustments_object
    ON tb_payment_adjustments (provider, kind, provider_object_id);

CREATE INDEX IF NOT EXISTS ix_payment_adjustments_payment_time
    ON tb_payment_adjustments (payment_id, created_at DESC);

CREATE INDEX IF NOT EXISTS ix_payment_adjustments_provider_txn
    ON tb_payment_adjustments (provider_transaction_id);
