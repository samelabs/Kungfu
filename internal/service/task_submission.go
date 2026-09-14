package service

import (
	"context"
	"encoding/json"
	"kungfu.md/internal/credits"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// maxTaskResponseLogBytes caps how much of a task response body is persisted to the log.
const maxTaskResponseLogBytes = 4000

// TaskSubmitResult is the return value of Submit.
type TaskSubmitResult struct {
	TaskCode string                 `json:"task_code"`
	Post     map[string]interface{} `json:"post"`
	Billing  map[string]interface{} `json:"billing"`
}

// Submit processes an agent task submission.
//
// The task row is locked BEFORE the owner POST and stays locked through
// settlement — no more "POST first, lock later":
//
//	BEGIN
//	SELECT task FOR UPDATE
//	  agentAcceptable (status=open AND fundable) else 404/409
//	POST owner API (still holding the row lock)
//	  non-2xx / delivery failure -> ROLLBACK, log, 424
//	2xx:
//	  budget -= price (auto-close when the next delivery is unfundable)
//	  credits.Record(earn_task, +price)   [same tx]
//	COMMIT
//
// Concurrency: parallel submissions serialize on the row lock; each one
// re-checks fundability under the lock, so no submission POSTs against a
// budget that cannot pay it, and the budget can never be over-delivered.
func Submit(ctx context.Context, pool *pg.Pool, taskCode string, botID int64, input map[string]interface{}) (*TaskSubmitResult, error) {
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	// 1. Lock the task row — the serialization point for the whole flow.
	task, err := repository.FindTaskByCodeForUpdate(ctx, tx, taskCode)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	if task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}

	postapi := ""
	if task.PostAPI != nil {
		postapi = strings.TrimSpace(*task.PostAPI)
	}
	price := task.Price
	code := task.Code

	// 2. Full TaskCheck contract under the lock, in the documented order:
	//    status, then postapi structure, then price, then fundability.
	//    Every failure rolls back FIRST (releasing the transaction and row
	//    lock) and only then writes the best-effort log on the pool — a
	//    log write while the tx still holds the pool's only connection
	//    deadlocks a MaxConns=1 pool. Zero POST hits, no settlement.
	if task.Status != taskStatusOpen {
		rule := RaiseRule("TASK_NOT_OPEN")
		_ = tx.Rollback(ctx)
		insertTaskEventLog(ctx, pool, code, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}
	if rule := ValidatePostapi(postapi, 2048); rule != nil {
		_ = tx.Rollback(ctx)
		insertTaskEventLog(ctx, pool, code, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}
	if rule := ValidatePrice(price); rule != nil {
		_ = tx.Rollback(ctx)
		insertTaskEventLog(ctx, pool, code, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}
	if !fundable(task.Budget, price) {
		rule := RaiseRule("TASK_BUDGET_EXHAUSTED")
		_ = tx.Rollback(ctx)
		insertTaskEventLog(ctx, pool, code, botID, "kfcheck", nil, false, nil, nil, rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}

	// 3. POST to owner's API while holding the task row lock.
	payload := delivery.BuildPayload(code, input)
	payloadBytes, _ := json.Marshal(payload)

	postResult := delivery.PostJSON(postapi, payloadBytes, delivery.AgentSubmitErrorConfig())

	if !postResult.Success {
		_ = tx.Rollback(ctx)
		insertTaskEventLog(ctx, pool, code, botID, "post_failed", nil, false,
			postResult.ResponseCode, nil, postResult.ErrorCode, "")
		return nil, errors.NewWithDetails(424,
			ifEmpty(postResult.ErrorCode, "TASK_POST_FAILED"),
			"Task delivery failed. Please retry later.",
			map[string]interface{}{
				"post": map[string]interface{}{
					"delivered":     false,
					"response_code": postResult.ResponseCode,
				},
			})
	}

	// 4. Settle under the same lock and transaction: budget decrement +
	//    earn_task credit. Any budget write failure rolls everything back —
	//    the agent is never paid for a budget the task does not have.
	balance, settleErr := settleLockedTask(ctx, pool, tx, task, botID, price)
	if settleErr != nil {
		return nil, settleErr
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling task")
	}

	// 5. Log success (post-commit, best-effort).
	var respBodyForLog *string
	if postResult.ResponseBody != nil {
		truncated := truncateForLog(*postResult.ResponseBody)
		respBodyForLog = &truncated
	}
	insertTaskEventLog(ctx, pool, code, botID, "post_succeeded", nil, true,
		postResult.ResponseCode, respBodyForLog, "", "")

	var respCodeVal interface{}
	if postResult.ResponseCode != nil {
		respCodeVal = *postResult.ResponseCode
	}
	logOperation(ctx, pool, &botID, "task_submit", strPtr("task"), &code,
		map[string]interface{}{
			"reward":        price,
			"response_code": respCodeVal,
		}, true)

	return &TaskSubmitResult{
		TaskCode: code,
		Post: map[string]interface{}{
			"delivered":     true,
			"response_code": derefInt(postResult.ResponseCode),
		},
		Billing: map[string]interface{}{
			"reward":  price,
			"balance": balance,
		},
	}, nil
}

// settleLockedTask decrements the budget (with unified auto-close) and
// awards the agent credit inside the caller's transaction. The task row
// is already locked by the caller. A budget write failure returns an
// error — earn_task can never land without the budget write succeeding.
func settleLockedTask(ctx context.Context, pool *pg.Pool, tx pgx.Tx,
	task *repository.TaskForUpdate, botID int64, price float64) (float64, error) {

	// Decrement budget with conditional auto-close (repository takes the
	// minimum threshold explicitly; the unified rule also closes when the
	// remainder can no longer pay one more delivery).
	if err := repository.DecrementTaskBudgetForDelivery(ctx, tx, task.ID, price, MinOpenBudget); err != nil {
		return 0, errors.New(500, "INTERNAL_ERROR", "Error settling task budget")
	}

	// Award credit in the same transaction.
	balance, err := credits.Record(ctx, pool, tx, botID, "earn_task", price, strPtr("task"), &task.Code)
	if err != nil {
		return 0, err
	}
	return balance, nil
}

// insertTaskEventLog writes a task delivery log entry (best-effort: the
// business flow is never failed by a log write, but failures are logged).
func insertTaskEventLog(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	action string, payload map[string]interface{}, success bool,
	responseCode *int, responseBody *string, errorCode, errorMessage string) {

	// Log-sink hardening: the persisted copies of payload / response /
	// error text pass through the existing Kungfu API-key redaction. The
	// actual PostAPI delivery payload is never touched.
	if payload != nil {
		payload = security.RedactSecrets(payload).(map[string]interface{})
	}
	if responseBody != nil {
		rb := security.RedactSecrets(*responseBody).(string)
		responseBody = &rb
	}
	errorMessage = security.RedactSecrets(errorMessage).(string)

	var payloadJSON *string
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err == nil {
			s := string(encoded)
			payloadJSON = &s
		}
	}

	if err := repository.InsertTaskLog(ctx, pool, repository.NewTaskLogInput{
		TaskCode:     taskCode,
		BotID:        &botID,
		Action:       action,
		PayloadJSON:  payloadJSON,
		ResponseCode: responseCode,
		ResponseBody: responseBody,
		Success:      success,
		ErrorCode:    strPtrOrNil(errorCode),
		ErrorMessage: strPtrOrNil(errorMessage),
	}); err != nil {
		log.Printf("task log write failed: task=%s action=%s err=%v", taskCode, action, err)
	}
}

func truncateForLog(value string) string {
	return truncateUTF8ForLog(value, maxTaskResponseLogBytes)
}

// truncateUTF8ForLog normalizes invalid UTF-8, then truncates to the byte
// budget without cutting inside a rune, appending the existing
// "... [truncated]" marker when truncation happens. Used for every
// remotely-sourced string before it reaches a task log or API preview.
func truncateUTF8ForLog(value string, maxBytes int) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "... [truncated]"
}

// logOperation is a best-effort audit wrapper: the main business flow is
// never failed by an audit write, but a failed audit write is logged as a
// warning (never silent). Payloads are masked by the repository layer before
// persistence; only non-sensitive context (action, error) reaches the log.
func logOperation(ctx context.Context, q pg.Querier, botID *int64, action string,
	targetType, targetID *string, requestData map[string]interface{}, success bool) {
	if err := repository.InsertOperationLog(ctx, q, repository.LogInsertData{
		BotID:       botID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		RequestData: requestData,
		Success:     success,
	}); err != nil {
		log.Printf("audit log write failed: action=%s target=%v err=%v", action, targetID, err)
	}
}

// Helper functions

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case float32:
			return float64(n)
		case int64:
			return float64(n)
		case int:
			return float64(n)
		}
	}
	return 0
}

func strPtr(s string) *string { return &s }

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ifEmpty(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func derefInt(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
