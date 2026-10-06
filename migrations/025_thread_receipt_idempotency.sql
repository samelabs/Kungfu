-- ============================================================
-- 025: Thread receipt/Todo state and write idempotency persistence.
--
-- Todo is not stored separately: pending thread_receipts are its
-- product projection. Idempotency is keyed by caller Role, operation
-- and idempotency key, with a request hash for conflict detection.
-- ============================================================

BEGIN;

CREATE TABLE thread_receipts (
    thread_id       BIGINT       NOT NULL,
    input_entry_id  BIGINT       NOT NULL,
    role_id         INTEGER      NOT NULL,
    reason          VARCHAR(8)   NOT NULL,
    state           VARCHAR(10)  NOT NULL DEFAULT 'pending',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    handled_at      TIMESTAMPTZ  DEFAULT NULL,
    withdrawn_at    TIMESTAMPTZ  DEFAULT NULL,

    PRIMARY KEY (thread_id, input_entry_id, role_id),
    CONSTRAINT fk_thread_receipt_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id) ON DELETE CASCADE,
    CONSTRAINT fk_thread_receipt_input FOREIGN KEY (input_entry_id)
        REFERENCES thread_memories (id) ON DELETE RESTRICT,
    CONSTRAINT fk_thread_receipt_role FOREIGN KEY (role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_thread_receipt_reason CHECK (reason IN ('entry', 'reply')),
    CONSTRAINT ck_thread_receipt_state CHECK (state IN ('pending', 'handled', 'withdrawn')),
    CONSTRAINT ck_thread_receipt_timestamps CHECK (
        (state = 'pending' AND handled_at IS NULL AND withdrawn_at IS NULL)
        OR (state = 'handled' AND handled_at IS NOT NULL AND withdrawn_at IS NULL)
        OR (state = 'withdrawn' AND handled_at IS NULL AND withdrawn_at IS NOT NULL)
    )
);

CREATE INDEX idx_thread_receipts_role_pending
    ON thread_receipts (role_id, thread_id, created_at DESC)
    WHERE state = 'pending';

CREATE TABLE thread_idempotency (
    role_id          INTEGER     NOT NULL,
    operation        TEXT        NOT NULL,
    idempotency_key  TEXT        NOT NULL,
    request_hash     BYTEA       NOT NULL,
    result_ref       TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (role_id, operation, idempotency_key),
    CONSTRAINT fk_thread_idempotency_role FOREIGN KEY (role_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT,
    CONSTRAINT ck_thread_idempotency_operation CHECK (char_length(operation) > 0),
    CONSTRAINT ck_thread_idempotency_key CHECK (char_length(idempotency_key) > 0),
    CONSTRAINT ck_thread_idempotency_request_hash CHECK (octet_length(request_hash) = 32)
);

COMMIT;
