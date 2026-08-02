package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
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

// Flow:
// 1. TaskCheck: validate postapi/price/budget
// 2. POST to owner's API (10s timeout, no redirect following)
// 3. If POST failed → log + return 424
// 4. If POST succeeded → settle: decrement budget + award credit (in transaction)
// 5. Log task event + operation log
func Submit(ctx context.Context, pool *pg.Pool, task map[string]interface{}, botID int64, input map[string]interface{}) (*TaskSubmitResult, error) {
	// Extract task fields
	postapi := strings.TrimSpace(getString(task, "postapi"))
	price := getFloat(task, "price")
	taskCode := getString(task, "code")

	// 1. TaskCheck
	checkErr := RunTaskCheck(postapi, price, func() *TaskCheckError {
		return ensureBudgetAvailable(ctx, pool, taskCode, price)
	})

	if checkErr != nil {
		// Log the check failure
		insertTaskEventLog(ctx, pool, taskCode, botID, "kfcheck", nil, false, nil, nil, checkErr.Rule.Code, checkErr.Rule.LogMsg)
		return nil, checkErr.ToAppError()
	}

	// 2. POST to owner's API
	payload := delivery.BuildPayload(taskCode, input)
	payloadBytes, _ := json.Marshal(payload)

	postResult := delivery.PostJSON(postapi, payloadBytes, delivery.AgentSubmitErrorConfig())

	if !postResult.Success {
		// Log failure
		insertTaskEventLog(ctx, pool, taskCode, botID, "post_failed", nil, false,
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

	// 4. Settle: decrement budget + award credit
	balance, settleErr := settleDeliveredSubmission(ctx, pool, taskCode, botID, price)
	if settleErr != nil {
		return nil, settleErr
	}

	// 5. Log success
	var respBodyForLog *string
	if postResult.ResponseBody != nil {
		truncated := truncateForLog(*postResult.ResponseBody)
		respBodyForLog = &truncated
	}
	insertTaskEventLog(ctx, pool, taskCode, botID, "post_succeeded", nil, true,
		postResult.ResponseCode, respBodyForLog, "", "")

	// Operation log
	var respCodeVal interface{}
	if postResult.ResponseCode != nil {
		respCodeVal = *postResult.ResponseCode
	}
	logOperation(ctx, pool, &botID, "task_submit", strPtr("task"), &taskCode,
		map[string]interface{}{
			"reward":        price,
			"response_code": respCodeVal,
		}, true)

	return &TaskSubmitResult{
		TaskCode: taskCode,
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

// ensureBudgetAvailable checks task budget/status without locking.
func ensureBudgetAvailable(ctx context.Context, pool *pg.Pool, taskCode string, price float64) *TaskCheckError {
	task, err := repository.FindTaskBudgetStatusByCode(ctx, pool, taskCode)
	if err != nil || task == nil {
		return RaiseRule("TASK_NOT_OPEN")
	}
	if task.Status != "open" {
		return RaiseRule("TASK_NOT_OPEN")
	}
	if task.Budget < price || task.Budget < MinOpenBudget {
		return RaiseRule("TASK_BUDGET_EXHAUSTED")
	}
	return nil
}

// settleDeliveredSubmission decrements task budget and awards credit in one transaction.
func settleDeliveredSubmission(ctx context.Context, pool *pg.Pool, taskCode string, botID int64, price float64) (float64, error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// SELECT FOR UPDATE on task
	task, err := repository.FindTaskBudgetStatusByCodeForUpdate(ctx, tx, taskCode)
	if err != nil || task == nil {
		return 0, errors.New(409, "TASK_NOT_OPEN", "Task is not open for submissions")
	}

	if task.Status != "open" {
		return 0, errors.New(409, "TASK_NOT_OPEN", "Task is not open for submissions")
	}

	if task.Budget < price || task.Budget < MinOpenBudget {
		return 0, errors.New(409, "TASK_BUDGET_EXHAUSTED", "Task budget is not enough for this submission")
	}

	// Decrement budget (atomic SQL with conditional close)
	repository.DecrementTaskBudgetForDelivery(ctx, tx, task.ID, price)

	// Award credit (nested in same transaction)
	balance, err := Record(ctx, pool, tx, botID, "earn_task", price, strPtr("task"), &taskCode)
	if err != nil {
		return 0, err
	}

	// Commit
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	tx = nil // prevent deferred rollback

	return balance, nil
}

// insertTaskEventLog writes a task delivery log entry.
func insertTaskEventLog(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	action string, payload map[string]interface{}, success bool,
	responseCode *int, responseBody *string, errorCode, errorMessage string) {

	var payloadJSON *string
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err == nil {
			s := string(encoded)
			payloadJSON = &s
		}
	}

	_ = repository.InsertTaskLog(ctx, pool, repository.NewTaskLogInput{
		TaskCode:     taskCode,
		BotID:        &botID,
		Action:       action,
		PayloadJSON:  payloadJSON,
		ResponseCode: responseCode,
		ResponseBody: responseBody,
		Success:      success,
		ErrorCode:    strPtrOrNil(errorCode),
		ErrorMessage: strPtrOrNil(errorMessage),
	})
}

func truncateForLog(value string) string {
	if len(value) <= maxTaskResponseLogBytes {
		return value
	}
	return value[:maxTaskResponseLogBytes] + "... [truncated]"
}

// Keeping for reference during migration.

// logOperation is a convenience wrapper for repository.InsertOperationLog.
func logOperation(ctx context.Context, q pg.Querier, botID *int64, action string,
	targetType, targetID *string, requestData map[string]interface{}, success bool) {
	repository.InsertOperationLog(ctx, q, repository.LogInsertData{
		BotID:       botID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		RequestData: requestData,
		Success:     success,
	})
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
