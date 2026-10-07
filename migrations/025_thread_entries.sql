-- ============================================================
-- 025: Thread entries and receipts (D3) — kungfu.md §6.3.
-- thread_entries: one immutable signed post per seq; the memory
-- version is pinned at post time and asked[] is frozen at post time
-- (the response-object rules decide it once).
-- thread_receipts: the pending/fulfilled/withdrawn obligation rows;
-- fulfilled resolution ∈ {reply, handle, take} ("take" is written by
-- the assignment stage D4), withdrawn resolution ∈ {retract, leave,
-- remove, role_change, close}.
-- Cross-thread references are dead at the DB level: every intra-
-- thread pointer (reply_to, receipt entry) is a composite FK on
-- (thread_id, entry_id), so a child can never point at another
-- thread's row (the C1-era drift this line kills for good).
-- ============================================================

BEGIN;

CREATE TABLE thread_entries (
    id              BIGSERIAL    NOT NULL,
    thread_id       INTEGER      NOT NULL,
    seq             BIGINT       NOT NULL,
    author_id       INTEGER      NOT NULL,
    memory_id       INTEGER      NOT NULL,
    memory_revision BIGINT       NOT NULL,
    reply_to_id     BIGINT       DEFAULT NULL,
    asked_json      JSONB        NOT NULL DEFAULT '[]',
    summary         VARCHAR(500) DEFAULT NULL, -- the entry's own digest (D-012)
    assign_id       BIGINT       DEFAULT NULL, -- D4 placeholder
    created_at      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (id),
    UNIQUE (thread_id, seq),
    UNIQUE (thread_id, id), -- composite FK target for same-thread pointers
    CONSTRAINT fk_entry_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id) ON DELETE CASCADE,
    CONSTRAINT fk_entry_memory FOREIGN KEY (memory_id)
        REFERENCES tb_kungfus (id),
    CONSTRAINT fk_entry_reply FOREIGN KEY (thread_id, reply_to_id)
        REFERENCES thread_entries (thread_id, id),
    CONSTRAINT ck_entry_revision_positive CHECK (memory_revision >= 1)
);

CREATE INDEX idx_thread_entries_thread_seq ON thread_entries (thread_id, seq DESC);

CREATE TABLE thread_receipts (
    id           BIGSERIAL   NOT NULL,
    thread_id    INTEGER     NOT NULL,
    entry_id     INTEGER     NOT NULL,
    account_id   INTEGER     NOT NULL,
    state        TEXT        NOT NULL,
    resolution   TEXT        DEFAULT NULL,
    note         TEXT        DEFAULT NULL,
    resolved_at  TIMESTAMP   DEFAULT NULL,
    created_at   TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (id),
    UNIQUE (entry_id, account_id), -- one obligation per (entry, member), forever
    CONSTRAINT fk_receipt_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id) ON DELETE CASCADE,
    CONSTRAINT fk_receipt_entry FOREIGN KEY (thread_id, entry_id)
        REFERENCES thread_entries (thread_id, id) ON DELETE CASCADE,
    CONSTRAINT ck_receipt_state CHECK (state IN ('pending', 'fulfilled', 'withdrawn')),
    CONSTRAINT ck_receipt_resolution CHECK (
        (state = 'pending'   AND resolution IS NULL) OR
        (state = 'fulfilled' AND resolution IN ('reply', 'handle', 'take')) OR
        (state = 'withdrawn' AND resolution IN ('retract', 'leave', 'remove', 'role_change', 'close'))
    ),
    CONSTRAINT ck_receipt_note CHECK (note IS NULL OR state = 'fulfilled')
);

CREATE INDEX idx_thread_receipts_account ON thread_receipts (account_id, state);

COMMIT;
