-- ============================================================
-- 023: Memory revision (D1) — kungfu.md §5 versioned Memory.
-- tb_kungfus gains revision (current version, starts at 1) and
-- origin (standalone today; 'thread' memories arrive with the
-- Thread stages — memory_list already excludes them).
-- memory_revisions keeps the immutable snapshot of every PRIOR
-- version: one row per (memory, revision), written by the author's
-- update transaction (row lock → archive old version → content
-- update + revision bump). Creation never writes history.
--
-- Backfill: the column defaults populate every existing row with
-- revision=1 / origin='standalone'. Existing content is NOT copied
-- into memory_revisions — history starts with the next update.
-- ============================================================

BEGIN;

ALTER TABLE tb_kungfus
    ADD COLUMN revision BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN origin   TEXT  NOT NULL DEFAULT 'standalone';

-- One immutable row per prior version of a memory. The snapshot
-- columns carry the old version's full content plus its own write
-- time (updated_at); archived_at records when the version was
-- superseded. The pair (memory_id, revision) is unique — an update
-- racing another update archives each version exactly once.
CREATE TABLE memory_revisions (
    memory_id   INTEGER       NOT NULL,
    revision    BIGINT        NOT NULL,
    title       VARCHAR(128)  NOT NULL,
    tags_json   JSONB         NOT NULL,
    description VARCHAR(500)  DEFAULT NULL,
    content     TEXT          NOT NULL,
    checksum    CHAR(64)      NOT NULL,
    updated_at  TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived_at TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (memory_id, revision),
    CONSTRAINT fk_memory_revision_kungfu FOREIGN KEY (memory_id)
        REFERENCES tb_kungfus (id) ON DELETE CASCADE,
    CONSTRAINT ck_memory_revision_positive CHECK (revision >= 1)
);

COMMIT;
