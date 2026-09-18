package service

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"

	"kungfu.md/internal/delivery"
)

// pgQuerier is satisfied by pgx.Tx (and *pg.Pool) — the settlement helper
// runs inside the caller's transaction.
type pgQuerier = pgx.Tx

// TestTaskService provides task delivery testing for owners.
//
// Documented flow: create pending -> owner test -> open. The owner test:
//   - allowed on pending AND open tasks (closed is not testable);
//   - locks the task row, validates postapi/price/budget (unified rules),
//     POSTs to the owner API while holding the lock;
//   - on 2xx the budget settles (budget -= price) with NO earn_task;
//   - a pending task stays pending after a successful test;
//   - an open task auto-closes when the remainder is unfundable;
//   - any settlement failure is an API error — never a success response
//     with "status":"error" hidden in the billing map.

const (
	testMaxResponseBytes  = 16000 // MAX_RESPONSE_BYTES
	testDBResponseBodyMax = 65535 // DB_RESPONSE_BODY_MAX
	testDBErrorMessageMax = 256   // DB_ERROR_MESSAGE_MAX
	testDBPayloadJSONMax  = 60000 // DB_PAYLOAD_JSON_MAX
)

// TestTaskResult is the return value of TestTaskDeliver.
type TestTaskResult struct {
	TaskCode string                 `json:"task_code"`
	Post     map[string]interface{} `json:"post"`
	Billing  map[string]interface{} `json:"billing"`
}

// TestTaskDeliver lets an owner test their own task's postapi.
//
//	BEGIN
//	SELECT task FOR UPDATE (owner check: bot_id)
//	  closed -> 409; pending/open continue
//	unified gate: postapi/price valid + fundable
//	POST owner API (row lock held)
//	  failure -> ROLLBACK, log, 424 (budget untouched)
//	2xx:
//	  budget -= price (auto-close only for open tasks that became unfundable)
//	COMMIT
func TestTaskDeliver(ctx context.Context, pool *pg.Pool, botID int64, code string, input map[string]interface{}) (*TestTaskResult, error) {
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	defer func() { _ = pg.Rollback(tx) }() // no-op after commit

	// 1. Lock the task row.
	task, err := repository.FindTaskByCodeForUpdate(ctx, tx, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	if task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}
	if task.BotID != botID {
		return nil, errors.New(403, "NOT_OWNER", "Only the task owner can test this task")
	}

	// Explicit legal set: only pending and open are testable (the
	// documented pending -> test -> open flow plus open compatibility).
	// Any other status — closed or anything abnormal — is 409.
	if task.Status != taskStatusPending && task.Status != taskStatusOpen {
		return nil, errors.New(409, "TASK_NOT_OPEN", "Only pending or open tasks can be tested")
	}

	postapi := ""
	if task.PostAPI != nil {
		postapi = strings.TrimSpace(*task.PostAPI)
	}
	price := task.Price

	// 2. Full existing validation under the lock: postapi structure,
	//    price, fundability — the same TaskCheck contract as submit.
	//    Gate failures roll back FIRST (releasing the tx/connection) and
	//    only then write the best-effort log on the pool.
	if rule := ValidatePostapi(postapi, 2048); rule != nil {
		_ = pg.Rollback(tx)
		testLogEvent(ctx, pool, code, botID, "kfcheck", input, false, nil, nil,
			rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}
	if rule := ValidatePrice(price); rule != nil {
		_ = pg.Rollback(tx)
		testLogEvent(ctx, pool, code, botID, "kfcheck", input, false, nil, nil,
			rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}
	if !fundable(task.Budget, price) {
		rule := RaiseRule("TASK_BUDGET_EXHAUSTED")
		_ = pg.Rollback(tx)
		testLogEvent(ctx, pool, code, botID, "kfcheck", input, false, nil, nil,
			rule.Rule.Code, rule.Rule.LogMsg)
		return nil, rule.ToAppError()
	}

	// 3. POST to owner's API while holding the row lock.
	payload := delivery.BuildPayload(code, input)
	payloadBytes, _ := json.Marshal(payload)

	postResult := delivery.PostJSON(ctx, postapi, payloadBytes, delivery.TestTaskErrorConfig())

	if !postResult.Success {
		_ = pg.Rollback(tx)
		testLogEvent(ctx, pool, code, botID, "post_failed", payload, false,
			postResult.ResponseCode, postResult.ResponseBody,
			postResult.ErrorCode, postResult.ErrorMessage)

		return nil, errors.NewWithDetails(424,
			ifEmpty(postResult.ErrorCode, "TESTTASK_POST_FAILED"),
			ifEmpty(postResult.ErrorMessage, "Task test delivery failed"),
			map[string]interface{}{
				"post": map[string]interface{}{
					"delivered":     false,
					"response_code": postResult.ResponseCode,
					"response_body": testTruncateResponse(derefStr(postResult.ResponseBody)),
				},
			})
	}

	// 4. Settle: budget -= price, no earn_task. Settlement failure is an
	// API error (never a success response with an error status inside).
	nextBudget, mustClose, settleErr := testSettleLockedTask(ctx, tx, task, price)
	if settleErr != nil {
		return nil, settleErr
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error settling task test")
	}

	// 5. Log success (post-commit, best-effort).
	testLogEvent(ctx, pool, code, botID, "post_succeeded", payload, true,
		postResult.ResponseCode, postResult.ResponseBody, "", "")

	finalStatus := task.Status
	if mustClose {
		finalStatus = taskStatusClosed
	}
	return &TestTaskResult{
		TaskCode: code,
		Post: map[string]interface{}{
			"delivered":     true,
			"response_code": postResult.ResponseCode,
			"response_body": testTruncateResponse(derefStr(postResult.ResponseBody)),
		},
		Billing: map[string]interface{}{
			"cost":   price,
			"budget": nextBudget,
			"status": finalStatus,
		},
	}, nil
}

// testSettleLockedTask decrements the budget for a successful owner test,
// with no credit award. pending stays pending; open auto-closes when the
// remainder can no longer fund one more delivery.
func testSettleLockedTask(ctx context.Context, tx pgQuerier, task *repository.TaskForUpdate, price float64) (float64, bool, error) {
	nextBudget := task.Budget - price
	mustClose := false
	nextStatus := task.Status
	if task.Status == taskStatusOpen {
		if _, closeIt := nextBudgetAfterDelivery(task.Budget, price); closeIt {
			mustClose = true
			nextStatus = taskStatusClosed
		}
	}

	if err := repository.UpdateTaskBudgetAndStatus(ctx, tx, task.ID, nextBudget, nextStatus, mustClose); err != nil {
		return 0, false, errors.New(500, "INTERNAL_ERROR", "Error settling task test budget")
	}
	return nextBudget, mustClose, nil
}

// testLogEvent writes a task delivery log entry .
func testLogEvent(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	action string, payload map[string]interface{}, success bool,
	responseCode *int, responseBody *string, errorCode, errorMessage string) {

	// Log-sink hardening: persisted copies pass redaction BEFORE any
	// truncation (a secret spanning the boundary must not survive as a
	// partial unmasked key); the actual PostAPI payload is untouched.
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
			if len(s) <= testDBPayloadJSONMax {
				payloadJSON = &s
			} else {
				// Truncate preview
				previewLen := testDBPayloadJSONMax - 120
				if previewLen < 0 {
					previewLen = 0
				}
				preview := normalizeTruncateUTF8(s, previewLen, false)
				wrapper := map[string]interface{}{
					"_truncated": true,
					"bytes":      len(s),
					"preview":    preview,
				}
				wrapped, wErr := json.Marshal(wrapper)
				if wErr != nil || len(string(wrapped)) > testDBPayloadJSONMax {
					fallback := `{"_truncated":true}`
					payloadJSON = &fallback
				} else {
					ws := string(wrapped)
					payloadJSON = &ws
				}
			}
		}
	}

	// DB columns are marker-free budgets (VARCHAR caps); the API preview
	// paths keep their "... [truncated]" marker via testTruncateResponse.
	var respBodyForLog *string
	if responseBody != nil {
		truncated := normalizeTruncateUTF8(*responseBody, testDBResponseBodyMax, false)
		respBodyForLog = &truncated
	}

	var errMsgForLog *string
	if errorMessage != "" {
		truncated := normalizeTruncateUTF8(errorMessage, testDBErrorMessageMax, false)
		errMsgForLog = &truncated
	}

	if err := repository.InsertTaskLog(ctx, pool, repository.NewTaskLogInput{
		TaskCode:     taskCode,
		BotID:        &botID,
		Action:       action,
		PayloadJSON:  payloadJSON,
		ResponseCode: responseCode,
		ResponseBody: respBodyForLog,
		Success:      success,
		ErrorCode:    strPtrOrNil(errorCode),
		ErrorMessage: errMsgForLog,
	}); err != nil {
		log.Printf("task log write failed: task=%s action=%s err=%v", taskCode, action, err)
	}
}

func testTruncateResponse(value string) string {
	return normalizeTruncateUTF8(value, testMaxResponseBytes, true)
}

// derefStr safely dereferences a *string.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
