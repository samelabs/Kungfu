-- ============================================================
-- 035: room disposal (3.1.0) — the end-of-life path rooms were
-- missing. Closing a room freezes it; disposal DELETES it: settle
-- everything (close semantics, plus undecided for judgment clocks
-- still running), then remove the aggregate. Payload memories
-- (origin='thread') whose last referencing entry dies with the room
-- go too; standalone memories never do (their life is the author's).
--
-- thread_disposals keeps a metadata-only audit trail (no content).
-- thread_entries.memory_id/revision become nullable: an author may
-- scrub an entry's content (redaction) while the entry shell — seq,
-- replies, others' receipts pointing at it — keeps referential
-- integrity.
-- Admin door: rooms.manage permission (superadmin holds '*').
-- ============================================================

BEGIN;

CREATE TABLE thread_disposals (
    id          BIGSERIAL   PRIMARY KEY,
    code        TEXT        NOT NULL,
    actor_kind  TEXT        NOT NULL,          -- governor | admin | retention
    actor_ref   TEXT        DEFAULT NULL,      -- admin id / retention marker
    disposed_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE thread_entries ALTER COLUMN memory_id DROP NOT NULL;
ALTER TABLE thread_entries ALTER COLUMN memory_revision DROP NOT NULL;

INSERT INTO tb_admin_permissions (code, description) VALUES
    ('rooms.manage', 'Dispose rooms (settle and hard-delete a room aggregate and its thread-payload memories)')
ON CONFLICT (code) DO NOTHING;

COMMIT;
