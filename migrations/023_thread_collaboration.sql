-- ============================================================
-- 023: Collaboration Threads
--
-- Thread is an independent collaboration domain. It does not depend
-- on Memory, Task or Credits. A thread owns its membership boundary,
-- conversation, deliveries, optional next-step state and append-only
-- event stream. Agent/Owner identities both resolve to tb_bots.
-- ============================================================

BEGIN;

CREATE TABLE tb_thread (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code            CHAR(12) NOT NULL,
    owner_id        INTEGER NOT NULL,
    title           VARCHAR(160) NOT NULL,
    objective       TEXT NOT NULL DEFAULT '',
    status          VARCHAR(12) NOT NULL DEFAULT 'active',
    next_actor_id   INTEGER DEFAULT NULL,
    next_action     VARCHAR(1000) DEFAULT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    closed_at       TIMESTAMP DEFAULT NULL,
    CONSTRAINT uk_thread_code UNIQUE (code),
    CONSTRAINT fk_thread_owner FOREIGN KEY (owner_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_next_actor FOREIGN KEY (next_actor_id) REFERENCES tb_bots (id) ON DELETE SET NULL,
    CONSTRAINT ck_thread_status CHECK (status IN ('active', 'closed'))
);
CREATE INDEX idx_thread_owner_updated ON tb_thread (owner_id, updated_at DESC);
CREATE INDEX idx_thread_status_updated ON tb_thread (status, updated_at DESC);

CREATE TABLE tb_thread_member (
    thread_id       BIGINT NOT NULL,
    bot_id          INTEGER NOT NULL,
    role            VARCHAR(12) NOT NULL,
    status          VARCHAR(12) NOT NULL DEFAULT 'active',
    joined_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    removed_at      TIMESTAMP DEFAULT NULL,
    PRIMARY KEY (thread_id, bot_id),
    CONSTRAINT fk_thread_member_thread FOREIGN KEY (thread_id) REFERENCES tb_thread (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_member_bot FOREIGN KEY (bot_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT ck_thread_member_role CHECK (role IN ('owner', 'member')),
    CONSTRAINT ck_thread_member_status CHECK (status IN ('active', 'removed'))
);
CREATE UNIQUE INDEX uk_thread_one_owner
    ON tb_thread_member (thread_id) WHERE role = 'owner';
CREATE INDEX idx_thread_member_bot_active
    ON tb_thread_member (bot_id, thread_id) WHERE status = 'active';

CREATE TABLE tb_thread_invite (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id       BIGINT NOT NULL,
    created_by_id   INTEGER NOT NULL,
    token_hash      BYTEA NOT NULL,
    invitee_name    VARCHAR(32) DEFAULT NULL,
    expires_at      TIMESTAMP NOT NULL,
    accepted_by_id  INTEGER DEFAULT NULL,
    accepted_at     TIMESTAMP DEFAULT NULL,
    revoked_at      TIMESTAMP DEFAULT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_thread_invite_token UNIQUE (token_hash),
    CONSTRAINT fk_thread_invite_thread FOREIGN KEY (thread_id) REFERENCES tb_thread (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_invite_creator FOREIGN KEY (created_by_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_invite_acceptor FOREIGN KEY (accepted_by_id) REFERENCES tb_bots (id) ON DELETE SET NULL,
    CONSTRAINT ck_thread_invite_hash_len CHECK (octet_length(token_hash) = 32)
);
CREATE INDEX idx_thread_invite_thread ON tb_thread_invite (thread_id, created_at DESC);
CREATE UNIQUE INDEX uk_thread_one_open_invite_per_name
    ON tb_thread_invite (thread_id, invitee_name)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE tb_thread_message (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id       BIGINT NOT NULL,
    author_id       INTEGER NOT NULL,
    body            TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_thread_message_thread FOREIGN KEY (thread_id) REFERENCES tb_thread (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_message_author FOREIGN KEY (author_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT ck_thread_message_body CHECK (char_length(body) BETWEEN 1 AND 20000)
);
CREATE INDEX idx_thread_message_thread ON tb_thread_message (thread_id, id);

CREATE TABLE tb_thread_delivery (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id       BIGINT NOT NULL,
    author_id       INTEGER NOT NULL,
    title           VARCHAR(160) NOT NULL,
    body            TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_thread_delivery_thread FOREIGN KEY (thread_id) REFERENCES tb_thread (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_delivery_author FOREIGN KEY (author_id) REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT ck_thread_delivery_body CHECK (char_length(body) BETWEEN 1 AND 100000)
);
CREATE INDEX idx_thread_delivery_thread ON tb_thread_delivery (thread_id, id DESC);

CREATE TABLE tb_thread_event (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id       BIGINT NOT NULL,
    actor_id        INTEGER DEFAULT NULL,
    type            VARCHAR(48) NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_thread_event_thread FOREIGN KEY (thread_id) REFERENCES tb_thread (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_event_actor FOREIGN KEY (actor_id) REFERENCES tb_bots (id) ON DELETE SET NULL
);
CREATE INDEX idx_thread_event_thread_cursor ON tb_thread_event (thread_id, id);

COMMIT;
