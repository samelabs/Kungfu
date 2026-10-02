package service

// Task publisher lifecycle — spec §4 transition table, one transaction
// per operation, every status change validated by the internal/task
// kernel (TaskTransition) and applied as a compare-and-swap.
//
// Error codes are the publisher-side §8.4 catalogue, verbatim:
// NOT_OWNER, INVALID_STATE (details.status), INSUFFICIENT_CREDITS,
// VALIDATION_FAILED (details.errors[]),
// HAS_RESERVATIONS.

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// -- helpers --

// validationFailed converts ValidateContract output into the §8.4
// VALIDATION_FAILED error (details.errors[] of {field, message}).
func validationFailed(fieldErrors []task.FieldError) *errors.AppError {
	items := make([]map[string]string, 0, len(fieldErrors))
	for _, fe := range fieldErrors {
		items = append(items, map[string]string{"field": fe.Field, "message": fe.Message})
	}
	return errors.NewWithDetails(400, "VALIDATION_FAILED",
		"Contract validation failed", map[string]interface{}{"errors": items})
}

// invalidTaskState is §8.4 INVALID_STATE with the current status.
func invalidTaskState(status string) *errors.AppError {
	return errors.NewWithDetails(0, "INVALID_STATE",
		fmt.Sprintf("Operation not allowed in status %q", status),
		map[string]interface{}{"status": status})
}

// lockOwnedTask loads a task by code under the row lock and enforces
// the 404 → NOT_OWNER order. The caller owns the transaction.
func lockOwnedTask(ctx context.Context, q pg.Querier, publisherID int64, code string) (*repository.TaskRow, error) {
	t, err := repository.FindTaskByCodeForUpdate(ctx, q, code)
	if goerrors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t == nil {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if t.PublisherID != publisherID {
		return nil, errors.New(0, "NOT_OWNER", "Not your task")
	}
	return t, nil
}

// marshalContract stores the effective (WithDefaults) contract JSON.
func marshalContract(c task.Contract) ([]byte, error) {
	b, err := json.Marshal(c.WithDefaults())
	if err != nil {
		return nil, fmt.Errorf("marshal contract: %w", err)
	}
	return b, nil
}

// publisherStats extends the §6.3 block with the two publisher-only
// counters (WO-18): terminal submissions inside the 30-day window and
// claims valid right now.
type publisherStats struct {
	AcceptRate         *float64 `json:"accept_rate"`
	MedianReplySeconds *float64 `json:"median_reply_seconds"`
	FailureRate        *float64 `json:"failure_rate"`
	Submissions30d     int64    `json:"submissions_30d"`
	ActiveClaims       int64    `json:"active_claims"`
}

// publisherStatsView projects the shared §6.3 statistics plus the
// publisher-only counters; the three rates keep work_get's exact scope.
func publisherStatsView(s repository.TaskStats, activeClaims int64) publisherStats {
	base := statsView(s)
	return publisherStats{
		AcceptRate:         base.AcceptRate,
		MedianReplySeconds: base.MedianReplySeconds,
		FailureRate:        base.FailureRate,
		Submissions30d:     s.TerminalTotal,
		ActiveClaims:       activeClaims,
	}
}

// taskView projects a task row plus §4 derived amounts. The contract
// IS the task's one contract column (M1).
func taskView(ctx context.Context, q pg.Querier, t *repository.TaskRow) (map[string]interface{}, error) {
	var contract task.Contract
	view := map[string]interface{}{}
	if err := json.Unmarshal(t.Contract, &contract); err == nil {
		view["title"] = contract.Title
		view["price"] = contract.Price
		view["contract"] = json.RawMessage(t.Contract)
	}
	available := t.BudgetLocked - t.Settled - t.Reserved - t.Refunded
	slots := int64(0)
	if contract.Price > 0 {
		slots = available / contract.Price
	}
	view["code"] = t.Code
	view["status"] = t.Status
	view["budget_locked"] = t.BudgetLocked
	view["settled"] = t.Settled
	view["reserved"] = t.Reserved
	view["refunded"] = t.Refunded
	view["available"] = available
	view["slots"] = slots
	if t.PausedReason != nil {
		view["paused_reason"] = *t.PausedReason
	}
	if t.ClosedReason != nil {
		view["closed_reason"] = *t.ClosedReason
	}
	view["created_at"] = t.CreatedAt.UTC().Format(time.RFC3339)
	return view, nil
}

// -- §4 transitions --

// validateHarnessRefs checks that every harness_ref is one of the
// publisher's own active memories (M3): max 10, ownership enforced,
// all violations reported at once. Pure read; the caller runs it
// inside or beside its transaction.
func validateHarnessRefs(ctx context.Context, pool *pg.Pool, publisherID int64, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	var errs []task.FieldError
	for i, ref := range refs {
		k, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, publisherID, ref)
		if err != nil {
			return errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if k == nil {
			errs = append(errs, task.FieldError{
				Field:   fmt.Sprintf("harness_refs[%d]", i),
				Message: fmt.Sprintf("%q is not one of your active memories", ref),
			})
		}
	}
	if len(errs) > 0 {
		return validationFailed(errs)
	}
	return nil
}

// CreateTask is the §4 create transition: a validated contract
// budget lock (lock_task) in one transaction. budget must cover at
// least one unit of the price (§4/§11, WO-11: no numeric minimum) and
// the publisher's balance.
func CreateTask(ctx context.Context, pool *pg.Pool, publisherID int64, contract task.Contract, budget int64) (map[string]interface{}, error) {
	defaults := contract.WithDefaults()
	if errs := task.ValidateContract(defaults); len(errs) > 0 {
		return nil, validationFailed(errs)
	}
	if budget < defaults.Price {
		return nil, validationFailed([]task.FieldError{{
			Field:   "budget",
			Message: fmt.Sprintf("budget must be at least the price (%d), got %d", defaults.Price, budget),
		}})
	}
	if budget > task.MaxAmount {
		return nil, validationFailed([]task.FieldError{{
			Field:   "budget",
			Message: fmt.Sprintf("budget must be at most %d, got %d", task.MaxAmount, budget),
		}})
	}
	if balance, err := credits.Balance(ctx, pool, publisherID); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	} else if balance < budget {
		return nil, errors.New(0, "INSUFFICIENT_CREDITS",
			fmt.Sprintf("Insufficient credits. Need %d, have %d", budget, balance))
	}
	if err := validateHarnessRefs(ctx, pool, publisherID, defaults.HarnessRefs); err != nil {
		return nil, err
	}
	contractJSON, err := marshalContract(defaults)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}
	code, err := publiccode.GenerateUnique(func(code string) (bool, error) {
		return repository.TaskCodeExists(ctx, pool, code)
	})
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Code generation failed")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	taskID, err := repository.InsertTask(ctx, tx, repository.NewTaskRow{
		Code: code, PublisherID: publisherID, Contract: contractJSON,
	})
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := repository.LockTaskBudget(ctx, pool, tx, taskID, publisherID, budget); err != nil {
		if isInsufficientCreditsErr(err) {
			return nil, errors.New(0, "INSUFFICIENT_CREDITS", "Insufficient credits")
		}
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	t, err := repository.FindTaskByID(ctx, pool, taskID)
	if err != nil || t == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return taskView(ctx, pool, t)
}

func isInsufficientCreditsErr(err error) bool {
	appErr, ok := err.(*errors.AppError)
	return ok && appErr.Code == "INSUFFICIENT_CREDITS"
}

// UpdateTask is the §4 update transition: edit the Contract while the
// task is paused. The saved contract applies immediately to every
// later submission, including those under existing claims (a claim
// keeps the amount it reserved).
func UpdateTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string, contract task.Contract) (map[string]interface{}, error) {
	defaults := contract.WithDefaults()
	if errs := task.ValidateContract(defaults); len(errs) > 0 {
		return nil, validationFailed(errs)
	}
	if err := validateHarnessRefs(ctx, pool, publisherID, defaults.HarnessRefs); err != nil {
		return nil, err
	}
	contractJSON, err := marshalContract(defaults)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}

	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err := lockOwnedTask(ctx, tx, publisherID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != task.TaskPaused {
		return nil, invalidTaskState(t.Status)
	}
	if err := repository.UpdateTaskContract(ctx, tx, t.ID, contractJSON); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	after, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || after == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return taskView(ctx, pool, after)
}

// OpenTask is the §4 open transition: paused → open, one transaction.
// M4: no outbound requests; a single transaction.
func OpenTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string) (map[string]interface{}, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err := lockOwnedTask(ctx, tx, publisherID, code)
	if err != nil {
		return nil, err
	}
	var contract task.Contract
	if err := json.Unmarshal(t.Contract, &contract); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}
	defaults := contract.WithDefaults()
	if errs := task.ValidateContract(defaults); len(errs) > 0 {
		return nil, validationFailed(errs)
	}
	if err := validateHarnessRefs(ctx, pool, publisherID, defaults.HarnessRefs); err != nil {
		return nil, err
	}
	if err := repository.ApplyTaskStatus(ctx, tx, t.ID, t.Status, task.EventOpen, nil); err != nil {
		return nil, invalidTaskState(t.Status)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	after, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || after == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return taskView(ctx, pool, after)
}

// PauseTask is the §4 pause transition (open → paused). Existing
// active claims may still submit; in-flight submissions continue.
func PauseTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string) (map[string]interface{}, error) {
	after, err := applyStatusChange(ctx, pool, code, &publisherID, task.EventPause, nil, nil)
	if err != nil {
		return nil, err
	}
	return taskView(ctx, pool, after)
}

// CloseTask is the §4 close transition (any non-closed → closed,
// terminal). Platform-closed tasks are already closed and cannot
// reopen (the kernel has no closed edge). Active claims keep their
// reservation and may still submit until expiry (no renewal);
// in-flight submissions complete — the reservations drain through the
// normal claim/submission terminal paths, after which refund applies.
func CloseTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string) (map[string]interface{}, error) {
	after, err := applyStatusChange(ctx, pool, code, &publisherID, task.EventClose, nil, nil)
	if err != nil {
		return nil, err
	}
	return taskView(ctx, pool, after)
}

// applyStatusChange is the locked §4 status-transition core shared by
// the publisher lifecycle (pause/close) and the platform close
// (WO-8b): lock the task by code — ownership-checked when publisherID
// is set — validate the event against the transition kernel, apply it
// as a compare-and-swap with an optional reason, run extra
// same-transaction writes against the locked row, commit and re-read.
func applyStatusChange(ctx context.Context, pool *pg.Pool, code string, publisherID *int64, event string, reason *string,
	extra func(ctx context.Context, tx pgx.Tx, t *repository.TaskRow) error) (*repository.TaskRow, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	var t *repository.TaskRow
	if publisherID != nil {
		t, err = lockOwnedTask(ctx, tx, *publisherID, code)
	} else {
		t, err = lockAnyTask(ctx, tx, code)
	}
	if err != nil {
		return nil, err
	}
	if _, err := task.TaskTransition(t.Status, event); err != nil {
		return nil, invalidTaskState(t.Status)
	}
	if err := repository.ApplyTaskStatus(ctx, tx, t.ID, t.Status, event, reason); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if extra != nil {
		if err := extra(ctx, tx, t); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	after, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || after == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return after, nil
}

// lockAnyTask loads a task by code under the row lock for a platform
// governance write: no ownership requirement (WO-8b).
func lockAnyTask(ctx context.Context, q pg.Querier, code string) (*repository.TaskRow, error) {
	t, err := repository.FindTaskByCodeForUpdate(ctx, q, code)
	if goerrors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t == nil {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	return t, nil
}

// FundTask is the §4 fund transition: budget_locked += amount
// (fund_task) on any non-closed task.
func FundTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string, amount int64) (map[string]interface{}, error) {
	if amount <= 0 {
		return nil, validationFailed([]task.FieldError{{
			Field: "amount", Message: fmt.Sprintf("must be a positive integer, got %d", amount),
		}})
	}
	if amount > task.MaxAmount {
		return nil, validationFailed([]task.FieldError{{
			Field: "amount", Message: fmt.Sprintf("must be at most %d, got %d", task.MaxAmount, amount),
		}})
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err := lockOwnedTask(ctx, tx, publisherID, code)
	if err != nil {
		return nil, err
	}
	if t.Status == task.TaskClosed {
		return nil, invalidTaskState(t.Status)
	}
	// budget_locked itself stays ≤ MaxAmount: the accumulated cap
	// (both operands are ≤ MaxAmount, so the sum cannot overflow).
	if t.BudgetLocked+amount > task.MaxAmount {
		return nil, validationFailed([]task.FieldError{{
			Field:   "amount",
			Message: fmt.Sprintf("funding %d would push budget_locked past the maximum %d", amount, task.MaxAmount),
		}})
	}
	if err := repository.FundTaskBudget(ctx, pool, tx, t.ID, publisherID, amount); err != nil {
		if isInsufficientCreditsErr(err) {
			return nil, errors.New(0, "INSUFFICIENT_CREDITS", "Insufficient credits")
		}
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	after, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || after == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return taskView(ctx, pool, after)
}

// RefundTask is the §4 refund transition: closed, no reservations,
// available > 0 → the available budget returns to the publisher
// (refund_task).
func RefundTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string) (map[string]interface{}, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err := lockOwnedTask(ctx, tx, publisherID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != task.TaskClosed {
		return nil, invalidTaskState(t.Status)
	}
	if t.Reserved > 0 {
		return nil, errors.NewWithDetails(0, "HAS_RESERVATIONS",
			fmt.Sprintf("Task still holds %d reserved credits", t.Reserved),
			map[string]interface{}{"reserved": t.Reserved})
	}
	if t.BudgetLocked-t.Settled-t.Reserved-t.Refunded <= 0 {
		return nil, errors.New(0, "NOTHING_TO_REFUND", "No available budget to refund")
	}
	if _, err := repository.RefundTaskAvailable(ctx, pool, tx, t.ID, publisherID); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	after, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil || after == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return taskView(ctx, pool, after)
}

// GetTask returns one owned task with its contract, the §4 derived
// amounts, and the §6.3 30-day statistics.
func GetTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string) (map[string]interface{}, error) {
	t, err := repository.FindTaskByCode(ctx, pool, code)
	if goerrors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if t == nil {
		return nil, errors.New(0, "TASK_NOT_FOUND", "Task not found")
	}
	if t.PublisherID != publisherID {
		return nil, errors.New(0, "NOT_OWNER", "Not your task")
	}
	view, err := taskView(ctx, pool, t)
	if err != nil {
		return nil, err
	}
	stats, err := repository.GetTaskStats(ctx, pool, t.ID, time.Now().Add(-statsWindow))
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	claims, err := repository.CountActiveClaims(ctx, pool, t.ID, time.Now())
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	view["stats"] = publisherStatsView(stats, claims)
	return view, nil
}

// TaskListFilter is the task_list query surface (WO-19 Q2): status,
// keyword over title, exact code and paging. All optional.
type TaskListFilter struct {
	Status   string
	Q        string
	Code     string
	Page     int
	PageSize int
}

// Normalize trims the text filters and applies the paging defaults
// (page 1; page_size 20, range 1–100).
func (f *TaskListFilter) Normalize() {
	f.Q = strings.TrimSpace(f.Q)
	f.Code = strings.TrimSpace(f.Code)
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
}

// ListTasks returns one page of the publisher's own tasks, newest
// first, with the total number of matching rows. Filtering and paging
// run in SQL; an unknown status is VALIDATION_FAILED (mirroring
// task_submissions' state filter).
func ListTasks(ctx context.Context, pool *pg.Pool, publisherID int64, filter TaskListFilter) ([]map[string]interface{}, int64, error) {
	if filter.Status != "" {
		valid := false
		for _, s := range task.TaskStatuses {
			if s == filter.Status {
				valid = true
			}
		}
		if !valid {
			return nil, 0, errors.NewWithDetails(400, "VALIDATION_FAILED",
				"Unknown task status",
				map[string]interface{}{"errors": []map[string]string{
					{"field": "status", "message": "must be one of open, paused, closed"}}})
		}
	}
	filter.Normalize()
	rows, total, err := repository.FindTasksByPublisherPage(ctx, pool, publisherID,
		filter.Status, filter.Q, filter.Code, filter.PageSize, (filter.Page-1)*filter.PageSize)
	if err != nil {
		return nil, 0, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		v, err := taskView(ctx, pool, &rows[i])
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, nil
}
