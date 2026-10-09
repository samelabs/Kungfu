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
//	§7.1 (Task 1.1) every bound contract version exists: the task's
//	   current version, every Claim's version and every Submission's
//	   version resolve to a task_contract_versions row
//	§7.2 (Task 1.2) the audience is consistent and fixed: a
//	   restricted contract ⇔ task_audience rows naming 1–50 existing
//	   agents (never the publisher), an open contract ⇔ no rows, and
//	   every published version carries the same audience
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

	// -- §7.1 (Task 1.1): every referenced contract version exists —
	//    the task's current version, every Claim's bound version and
	//    every Submission's recorded version resolve to a
	//    task_contract_versions row.
	var brokenVersions int64
	if err := q.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM tb_task t
		    WHERE t.id = $1 AND NOT EXISTS (
		      SELECT 1 FROM task_contract_versions v
		      WHERE v.task_id = t.id AND v.version = t.contract_version))
		  + (SELECT COUNT(*) FROM tb_task_claim c
		      WHERE c.task_id = $1 AND NOT EXISTS (
		        SELECT 1 FROM task_contract_versions v
		        WHERE v.task_id = c.task_id AND v.version = c.contract_version))
		  + (SELECT COUNT(*) FROM tb_task_submission s
		      WHERE s.task_id = $1 AND NOT EXISTS (
		        SELECT 1 FROM task_contract_versions v
		        WHERE v.task_id = s.task_id AND v.version = s.contract_version))`,
		taskID).Scan(&brokenVersions); err != nil {
		return fmt.Errorf("audit bound versions of task %d: %w", taskID, err)
	}
	if brokenVersions > 0 {
		add("§7.1: %d task/claim/submission rows reference a contract version with no task_contract_versions row", brokenVersions)
	}

	// -- §7.2 (Task 1.2): the audience is consistent and fixed. A
	//    restricted contract carries 1–50 resolved names as
	//    task_audience rows (agents exist by FK; the publisher is
	//    never among them), an open contract has no rows, and every
	//    published version carries the SAME audience (it is fixed at
	//    creation; canonicalized name order makes equal sets equal
	//    JSONB). Pre-1.2 contracts have no audience key → open.
	var audType string
	var audRows, publisherRows, jsonNames, driftedVersions int64
	if err := q.QueryRow(ctx, `
		SELECT
		  COALESCE(t.contract->'audience'->>'type', 'open'),
		  (SELECT COUNT(*) FROM task_audience ta WHERE ta.task_id = t.id),
		  (SELECT COUNT(*) FROM task_audience ta
		    WHERE ta.task_id = t.id AND ta.agent_id = t.publisher_id),
		  CASE WHEN jsonb_typeof(t.contract->'audience'->'agents') = 'array'
		       THEN jsonb_array_length(t.contract->'audience'->'agents')
		       ELSE 0 END,
		  (SELECT COUNT(*) FROM task_contract_versions v
		    WHERE v.task_id = t.id
		      AND COALESCE(v.contract->'audience', '{"type":"open"}'::jsonb)
		          IS DISTINCT FROM COALESCE(t.contract->'audience', '{"type":"open"}'::jsonb))
		FROM tb_task t WHERE t.id = $1`, taskID).
		Scan(&audType, &audRows, &publisherRows, &jsonNames, &driftedVersions); err != nil {
		return fmt.Errorf("audit audience of task %d: %w", taskID, err)
	}
	switch audType {
	case AudienceOpen:
		if audRows != 0 {
			add("§7.2: open audience but %d task_audience rows", audRows)
		}
	case AudienceRestricted:
		if audRows < 1 || audRows > maxAudienceAgents {
			add("§7.2: restricted audience with %d named agents (must be 1–%d)", audRows, maxAudienceAgents)
		}
		if publisherRows > 0 {
			add("§7.2: the publisher is named in its own task's audience")
		}
		if jsonNames != audRows {
			add("§7.2: contract names %d agents but %d task_audience rows exist", jsonNames, audRows)
		}
	default:
		add("§7.2: unknown audience type %q", audType)
	}
	if driftedVersions > 0 {
		add("§7.2: %d published contract versions carry a different audience than the current one", driftedVersions)
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
