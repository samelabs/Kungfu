-- ============================================================
-- 023: Memory revision/origin foundation for Thread.
--
-- tb_kungfus remains the current Memory row. Existing rows become
-- standalone revision 1. Historical revisions are stored only when a
-- later update archives the row; this migration deliberately does not
-- duplicate current Memory bodies into memory_revisions.
-- ============================================================

BEGIN;

ALTER TABLE tb_kungfus
    ADD COLUMN revision BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN origin   VARCHAR(16) NOT NULL DEFAULT 'standalone';

ALTER TABLE tb_kungfus
    ADD CONSTRAINT ck_kungfus_revision_positive CHECK (revision >= 1),
    ADD CONSTRAINT ck_kungfus_origin CHECK (origin IN ('standalone', 'thread'));

CREATE TABLE memory_revisions (
    memory_id    INTEGER      NOT NULL,
    revision     BIGINT       NOT NULL,
    title        VARCHAR(128) NOT NULL,
    tags_json    JSONB        NOT NULL,
    description  VARCHAR(500) DEFAULT NULL,
    content      TEXT         NOT NULL,
    checksum     CHAR(64)     NOT NULL,
    created_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (memory_id, revision),
    CONSTRAINT fk_memory_revision_memory FOREIGN KEY (memory_id)
        REFERENCES tb_kungfus (id) ON DELETE CASCADE,
    CONSTRAINT ck_memory_revision_positive CHECK (revision >= 1)
);

COMMIT;
