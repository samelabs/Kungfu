-- ============================================================
-- 037: a room's profile becomes editable. threads.subject existed
-- since 024 but had no write path after creation — a subject
-- minted from stale context lived forever. 037 adds the missing
-- profile half (description, ≤1000 chars) and the bookkeeping
-- column (updated_at, bumped by every profile write) that
-- thread_update reports back. Existing rows backfill with the
-- migration timestamp; both ALTERs are additive, so fresh and
-- existing databases replay identically.
-- ============================================================

BEGIN;

ALTER TABLE threads ADD COLUMN description VARCHAR(1000) DEFAULT NULL;
ALTER TABLE threads ADD COLUMN updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;

COMMIT;
