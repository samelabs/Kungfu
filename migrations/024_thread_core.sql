-- ============================================================
-- 024: Role Link and Thread persistence kernel.
--
-- Role identity is tb_bots. Collaboration locations are ThreadMemory
-- entry ids; raw Memory ids remain information-object identities.
-- This migration establishes persistence and relational constraints
-- only. Thread behavior is implemented in later stages.
-- ============================================================

BEGIN;

CREATE TABLE role_links (
    role_low_id          INTEGER     NOT NULL,
    role_high_id         INTEGER     NOT NULL,
    requested_by_role_id INTEGER     NOT NULL,
    status               VARCHAR(8)  NOT NULL DEFAULT 'pending',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    accepted_at          TIMESTAMPTZ DEFAULT NULL,

    PRIMARY KEY (role_low_id, role_high_id),
    CONSTRAINT fk_role_link_low FOREIGN KEY (role_low_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_role_link_high FOREIGN KEY (role_high_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_role_link_requester FOREIGN KEY (requested_by_role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_role_link_canonical CHECK (role_low_id < role_high_id),
    CONSTRAINT ck_role_link_requester_pair CHECK (
        requested_by_role_id = role_low_id OR requested_by_role_id = role_high_id
    ),
    CONSTRAINT ck_role_link_status CHECK (status IN ('pending', 'active')),
    CONSTRAINT ck_role_link_acceptance CHECK (
        (status = 'pending' AND accepted_at IS NULL)
        OR (status = 'active' AND accepted_at IS NOT NULL)
    )
);

CREATE INDEX idx_role_links_low_status
    ON role_links (role_low_id, status);
CREATE INDEX idx_role_links_high_status
    ON role_links (role_high_id, status);

CREATE TABLE threads (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code               CHAR(12)     NOT NULL,
    join_key_hash      BYTEA        DEFAULT NULL,
    join_entry_id      BIGINT       DEFAULT NULL,
    subject            TEXT         NOT NULL,
    created_by_role_id INTEGER      NOT NULL,
    parent_thread_id   BIGINT       DEFAULT NULL,
    anchor_entry_id    BIGINT       DEFAULT NULL,
    status             VARCHAR(8)   NOT NULL DEFAULT 'open',
    next_seq           BIGINT       NOT NULL DEFAULT 1,
    revision           BIGINT       NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT uk_threads_code UNIQUE (code),
    CONSTRAINT fk_threads_creator FOREIGN KEY (created_by_role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_threads_parent FOREIGN KEY (parent_thread_id)
        REFERENCES threads (id) ON DELETE RESTRICT,
    CONSTRAINT ck_threads_status CHECK (status IN ('open', 'closed')),
    CONSTRAINT ck_threads_next_seq_positive CHECK (next_seq >= 1),
    CONSTRAINT ck_threads_revision_positive CHECK (revision >= 1),
    CONSTRAINT ck_threads_root_child_shape CHECK (
        (parent_thread_id IS NULL AND anchor_entry_id IS NULL)
        OR (parent_thread_id IS NOT NULL AND anchor_entry_id IS NOT NULL)
    ),
    CONSTRAINT ck_threads_join_key_hash CHECK (
        join_key_hash IS NULL OR octet_length(join_key_hash) = 32
    ),
    CONSTRAINT ck_threads_join_pair CHECK (
        (join_key_hash IS NULL AND join_entry_id IS NULL)
        OR (join_key_hash IS NOT NULL AND join_entry_id IS NOT NULL)
    )
);

CREATE INDEX idx_threads_creator
    ON threads (created_by_role_id, updated_at DESC);
CREATE INDEX idx_threads_parent
    ON threads (parent_thread_id, id);
CREATE UNIQUE INDEX uk_threads_join_key_hash
    ON threads (join_key_hash) WHERE join_key_hash IS NOT NULL;

CREATE TABLE thread_memories (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id         BIGINT      NOT NULL,
    memory_id         INTEGER     NOT NULL,
    memory_revision   BIGINT      NOT NULL,
    seq               BIGINT      NOT NULL,
    author_role_id    INTEGER     NOT NULL,
    reply_to_entry_id BIGINT      DEFAULT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT fk_thread_memory_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_memory_memory FOREIGN KEY (memory_id)
        REFERENCES tb_kungfus (id) ON DELETE RESTRICT,
    CONSTRAINT fk_thread_memory_author FOREIGN KEY (author_role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_thread_memory_reply FOREIGN KEY (reply_to_entry_id)
        REFERENCES thread_memories (id) ON DELETE RESTRICT,
    CONSTRAINT uk_thread_memory_seq UNIQUE (thread_id, seq),
    CONSTRAINT uk_thread_memory_thread_entry UNIQUE (thread_id, id),
    CONSTRAINT ck_thread_memory_revision_positive CHECK (memory_revision >= 1),
    CONSTRAINT ck_thread_memory_seq_positive CHECK (seq >= 1)
);

CREATE INDEX idx_thread_memories_memory_revision
    ON thread_memories (memory_id, memory_revision);
CREATE INDEX idx_thread_memories_reply
    ON thread_memories (reply_to_entry_id) WHERE reply_to_entry_id IS NOT NULL;

-- A Child anchor must be an entry in its direct parent Thread.
ALTER TABLE threads
    ADD CONSTRAINT fk_threads_parent_anchor
    FOREIGN KEY (parent_thread_id, anchor_entry_id)
    REFERENCES thread_memories (thread_id, id)
    ON DELETE RESTRICT;

-- A join key is bound to one concrete collaboration occurrence.
ALTER TABLE threads
    ADD CONSTRAINT fk_threads_join_entry FOREIGN KEY (join_entry_id)
    REFERENCES thread_memories (id) ON DELETE RESTRICT;

CREATE TABLE thread_roles (
    thread_id          BIGINT      NOT NULL,
    role_id            INTEGER     NOT NULL,
    permission         VARCHAR(8)  NOT NULL,
    joined_by_role_id  INTEGER     DEFAULT NULL,
    entry_id            BIGINT      NOT NULL,
    joined_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (thread_id, role_id),
    CONSTRAINT fk_thread_role_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_role_role FOREIGN KEY (role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_thread_role_joiner FOREIGN KEY (joined_by_role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT fk_thread_role_entry FOREIGN KEY (entry_id)
        REFERENCES thread_memories (id) ON DELETE RESTRICT,
    CONSTRAINT ck_thread_role_permission CHECK (permission IN ('read', 'write', 'manage'))
);

CREATE INDEX idx_thread_roles_role
    ON thread_roles (role_id, thread_id);

COMMIT;
