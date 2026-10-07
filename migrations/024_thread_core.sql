-- ============================================================
-- 024: Thread core (D2) — kungfu.md §6.1, §6.2, §6.5, §4, L3.
-- Three tables:
--
--   threads            one row per room; the KEY GROUP is the three
--                      columns key_hash / key_role / key_issued_at —
--                      at most one active key, all three set together
--                      or all three NULL (never issued, revoked, or
--                      cleared by close §6.5). Issuing a new key
--                      overwrites all three in one statement, so the
--                      old key dies in the same transaction (§6.1).
--   thread_members     membership records exactly two facts (§6.2):
--                      which key admitted the account (NULL for the
--                      creator) and when. PK = UNIQUE(thread_id,
--                      account_id); terminated membership = row gone
--                      (leave, removal, account deactivation §4).
--   thread_idempotency one row per logical request (L3):
--                      PK (account_id, tool, key); same key + same
--                      request replays result_snapshot; same key +
--                      different request is a conflict, never a
--                      mutation.
--
-- Entries, receipts, assigns and turn projections arrive with the
-- D3–D5 stages; threads.next_seq exists now for them.
-- ============================================================

BEGIN;

CREATE TABLE threads (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code          VARCHAR(16)  NOT NULL,
    subject       VARCHAR(200) DEFAULT NULL,
    status        TEXT         NOT NULL DEFAULT 'open',
    key_hash      CHAR(64)     DEFAULT NULL,
    key_role      TEXT         DEFAULT NULL,
    key_issued_at TIMESTAMP    DEFAULT NULL,
    next_seq      BIGINT       NOT NULL DEFAULT 1,
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    closed_at     TIMESTAMP    DEFAULT NULL,

    CONSTRAINT uk_threads_code UNIQUE (code),
    CONSTRAINT ck_threads_status CHECK (status IN ('open', 'closed')),
    CONSTRAINT ck_threads_key_role CHECK (key_role IS NULL OR key_role IN ('governor', 'speaker', 'observer')),
    -- the single-active-key invariant: the three key columns are one
    -- group — all NULL (no active key) or all set (exactly one)
    CONSTRAINT ck_threads_key_group CHECK (
        (key_hash IS NULL) = (key_role IS NULL)
        AND (key_hash IS NULL) = (key_issued_at IS NULL)
    )
);

CREATE TABLE thread_members (
    thread_id           BIGINT   NOT NULL,
    account_id          INTEGER  NOT NULL,
    role                TEXT     NOT NULL,
    joined_via_key_hash CHAR(64) DEFAULT NULL,
    joined_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (thread_id, account_id),
    CONSTRAINT fk_thread_member_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id),
    CONSTRAINT fk_thread_member_bot FOREIGN KEY (account_id)
        REFERENCES tb_bots (id),
    CONSTRAINT ck_thread_member_role CHECK (role IN ('governor', 'speaker', 'observer'))
);

-- thread_list: my rooms, filtered by status (account_id first)
CREATE INDEX idx_thread_members_account ON thread_members (account_id, thread_id);

CREATE TABLE thread_idempotency (
    account_id      INTEGER NOT NULL,
    tool            TEXT    NOT NULL,
    key             TEXT    NOT NULL,
    request_hash    CHAR(64) NOT NULL,
    result_snapshot JSONB   NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (account_id, tool, key),
    CONSTRAINT fk_thread_idem_bot FOREIGN KEY (account_id)
        REFERENCES tb_bots (id)
);

COMMIT;
