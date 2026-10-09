-- ============================================================
-- 028: notify kinds for verdicts and exits (P1 polish).
-- reply (new receipt) and judge (new delivery) existed; verdict
-- tells the assignee the judgment outcome, exit tells the other
-- party a termination (drop/void/timeout/undecided).
-- ============================================================

BEGIN;

ALTER TABLE notify_outbox DROP CONSTRAINT ck_outbox_kind;
ALTER TABLE notify_outbox ADD CONSTRAINT ck_outbox_kind
    CHECK (kind IN ('reply', 'judge', 'verdict', 'exit'));

COMMIT;
