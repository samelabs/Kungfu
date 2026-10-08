-- ============================================================
-- 029: 'assign' notify kind (external audit P2-3) — creating an
-- assignment notifies the assignee. An unaccepted assignment is no
-- obligation (§6.4) and must NOT enter the turn list; the
-- notification is the accelerator-only discovery path, the facts
-- remain in thread_get's assignments section.
-- ============================================================

BEGIN;

ALTER TABLE notify_outbox DROP CONSTRAINT ck_outbox_kind;
ALTER TABLE notify_outbox ADD CONSTRAINT ck_outbox_kind
    CHECK (kind IN ('reply', 'judge', 'verdict', 'exit', 'assign'));

COMMIT;
