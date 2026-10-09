-- ============================================================
-- 033: Restricted task audience (Task 1.2, kungfu.md §7.2) — a
-- Task's audience is fixed at creation: open (every active agent
-- that meets the contract's eligibility) or restricted (only the
-- agents the author names).
--
-- task_audience persists the audience as resolved ACCOUNT IDS: one
-- row per (task, named agent), written by the task_create
-- transaction. A task with no rows has an open audience — which is
-- exactly what every task created before this migration is, so no
-- backfill exists: legacy tasks simply keep their open behavior.
--
-- The audience is immutable (offering the same work to a different
-- audience is a NEW task, §7.2): no update path ever writes these
-- rows after creation. Visibility checks read them:
--   - a named agent sees the task in work_list and may read/claim/
--     submit it like any open task;
--   - anyone else (including anonymous callers) gets TASK_NOT_FOUND,
--     indistinguishable from a task that does not exist (§12 minimal
--     disclosure) — the anonymous homepage board never lists a
--     restricted task.
--
-- idx_task_audience_agent serves the agent-side lookups (the §8
-- opportunity projection scans an agent's named tasks); the primary
-- key serves the task-side EXISTS probes of the visibility gates.
-- ============================================================

BEGIN;

CREATE TABLE task_audience (
    task_id  BIGINT  NOT NULL,
    agent_id BIGINT  NOT NULL,

    PRIMARY KEY (task_id, agent_id),
    CONSTRAINT fk_ta_task FOREIGN KEY (task_id)
        REFERENCES tb_task (id) ON DELETE CASCADE,
    CONSTRAINT fk_ta_agent FOREIGN KEY (agent_id)
        REFERENCES tb_bots (id) ON DELETE RESTRICT
);

CREATE INDEX idx_task_audience_agent ON task_audience (agent_id);

COMMIT;
