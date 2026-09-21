package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
)

// TaskSubmissionRepository persists durable submission rows.
//
// Authority boundary: the ONLY writers of tb_task_submissions and of
// tb_tasks.reserved_budget are the submission service primitives
// (accept/reserve, claim, record-outcome, settle/release) — synchronous
// requests and the recovery worker share them; REST/MCP handlers and
// any worker loop never touch this SQL directly.

// Submission states (010 schema CHECK contract).
const (
	SubStateReserved   = "reserved"
	SubStateDelivering = "delivering"
	SubStateDelivered  = "delivered"
	SubStateSettled    = "settled"
	SubStateRejected   = "rejected"
	SubStateUncertain  = "uncertain"
)

// Submission kinds.
const (
	SubKindAgent     = "agent"
	SubKindOwnerTest = "owner_test"
)

// SubmissionRow is the full durable submission fact set.
type SubmissionRow struct {
	ID               int64
	Code             string
	TaskID           int64
	TaskCode         string
	BotID            int64
	Kind             string
	ClientRequestKey string
	PayloadHash      string
	PayloadBody      *string
	PostAPISnapshot  string
	PriceSnapshot    int64
	ReservedAmount   int64
	State            string
	AttemptCount     int64
	LeaseUntil       *time.Time
	NextAttemptAt    *time.Time
	ResponseCode     *int
	ResponsePreview  *string
	SettledBalance   *int64
	BudgetAfter      *int64
	TaskStatusAfter  *string
	LastErrorCode    *string
	LastErrorMessage *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeliveredAt      *time.Time
	SettledAt        *time.Time
}

// TaskWithReserved extends the locked-task projection with reserved_budget.
// FindTaskByCodeForUpdate gains this column in 010.
type TaskWithReserved struct {
	ID       int64
	Code     string
	BotID    int64
	PostAPI  *string
	Budget   int64
	Reserved int64
	Price    int64
	Status   string
}

// FindTaskWithReservedForUpdate locks the task row and returns the
// budget/reserved/price/status projection. CALLER HOLDS THIS LOCK for the
// whole accept/claim/settle transaction (single serialization point).
func FindTaskWithReservedForUpdate(ctx context.Context, q pg.Querier, code string) (*TaskWithReserved, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, postapi, budget, reserved_budget, price, status
		FROM tb_tasks
		WHERE code = $1
		FOR UPDATE`, code)
	var t TaskWithReserved
	if err := row.Scan(&t.ID, &t.Code, &t.BotID, &t.PostAPI, &t.Budget, &t.Reserved, &t.Price, &t.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// FindSubmissionByCodeForUpdate locks one submission row by code.
// Lock order contract: task row FIRST, then submission row.
func FindSubmissionByCodeForUpdate(ctx context.Context, q pg.Querier, code string) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+`
		WHERE code = $1
		FOR UPDATE`, code))
}

// FindSubmissionByCode is the lock-free read (duplicate replay path after
// identity lookup decides no mutation is needed).
func FindSubmissionByCode(ctx context.Context, q pg.Querier, code string) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+`
		WHERE code = $1`, code))
}

// FindSubmissionByIdentityForUpdate locks the submission identified by the
// client idempotency contract (task_id, bot_id, kind, client_request_key).
func FindSubmissionByIdentityForUpdate(ctx context.Context, q pg.Querier, taskID, botID int64, kind, key string) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+`
		WHERE task_id = $1 AND bot_id = $2 AND kind = $3 AND client_request_key = $4
		FOR UPDATE`, taskID, botID, kind, key))
}

// InsertSubmissionReserved inserts a new submission in state 'reserved' and
// bumps the task's reserved_budget by reservedAmount in the same locked
// transaction. Caller has validated available_budget >= price under the task
// row lock. The reserved_budget <= budget CHECK backstops the invariant.
func InsertSubmissionReserved(ctx context.Context, q pg.Querier, s *SubmissionRow) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_task_submissions (
			code, task_id, task_code, bot_id, kind, client_request_key,
			payload_hash, payload_body, postapi_snapshot, price_snapshot,
			reserved_amount, state, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$10, 'reserved', NOW(), NOW()
		)`, s.Code, s.TaskID, s.TaskCode, s.BotID, s.Kind, s.ClientRequestKey,
		s.PayloadHash, s.PayloadBody, s.PostAPISnapshot, s.PriceSnapshot)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		UPDATE tb_tasks
		SET reserved_budget = reserved_budget + $1, updated_at = NOW()
		WHERE id = $2`, s.ReservedAmount, s.TaskID)
	return err
}

// ClaimSubmissionForDelivery is the SINGLE claim primitive shared by the
// synchronous request path and the recovery worker:
//
//	state reserved/uncertain -> delivering (attempt_count += 1, lease set)
//
// Transitioning out of 'uncertain' on a NEW attempt is allowed only when the
// caller passes allowUncertain=true (owner-confirmed retry); the worker never
// auto-retries uncertain submissions. Returns the updated row or nil when the
// claim did not happen (wrong state, lease still held by another claimer).
func ClaimSubmissionForDelivery(ctx context.Context, q pg.Querier, submissionID int64, leaseUntil time.Time, allowUncertain bool) (*SubmissionRow, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_submissions
		SET state = 'delivering',
		    attempt_count = attempt_count + 1,
		    lease_until = $2,
		    next_attempt_at = NULL,
		    updated_at = NOW()
		WHERE id = $1
		  AND (state = 'reserved'
		       OR (state = 'uncertain' AND $3)
		       OR (state = 'delivering' AND lease_until < NOW()))`,
		submissionID, leaseUntil, allowUncertain)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	// re-read the claimed row (locked in this tx)
	return scanSubmission(q.QueryRow(ctx, submissionSelect+` WHERE id = $1`, submissionID))
}

// ErrStaleAttempt is returned when a remote outcome write does not match
// the CURRENT delivering attempt (a newer attempt has taken over the
// submission). The write is a no-op: the stale attempt must not modify the
// submission, the task budget, the reservation, or the ledger.
var ErrStaleAttempt = errors.New("stale delivery attempt: outcome discarded")

// RecordOutcomeDelivered durably marks a 2xx outcome. From this durable
// write on, the submission is NEVER posted again.
//
// Attempt fencing: the write only lands when the submission is currently
// delivering AND its attempt_count equals the claimed attempt. A stale
// attempt (superseded by a reclaim after lease expiry) is a NO-OP — it
// must never rewrite a terminal row (settled stays settled, rejected
// stays rejected) nor move a row owned by a newer attempt.
func RecordOutcomeDelivered(ctx context.Context, q pg.Querier, submissionID int64, attempt int64, responseCode int, responsePreview *string, deliveredAt time.Time) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_submissions
		SET state = 'delivered',
		    response_code = $2,
		    response_preview = $3,
		    delivered_at = $4,
		    lease_until = NULL,
		    last_error_code = NULL,
		    last_error_message = NULL,
		    updated_at = NOW()
		WHERE id = $1
		  AND state = 'delivering'
		  AND attempt_count = $5`,
		submissionID, responseCode, responsePreview, deliveredAt, attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleAttempt
	}
	return nil
}

// RecordOutcomeRejected marks a definitive non-2xx outcome and releases the
// reservation back to available budget in the same transaction. Terminal.
// RecordOutcomeRejected marks a definitive non-2xx outcome and releases the
// reservation back to available budget in the same transaction. Terminal:
// rejected never transitions back. Attempt-fenced like the other outcome
// writers — a stale attempt neither writes the row nor releases anything.
func RecordOutcomeRejected(ctx context.Context, q pg.Querier, submissionID int64, taskID int64, attempt int64, responseCode int, responsePreview *string, errCode, errMsg string) error {
	var reserved int64
	if err := q.QueryRow(ctx,
		`SELECT reserved_amount FROM tb_task_submissions WHERE id = $1`, submissionID).Scan(&reserved); err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_submissions
		SET state = 'rejected',
		    response_code = $2,
		    response_preview = $3,
		    lease_until = NULL,
		    payload_body = NULL,
		    last_error_code = $4,
		    last_error_message = $5,
		    updated_at = NOW()
		WHERE id = $1
		  AND state = 'delivering'
		  AND attempt_count = $6`,
		submissionID, responseCode, responsePreview, errCode, errMsg, attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleAttempt
	}
	_, err = q.Exec(ctx, `
		UPDATE tb_tasks
		SET reserved_budget = reserved_budget - $1, updated_at = NOW()
		WHERE id = $2 AND reserved_budget >= $1`, reserved, taskID)
	return err
}

// RecordOutcomeUncertain marks an unknown remote outcome. Reservation is
// RETAINED. Never treated as definitive failure; no auto-release.
// Attempt-fenced: a stale attempt recording uncertain must not clobber a
// newer attempt's state (including a terminal one).
func RecordOutcomeUncertain(ctx context.Context, q pg.Querier, submissionID int64, attempt int64, errCode, errMsg string, nextAttemptAt time.Time) error {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_submissions
		SET state = 'uncertain',
		    lease_until = NULL,
		    next_attempt_at = $2,
		    last_error_code = $3,
		    last_error_message = $4,
		    updated_at = NOW()
		WHERE id = $1
		  AND state = 'delivering'
		  AND attempt_count = $5`,
		submissionID, nextAttemptAt, errCode, errMsg, attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleAttempt
	}
	return nil
}

// SettleSubmission is the SINGLE settlement authority for agent submissions:
//
//	task: budget -= reserved, reserved_budget -= reserved
//	credits: earn_task +price_snapshot (caller performs via credits.Record)
//	submission: state delivered -> settled, replay facts stored, payload erased
//
// The credits.Record call happens between the task mutation and this call,
// inside the caller's single transaction. Returns false when the row was
// already settled (idempotent replay).
func SettleSubmission(ctx context.Context, q pg.Querier, submissionID int64, settledBalance int64, budgetAfter int64, taskStatusAfter string, settledAt time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_task_submissions
		SET state = 'settled',
		    settled_balance = $2,
		    budget_after = $3,
		    task_status_after = $4,
		    settled_at = $5,
		    lease_until = NULL,
		    payload_body = NULL,
		    updated_at = NOW()
		WHERE id = $1 AND state = 'delivered'`,
		submissionID, settledBalance, budgetAfter, taskStatusAfter, settledAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SettleOwnerTestSubmission is the owner_test settlement variant: same task
// mutations, no earn_task (the caller never calls credits.Record).
func SettleOwnerTestSubmission(ctx context.Context, q pg.Querier, submissionID int64, budgetAfter int64, taskStatusAfter string, settledAt time.Time) (bool, error) {
	return SettleSubmission(ctx, q, submissionID, 0, budgetAfter, taskStatusAfter, settledAt)
}

// DecrementTaskBudgetOnSettle performs the task-side economic mutation of
// settlement under the held task lock: budget -= reservedAmount,
// reserved_budget -= reservedAmount. Auto-close (open tasks only) applies
// the existing rule to the post-settlement AVAILABLE budget
// (budget - reserved_budget after the settlement): available below
// MinOpenBudget or below the CURRENT task price closes the task.
func DecrementTaskBudgetOnSettle(ctx context.Context, q pg.Querier, taskID int64, reservedAmount, minBudget int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET budget = budget - $1,
		    reserved_budget = reserved_budget - $1,
		    status = CASE
		        WHEN status = 'open' AND ((budget - $1) - (reserved_budget - $1) < $2
		                                  OR (budget - $1) - (reserved_budget - $1) < price)
		        THEN 'closed' ELSE status END,
		    closed_at = CASE
		        WHEN status = 'open' AND ((budget - $1) - (reserved_budget - $1) < $2
		                                  OR (budget - $1) - (reserved_budget - $1) < price)
		        THEN NOW() ELSE closed_at END,
		    updated_at = NOW()
		WHERE id = $3`,
		reservedAmount, minBudget, taskID)
	return err
}

// ClaimRecoverableSubmissions returns up to limit submission ids the recovery
// worker should consider: reserved rows whose next_attempt_at has arrived,
// delivering rows whose lease expired, and delivered rows not yet settled.
// FOR UPDATE SKIP LOCKED keeps multiple workers from grabbing the same rows;
// the claim primitive re-checks state under the lock.
func ClaimRecoverableSubmissions(ctx context.Context, q pg.Querier, limit int) ([]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT id FROM tb_task_submissions
		WHERE (state = 'reserved' AND (next_attempt_at IS NULL OR next_attempt_at <= NOW()))
		   OR (state = 'delivering' AND lease_until < NOW())
		   OR (state = 'delivered')
		ORDER BY updated_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReleaseReservationOnTask removes the reservation of a terminal
// (rejected) submission from a task — used by RecordOutcomeRejected's
// caller when the release must be conditional on reserved_budget > 0.
func ReleaseReservationOnTask(ctx context.Context, q pg.Querier, taskID, amount int64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET reserved_budget = reserved_budget - $1, updated_at = NOW()
		WHERE id = $2 AND reserved_budget >= $1`, amount, taskID)
	return err
}

// submissionSelect is the canonical column list.
const submissionSelect = `
	SELECT id, code, task_id, task_code, bot_id, kind, client_request_key,
	       payload_hash, payload_body, postapi_snapshot, price_snapshot,
	       reserved_amount, state, attempt_count, lease_until, next_attempt_at,
	       response_code, response_preview, settled_balance, budget_after,
	       task_status_after, last_error_code, last_error_message,
	       created_at, updated_at, delivered_at, settled_at
	FROM tb_task_submissions`

func scanSubmission(row pgx.Row) (*SubmissionRow, error) {
	var s SubmissionRow
	err := row.Scan(&s.ID, &s.Code, &s.TaskID, &s.TaskCode, &s.BotID, &s.Kind,
		&s.ClientRequestKey, &s.PayloadHash, &s.PayloadBody, &s.PostAPISnapshot,
		&s.PriceSnapshot, &s.ReservedAmount, &s.State, &s.AttemptCount,
		&s.LeaseUntil, &s.NextAttemptAt, &s.ResponseCode, &s.ResponsePreview,
		&s.SettledBalance, &s.BudgetAfter, &s.TaskStatusAfter,
		&s.LastErrorCode, &s.LastErrorMessage,
		&s.CreatedAt, &s.UpdatedAt, &s.DeliveredAt, &s.SettledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// FindSubmissionByIdentity is the lock-free identity lookup (response
// projection after processing).
func FindSubmissionByIdentity(ctx context.Context, q pg.Querier, taskID, botID int64, kind, key string) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+`
		WHERE task_id = $1 AND bot_id = $2 AND kind = $3 AND client_request_key = $4`,
		taskID, botID, kind, key))
}

// FindSubmissionByID is the lock-free primary-key lookup (recovery scan
// follow-up).
func FindSubmissionByID(ctx context.Context, q pg.Querier, id int64) (*SubmissionRow, error) {
	return scanSubmission(q.QueryRow(ctx, submissionSelect+`
		WHERE id = $1`, id))
}
