package repository

// Task 1.2 (WO-32) — restricted task audience (kungfu.md §7.2) and
// work opportunity discovery (§8). task_audience (migration 033)
// holds the audience as resolved account ids, written once by the
// task_create transaction and never updated: the audience is fixed
// at creation. Reads here answer two questions — "may this caller
// see this task at all" (the visibility gates behind the
// TASK_NOT_FOUND parity of §12 minimal disclosure) and "which
// restricted tasks are offered to this agent right now" (the §8
// opportunity projection: offered, never owed).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/task"
)

// FindBotIDByName resolves an agent name to its account id — any
// status: audience membership is a fact fixed at creation, and a
// disabled account that is re-enabled picks its audience seats back
// up (whether it may ACT is the auth layer's question, not the
// audience's). Returns nil when the name is unknown.
func FindBotIDByName(ctx context.Context, q pg.Querier, name string) (*int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM tb_bots WHERE bot_name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find bot by name: %w", err)
	}
	return &id, nil
}

// InsertTaskAudience writes the audience rows of a freshly created
// restricted task (one per named agent, ids already resolved). The
// caller owns the create transaction; an open audience writes
// nothing (no rows = open, the migration's compatibility rule).
func InsertTaskAudience(ctx context.Context, q pg.Querier, taskID int64, agentIDs []int64) error {
	if len(agentIDs) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `
		INSERT INTO task_audience (task_id, agent_id)
		SELECT $1, x FROM unnest($2::bigint[]) AS x`, taskID, agentIDs)
	if err != nil {
		return fmt.Errorf("insert task audience: %w", err)
	}
	return nil
}

// TaskAudienceVisible reports whether agentID may read the task's
// contract layer: an open task (no audience rows) is visible to
// everyone; a restricted task only to its named agents (§7.2, §9).
// The publisher is not special-cased here — the service decides the
// author always reads their own task.
func TaskAudienceVisible(ctx context.Context, q pg.Querier, taskID, agentID int64) (bool, error) {
	var restricted, named bool
	err := q.QueryRow(ctx, `
		SELECT
		  EXISTS(SELECT 1 FROM task_audience WHERE task_id = $1),
		  EXISTS(SELECT 1 FROM task_audience WHERE task_id = $1 AND agent_id = $2)`,
		taskID, agentID).Scan(&restricted, &named)
	if err != nil {
		return false, fmt.Errorf("task audience visible: %w", err)
	}
	return !restricted || named, nil
}

// offeredWorkWhere is the §8 opportunity predicate over tb_task: a
// restricted task naming the agent ($1), open, with slots >= 1, not
// the agent's own, below the agent's rejection cap, not currently
// held under an active unexpired claim, and the agent itself active
// (a deactivated agent is offered nothing). It extends the
// FindOpenWorkPage filters — an opportunity is exactly "work_list
// would list it" plus "named" plus "not already taken".
const offeredWorkWhere = ` WHERE tb_task.status = 'open'
	  AND (tb_task.contract->>'price')::bigint >= 1
	  AND tb_task.budget_locked - tb_task.settled - tb_task.reserved - tb_task.refunded
	      >= (tb_task.contract->>'price')::bigint
	  AND tb_task.publisher_id <> $1
	  AND (SELECT COUNT(*) FROM tb_task_submission s
	        JOIN tb_task_submission_event e
	          ON e.submission_id = s.submission_id AND e.to_state = 'rejected'
	        WHERE s.task_id = tb_task.id AND s.agent_id = $1 AND s.state = 'rejected'
	          AND e.at > $2)
	      < COALESCE(NULLIF(tb_task.contract #>> '{limits,max_rejected_per_agent}', '')::bigint, %d)
	  AND EXISTS (SELECT 1 FROM task_audience ta
	              WHERE ta.task_id = tb_task.id AND ta.agent_id = $1)
	  AND NOT EXISTS (SELECT 1 FROM tb_task_claim c
	                  WHERE c.task_id = tb_task.id AND c.agent_id = $1
	                    AND c.status = 'active' AND c.expires_at > $3)
	  AND EXISTS (SELECT 1 FROM tb_bots b WHERE b.id = $1 AND b.status = 'active')`

// CountOfferedWork totals the agent's §8 opportunities: restricted
// tasks naming the agent that work_list would still list (open,
// slots, eligibility) and the agent does not already hold an active
// claim on. Pure count — todo_list reports it as the "tasks" side
// of the opportunities projection.
func CountOfferedWork(ctx context.Context, q pg.Querier, agentID int64, now time.Time) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_task`+fmt.Sprintf(offeredWorkWhere, task.DefaultMaxRejectedPerAgent),
		agentID, now.Add(-task.RejectionWindowHours*time.Hour), now).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count offered work: %w", err)
	}
	return n, nil
}

// FindOfferedWorkPage returns ONE page of the agent's §8
// opportunities (work_list offered_to_me=true), newest first, with
// the total — same shape and ordering as FindOpenWorkPage so the
// paging contract is identical.
func FindOfferedWorkPage(ctx context.Context, q pg.Querier, agentID int64, f WorkFilter, now time.Time, limit, offset int) ([]WorkCandidate, int64, error) {
	where := fmt.Sprintf(offeredWorkWhere, task.DefaultMaxRejectedPerAgent)
	args := []any{agentID, now.Add(-task.RejectionWindowHours * time.Hour), now}
	if f.Code != "" {
		args = append(args, f.Code)
		where += fmt.Sprintf(` AND tb_task.code = $%d`, len(args))
	}
	if f.Keyword != "" {
		args = append(args, "%"+EscapeLike(f.Keyword)+"%")
		n := len(args)
		where += fmt.Sprintf(`
		  AND (COALESCE(tb_task.contract->>'title', '') ILIKE $%d ESCAPE '\'
		        OR COALESCE(tb_task.contract->>'requirements', '') ILIKE $%d ESCAPE '\')`, n, n)
	}
	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	query := `
		SELECT tb_task.id, tb_task.code, tb_task.publisher_id, tb_task.status,
		       tb_task.budget_locked, tb_task.settled, tb_task.reserved, tb_task.refunded,
		       tb_task.paused_reason, tb_task.closed_reason, tb_task.contract,
		       tb_task.created_at, tb_task.updated_at,
		       tb_task.contract AS work_contract
		FROM tb_task` +
		where + `
		ORDER BY tb_task.created_at DESC, tb_task.id DESC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []WorkCandidate
	for rows.Next() {
		var c WorkCandidate
		if err := rows.Scan(&c.Task.ID, &c.Task.Code, &c.Task.PublisherID, &c.Task.Status,
			&c.Task.BudgetLocked, &c.Task.Settled, &c.Task.Reserved, &c.Task.Refunded,
			&c.Task.PausedReason, &c.Task.ClosedReason, &c.Task.Contract,
			&c.Task.CreatedAt, &c.Task.UpdatedAt, &c.Contract); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}
