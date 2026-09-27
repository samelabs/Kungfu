package task

import (
	"context"
	"fmt"
	"strings"

	"kungfu.md/internal/pg"
)

// InvariantViolation collects every §10 breach found for one task.
// The message lists each finding so a failing test reports all of
// them at once.
type InvariantViolation struct {
	TaskID int64
	Items  []string
}

func (e *InvariantViolation) Error() string {
	return fmt.Sprintf("task %d invariant violations: %s",
		e.TaskID, strings.Join(e.Items, "; "))
}

// Ledger settlement reference conventions used by the repository
// money primitives: settlement pays the agent with an earn_task
// ledger row referencing the submission.
const (
	LedgerTypeLock   = "lock_task"
	LedgerTypeFund   = "fund_task"
	LedgerTypeEarn   = "earn_task"
	LedgerTypeRefund = "refund_task"

	RefTypeTask       = "task"
	RefTypeSubmission = "task_submission"
)

// CheckInvariants audits spec §10 items 1, 2, 3, 6 and 9 for one
// task, read-only, on a pool or inside a caller's transaction:
//
//	1  budget_locked = settled + reserved + refunded + available,
//	   available ≥ 0, reserved ≥ 0
//	2  reserved = Σ active Claim.amount
//	             + Σ {delivering, uncertain} Submission.amount
//	3  each Submission has at most one settlement record (earn_task
//	   ledger row); a settled Submission has exactly one and its
//	   amount equals the Submission's amount
//	6  at most one Submission per (agent, task, request_key)
//	9  each Submission's state equals the to_state of its last
//	   SubmissionEvent (append-only itself is enforced by trigger)
//
// It returns nil when every check passes. A missing task is an
// error, not a violation.
func CheckInvariants(ctx context.Context, q pg.Querier, taskID int64) error {
	var budgetLocked, settled, reserved, refunded int64
	err := q.QueryRow(ctx, `
		SELECT budget_locked, settled, reserved, refunded
		FROM tb_task WHERE id = $1`, taskID).
		Scan(&budgetLocked, &settled, &reserved, &refunded)
	if err != nil {
		return fmt.Errorf("load task %d: %w", taskID, err)
	}

	v := &InvariantViolation{TaskID: taskID}
	add := func(format string, args ...any) {
		v.Items = append(v.Items, fmt.Sprintf(format, args...))
	}

	// -- §10.1: available = budget_locked − settled − reserved − refunded
	if reserved < 0 {
		add("§10.1: reserved = %d < 0", reserved)
	}
	available := budgetLocked - settled - reserved - refunded
	if available < 0 {
		add("§10.1: available = %d < 0 (budget_locked=%d settled=%d reserved=%d refunded=%d)",
			available, budgetLocked, settled, reserved, refunded)
	}

	// -- §10.2: reserved = Σ active claims + Σ non-terminal submissions
	var claimSum, submissionSum int64
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM tb_task_claim
		WHERE task_id = $1 AND status = 'active'`, taskID).Scan(&claimSum); err != nil {
		return fmt.Errorf("sum active claims: %w", err)
	}
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM tb_task_submission
		WHERE task_id = $1 AND state IN ('delivering', 'uncertain')`,
		taskID).Scan(&submissionSum); err != nil {
		return fmt.Errorf("sum non-terminal submissions: %w", err)
	}
	if reserved != claimSum+submissionSum {
		add("§10.2: reserved = %d but active claims = %d + non-terminal submissions = %d",
			reserved, claimSum, submissionSum)
	}

	// -- §10.3 + §10.9: per-submission settlement, state vs. last event
	rows, err := q.Query(ctx, `
		SELECT s.submission_id, s.agent_id, s.request_key, s.state, s.amount,
		       (SELECT e.to_state FROM tb_task_submission_event e
		         WHERE e.submission_id = s.submission_id
		         ORDER BY e.seq DESC LIMIT 1),
		       (SELECT COUNT(*) FROM tb_task_submission_event e
		         WHERE e.submission_id = s.submission_id),
		       (SELECT COUNT(*) FROM tb_transactions t
		         WHERE t.type = 'earn_task'
		           AND t.ref_type = 'task_submission'
		           AND t.ref_id = s.submission_id::text
		           AND t.amount > 0),
		       (SELECT COALESCE(SUM(t.amount), 0) FROM tb_transactions t
		         WHERE t.type = 'earn_task'
		           AND t.ref_type = 'task_submission'
		           AND t.ref_id = s.submission_id::text)
		FROM tb_task_submission s
		WHERE s.task_id = $1
		ORDER BY s.submission_id`, taskID)
	if err != nil {
		return fmt.Errorf("load submissions: %w", err)
	}
	defer rows.Close()

	seenIdentity := make(map[[2]string]string) // §10.6: agent+request_key → state
	for rows.Next() {
		var (
			subID, agentID, amount int64
			requestKey, state      string
			lastTo                 *string
			eventCount             int64
			settleCount            int64
			settleSum              int64
		)
		if err := rows.Scan(&subID, &agentID, &requestKey, &state, &amount,
			&lastTo, &eventCount, &settleCount, &settleSum); err != nil {
			return fmt.Errorf("scan submission: %w", err)
		}

		// §10.3: at most one settlement record; settled ⇒ exactly one;
		// the settlement amount equals the submission's amount.
		if settleCount > 1 {
			add("§10.3: submission %d has %d earn_task settlement records", subID, settleCount)
		}
		if settleCount == 1 && settleSum != amount {
			add("§10.3: submission %d settled %d, amount %d", subID, settleSum, amount)
		}
		if state == SubSettled && settleCount != 1 {
			add("§10.3: submission %d settled but has %d settlement records", subID, settleCount)
		}
		if state != SubSettled && settleCount != 0 {
			add("§10.3: submission %d in state %s but has %d settlement records",
				subID, state, settleCount)
		}

		// §10.9: state equals the last event's to_state; ≥1 event.
		if eventCount == 0 {
			add("§10.9: submission %d has no SubmissionEvent", subID)
		} else if lastTo == nil || *lastTo != state {
			last := "(none)"
			if lastTo != nil {
				last = *lastTo
			}
			add("§10.9: submission %d state %s, last event to_state %s", subID, state, last)
		}

		// §10.6: unique (agent, task, request_key) — the DB unique key
		// is the enforcer; audit for breaches anyway.
		identity := [2]string{fmt.Sprint(agentID), requestKey}
		if prev, dup := seenIdentity[identity]; dup {
			add("§10.6: duplicate (agent %d, request_key %q): states %s and %s",
				agentID, requestKey, prev, state)
		} else {
			seenIdentity[identity] = state
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate submissions: %w", err)
	}

	if len(v.Items) > 0 {
		return v
	}
	return nil
}
