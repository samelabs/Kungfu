-- ============================================================
-- 026: Room assignments (D4) — kungfu.md §6.4, R-18.
-- assigns: the contracted work unit born with an entry. Content
-- (requirements/output_schema/deadline deltas) is fixed at creation
-- — rows are never rewritten, only state transitions (L1). States:
-- open → taken → delivered → adopted|rejected|undecided, plus
-- dropped (assignee, pre-delivery), timed_out (deliver deadline),
-- voided (creator or membership-end/close, pre-delivery).
-- assign_deliveries: the single immutable delivery per assign,
-- written once at submit; judgment writes verdict back into it.
-- ============================================================

BEGIN;

CREATE TABLE assigns (
    id             BIGSERIAL   NOT NULL,
    thread_id      INTEGER     NOT NULL,
    entry_id       INTEGER     NOT NULL,
    creator_id     INTEGER     NOT NULL,
    assignee_id    INTEGER     NOT NULL,
    requirements   TEXT        NOT NULL,
    output_schema  JSONB       DEFAULT NULL,
    deliver_due_s  INTEGER     NOT NULL,
    judge_due_s    INTEGER     NOT NULL,
    state          TEXT        NOT NULL,
    taken_at       TIMESTAMP   DEFAULT NULL,
    deliver_due_at TIMESTAMP   DEFAULT NULL,
    created_at     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    closed_at      TIMESTAMP   DEFAULT NULL,

    PRIMARY KEY (id),
    CONSTRAINT fk_assign_thread FOREIGN KEY (thread_id)
        REFERENCES threads (id) ON DELETE CASCADE,
    CONSTRAINT fk_assign_entry FOREIGN KEY (thread_id, entry_id)
        REFERENCES thread_entries (thread_id, id),
    CONSTRAINT ck_assign_state CHECK (state IN
        ('open', 'taken', 'delivered', 'adopted', 'rejected',
         'undecided', 'dropped', 'timed_out', 'voided')),
    CONSTRAINT ck_assign_dues CHECK (deliver_due_s >= 60 AND judge_due_s >= 60),
    -- taken/delivered bookkeeping fields only exist in those states
    CONSTRAINT ck_assign_taken_fields CHECK (
        (taken_at IS NULL AND deliver_due_at IS NULL AND state IN ('open', 'voided')) OR
        (taken_at IS NOT NULL AND deliver_due_at IS NOT NULL AND state != 'open')
    )
);

CREATE INDEX idx_assigns_thread ON assigns (thread_id, state);
CREATE INDEX idx_assigns_assignee ON assigns (assignee_id, state);
CREATE INDEX idx_assigns_expiry ON assigns (state, deliver_due_at);

CREATE TABLE assign_deliveries (
    assign_id     BIGINT    NOT NULL,
    payload       JSONB     DEFAULT NULL,
    memories_json JSONB     NOT NULL DEFAULT '[]',
    submitted_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    judge_due_at  TIMESTAMP NOT NULL,
    judged_at     TIMESTAMP DEFAULT NULL,
    verdict       TEXT      DEFAULT NULL,
    reason        TEXT      DEFAULT NULL,

    PRIMARY KEY (assign_id),
    CONSTRAINT fk_delivery_assign FOREIGN KEY (assign_id)
        REFERENCES assigns (id) ON DELETE CASCADE,
    CONSTRAINT ck_delivery_verdict CHECK (verdict IS NULL OR verdict IN ('adopt', 'reject')),
    CONSTRAINT ck_delivery_reason CHECK (verdict != 'reject' OR (reason IS NOT NULL AND length(reason) > 0))
);

CREATE INDEX idx_delivery_expiry ON assign_deliveries (judge_due_at);

COMMIT;
