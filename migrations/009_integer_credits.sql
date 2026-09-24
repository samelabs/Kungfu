-- ============================================================
-- 009: Integer Credits conversion (v1.3)
-- Credits become WHOLE INTEGER units stored as BIGINT.
-- Fiat stays integer minor units (amount_minor etc.) — untouched.
--
-- Fail-closed contract:
--   * THIS FILE owns its transaction: BEGIN ... COMMIT. A failure at
--     ANY point (preflight OR a later ALTER) rolls back the ENTIRE
--     migration — never a partially converted schema. Atomicity does
--     NOT depend on ON_ERROR_STOP, on the caller, or on the driver
--     wrapping the file in an implicit transaction.
--   * every affected column is checked for fractional values
--     BEFORE any type conversion — a single non-integral
--     historical value aborts the migration (no ROUND,
--     no TRUNCATE of a fractional financial fact);
--   * exact integral numeric values convert to BIGINT with
--     signs preserved (negative ledger rows and authoritative
--     negative balances stay negative);
--   * all rows and relationships are preserved;
--   * safe on the fresh-install chain where 001-003 already
--     create these columns as BIGINT (no-op).
-- ============================================================

BEGIN;

-- Fail closed on fractional historical data. COALESCE keeps NULL
-- impossible (all columns are NOT NULL), but the check stays cheap
-- and explicit. NaN compares out-of-range in numeric comparisons
-- (NaN <> NaN), so a NaN balance would slip a "value % 1 <> 0"
-- test — the ::numeric(20,0) cast below rejects it outright.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM (
            SELECT balance AS v FROM tb_bots
            UNION ALL
            SELECT budget FROM tb_tasks
            UNION ALL
            SELECT price FROM tb_tasks
            UNION ALL
            SELECT amount FROM tb_transactions
            UNION ALL
            SELECT balance_after FROM tb_transactions
            UNION ALL
            SELECT credits FROM tb_payments
            UNION ALL
            SELECT credits_price FROM tb_store_products
            UNION ALL
            SELECT credits_cost FROM tb_redemptions
        ) s
        WHERE v IS NULL OR v <> floor(v)
    ) THEN
        RAISE EXCEPTION '009_integer_credits: fractional Credits values exist (never rounded, never truncated); resolve them manually before converting'
            USING ERRCODE = '22023';
    END IF;
END
$$;

-- Convert each column only when it is not already BIGINT
-- (fresh-install chain: 001-003 now create BIGINT directly).

ALTER TABLE tb_bots
    ALTER COLUMN balance TYPE BIGINT
        USING (balance :: numeric(20,0) :: bigint);

ALTER TABLE tb_tasks
    ALTER COLUMN budget TYPE BIGINT
        USING (budget :: numeric(20,0) :: bigint),
    ALTER COLUMN price TYPE BIGINT
        USING (price :: numeric(20,0) :: bigint);

ALTER TABLE tb_transactions
    ALTER COLUMN amount TYPE BIGINT
        USING (amount :: numeric(20,0) :: bigint),
    ALTER COLUMN balance_after TYPE BIGINT
        USING (balance_after :: numeric(20,0) :: bigint);

ALTER TABLE tb_payments
    ALTER COLUMN credits TYPE BIGINT
        USING (credits :: numeric(20,0) :: bigint);

ALTER TABLE tb_store_products
    ALTER COLUMN credits_price TYPE BIGINT
        USING (credits_price :: numeric(20,0) :: bigint);

ALTER TABLE tb_redemptions
    ALTER COLUMN credits_cost TYPE BIGINT
        USING (credits_cost :: numeric(20,0) :: bigint);

COMMIT;
