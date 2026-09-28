package service

// Task publisher lifecycle — spec §4 transition table, one transaction
// per operation, every status change validated by the internal/task
// kernel (TaskTransition) and applied as a compare-and-swap. The open
// transition additionally runs the §5.4 test delivery OUTSIDE the
// write transaction and re-locks + re-confirms the status before
// writing (§7.1 request shape).
//
// Error codes are the publisher-side §8.4 catalogue, verbatim:
// NOT_OWNER, INVALID_STATE (details.status), INSUFFICIENT_CREDITS,
// VALIDATION_FAILED (details.errors[]), TEST_DELIVERY_FAILED,
// HAS_RESERVATIONS. A missing task is 404 NOT_FOUND (not in the
// catalogue; noted in the work-order report).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// testDeliveryResponsePreviewBytes caps the receiver response excerpt
// carried in TEST_DELIVERY_FAILED details.
const testDeliveryResponsePreviewBytes = 500

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

// taskView projects a task row plus §4 derived amounts. The effective
// contract is the current version's snapshot once one exists, else the
// draft. While the task is draft or paused, draft exposes the SAVED
// draft — the contract the next open applies — and draft_pending is
// true once a version exists and that draft differs from the live
// snapshot (jsonEqual), so a paused edit is visible in task_get and in
// the task_update result instead of looking lost. open and closed
// never expose draft.
func taskView(ctx context.Context, q pg.Querier, t *repository.TaskRow) (map[string]interface{}, error) {
	contractJSON := t.DraftContract
	if t.Version >= 1 {
		if v, err := repository.FindTaskVersion(ctx, q, t.ID, t.Version); err == nil && v != nil {
			contractJSON = v.Contract
		} else if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
	}
	var contract task.Contract
	view := map[string]interface{}{}
	if err := json.Unmarshal(contractJSON, &contract); err == nil {
		// The publisher sees the whole effective contract (its own
		// receiver included) — task_update replaces it as a whole.
		view["title"] = contract.Title
		view["price"] = contract.Price
		view["contract"] = json.RawMessage(contractJSON)
	}
	if t.Status == task.TaskDraft || t.Status == task.TaskPaused {
		view["draft"] = json.RawMessage(t.DraftContract)
	}
	view["draft_pending"] = t.Version >= 1 && !jsonEqual(t.DraftContract, contractJSON)
	available := t.BudgetLocked - t.Settled - t.Reserved - t.Refunded
	slots := int64(0)
	if contract.Price > 0 {
		slots = available / contract.Price
	}
	view["code"] = t.Code
	view["status"] = t.Status
	view["version"] = t.Version
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

// jsonEqual compares two JSON documents by semantics (key order and
// whitespace irrelevant).
func jsonEqual(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return string(a) == string(b)
	}
	na, _ := json.Marshal(va)
	nb, _ := json.Marshal(vb)
	return string(na) == string(nb)
}

// -- §4 transitions --

// CreateTask is the §4 create transition: a validated draft plus the
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
// task is draft or paused. A paused edit takes effect as a NEW
// version on the next open; existing claims and submissions keep
// their version.
func UpdateTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string, contract task.Contract) (map[string]interface{}, error) {
	defaults := contract.WithDefaults()
	if errs := task.ValidateContract(defaults); len(errs) > 0 {
		return nil, validationFailed(errs)
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
	if t.Status != task.TaskDraft && t.Status != task.TaskPaused {
		return nil, invalidTaskState(t.Status)
	}
	if err := repository.UpdateDraftContract(ctx, tx, t.ID, contractJSON); err != nil {
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

// OpenTask is the §4 open transition with the §5.4 test delivery:
//
//	phase 1 (tx): lock, confirm draft/paused, read the draft contract
//	phase 2 (no tx): validate contract, snapshot owned harness, deliver
//	                 the test payload to the receiver
//	phase 3 (tx): re-lock, confirm the status did not change, write
//	              the version snapshot and open
func OpenTask(ctx context.Context, pool *pg.Pool, publisherID int64, code string) (map[string]interface{}, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	t, err := lockOwnedTask(ctx, tx, publisherID, code)
	if err != nil {
		_ = pg.Rollback(tx)
		return nil, err
	}
	status := t.Status
	taskID := t.ID
	draftJSON := t.DraftContract
	currentVersion := t.Version
	_ = pg.Rollback(tx) // phase 1 is read-only under the lock

	if _, err := task.TaskTransition(status, task.EventOpen); err != nil {
		return nil, invalidTaskState(status)
	}

	var contract task.Contract
	if err := json.Unmarshal(draftJSON, &contract); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Stored contract is not valid JSON")
	}
	defaults := contract.WithDefaults()
	if errs := task.ValidateContract(defaults); len(errs) > 0 {
		return nil, validationFailed(errs)
	}

	// Harness snapshot: every ref must be an ACTIVE memory of THIS
	// publisher (§3 harness_refs) — all violations reported at once.
	harness := make([]map[string]interface{}, 0, len(defaults.HarnessRefs))
	var harnessErrs []task.FieldError
	for i, ref := range defaults.HarnessRefs {
		k, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, publisherID, ref)
		if err != nil {
			return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
		}
		if k == nil {
			harnessErrs = append(harnessErrs, task.FieldError{
				Field:   fmt.Sprintf("harness_refs[%d]", i),
				Message: fmt.Sprintf("%q is not one of your active memories", ref),
			})
			continue
		}
		harness = append(harness, map[string]interface{}{
			"ref_id":      k.Code,
			"title":       k.Title,
			"description": k.Description,
			"content":     k.Content,
		})
	}
	if len(harnessErrs) > 0 {
		return nil, validationFailed(harnessErrs)
	}
	harnessJSON, err := json.Marshal(harness)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}
	contractJSON, err := marshalContract(defaults)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Internal error")
	}

	newVersion := currentVersion + 1
	if err := runTestDelivery(ctx, defaults, code, newVersion); err != nil {
		return nil, err
	}

	// Phase 3: write under a fresh lock; a concurrent status change —
	// or a draft edited between the phases — aborts with INVALID_STATE
	// instead of double-opening (§7c: the draft is compared by JSON
	// semantics, details.reason=DRAFT_CHANGED).
	tx, err = pool.TxBegin(ctx)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	defer func() { _ = pg.Rollback(tx) }()
	t, err = lockOwnedTask(ctx, tx, publisherID, code)
	if err != nil {
		return nil, err
	}
	if t.Status != status || t.Version != currentVersion {
		return nil, invalidTaskState(t.Status)
	}
	if !jsonEqual(t.DraftContract, draftJSON) {
		return nil, errors.NewWithDetails(0, "INVALID_STATE",
			"The draft changed while the task was opening",
			map[string]interface{}{"status": t.Status, "reason": "DRAFT_CHANGED"})
	}
	if err := repository.InsertTaskVersion(ctx, tx, taskID, newVersion, contractJSON, harnessJSON); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := repository.ApplyTaskStatus(ctx, tx, taskID, status, task.EventOpen, nil); err != nil {
		return nil, invalidTaskState(status)
	}
	if err := repository.SetTaskVersion(ctx, tx, taskID, newVersion); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}

	after, err := repository.FindTaskByID(ctx, pool, taskID)
	if err != nil || after == nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	return taskView(ctx, pool, after)
}

// runTestDelivery performs the §4 open-time test delivery: the §7.1
// request shape with agent_ref "test", the contract's sample as the
// payload, and the header Kungfu-Test: 1. The idempotency key is
// test-<code>-<version>-<random hex>, unique per open attempt, so a
// receiver that caches by key never replays a stale 500 from a
// previous attempt. The receiver must answer 2xx; any other outcome
// is TEST_DELIVERY_FAILED with the status code and the first 500
// bytes of the response (cut on a rune boundary). No reservation, no
// settlement, no submission.
func runTestDelivery(ctx context.Context, contract task.Contract, code string, version int32) error {
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Internal error")
	}
	testKey := fmt.Sprintf("test-%s-%d-%x", code, version, randBytes)
	payload := contract.Sample
	body, err := json.Marshal(map[string]json.RawMessage{
		"submission_id": json.RawMessage(`"` + testKey + `"`),
		"task_code":     json.RawMessage(`"` + code + `"`),
		"version":       json.RawMessage(fmt.Sprintf(`%d`, version)),
		"agent_ref":     json.RawMessage(`"test"`),
		"payload":       payload,
	})
	if err != nil {
		return errors.New(0, "INTERNAL_ERROR", "Internal error")
	}

	res := delivery.PostJSON(ctx, contract.Receiver.URL, body, map[string]string{
		"Idempotency-Key":     testKey,
		"Kungfu-Task":         code,
		"Kungfu-Task-Version": fmt.Sprintf("%d", version),
		"Kungfu-Test":         "1",
	}, delivery.TestTaskErrorConfig())

	if res.Success {
		return nil
	}

	details := map[string]interface{}{"status_code": 0, "response": ""}
	if res.ResponseCode != nil {
		details["status_code"] = *res.ResponseCode
	}
	if res.ResponseBody != nil {
		details["response"] = truncateRunes(*res.ResponseBody, testDeliveryResponsePreviewBytes)
	}
	message := "Test delivery to the receiver failed"
	if res.ErrorMessage != "" {
		message += ": " + res.ErrorMessage
	}
	return errors.NewWithDetails(502, "TEST_DELIVERY_FAILED", message, details)
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

// GetTask returns one owned task with its effective contract, the §4
// derived amounts, the saved draft (draft/paused) and the §6.3 30-day
// statistics — exactly the statsView scope work_get reports.
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
	view["stats"] = statsView(stats)
	return view, nil
}

// ListTasks returns the publisher's own tasks, newest first.
func ListTasks(ctx context.Context, pool *pg.Pool, publisherID int64) ([]map[string]interface{}, error) {
	rows, err := repository.ListTasksByPublisher(ctx, pool, publisherID)
	if err != nil {
		return nil, errors.New(0, "INTERNAL_ERROR", "Database error")
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		v, err := taskView(ctx, pool, &rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
