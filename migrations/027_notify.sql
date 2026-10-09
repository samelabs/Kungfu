-- ============================================================
-- 027: Notify endpoints and outbox (D7) — the accelerator only.
-- account_notify: one endpoint per account, verified by challenge
-- at registration; the secret signs dispatches (HMAC-SHA256).
-- notify_outbox: notifications are written in the SAME transaction
-- as the facts that caused them, then dispatched best-effort by a
-- ticker. Losing one is always harmless: todo_list is the truth
-- (kungfu.md §8; PRD A21).
-- ============================================================

BEGIN;

CREATE TABLE account_notify (
    account_id  INTEGER   NOT NULL,
    url         TEXT      NOT NULL,
    secret      TEXT      NOT NULL,
    verified_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (account_id)
);

CREATE TABLE notify_outbox (
    id         BIGSERIAL   NOT NULL,
    account_id INTEGER     NOT NULL,
    kind       TEXT        NOT NULL,
    count      INTEGER     NOT NULL DEFAULT 1,
    attempts   INTEGER     NOT NULL DEFAULT 0,
    sent_at    TIMESTAMP   DEFAULT NULL,
    created_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,

    PRIMARY KEY (id),
    CONSTRAINT fk_outbox_account FOREIGN KEY (account_id)
        REFERENCES tb_bots (id) ON DELETE CASCADE,
    CONSTRAINT ck_outbox_kind CHECK (kind IN ('reply', 'judge'))
);

CREATE INDEX idx_outbox_unsent ON notify_outbox (sent_at) WHERE sent_at IS NULL;

COMMIT;
