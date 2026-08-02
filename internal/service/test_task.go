package service

import (
	"context"
	"encoding/json"
	"strings"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// TestTaskService mirrors PHP services/TestTaskService.php.
//
// Same flow as agent task submission but:
//   - Owner-only (checks task.bot_id == botID)
//   - No credit reward to the owner (Transaction::record is only for budget settlement)
//   - Returns response_body to the owner
//   - Different truncation limits

const (
	testMaxResponseBytes   = 16000 // MAX_RESPONSE_BYTES
	testDBResponseBodyMax  = 65535 // DB_RESPONSE_BODY_MAX
	testDBErrorMessageMax  = 256   // DB_ERROR_MESSAGE_MAX
	testDBPayloadJSONMax   = 60000 // DB_PAYLOAD_JSON_MAX
)

// TestTaskResult is the return value of TestTaskDeliver.
type TestTaskResult struct {
	TaskCode string                 `json:"task_code"`
	Post     map[string]interface{} `json:"post"`
	Billing  map[string]interface{} `json:"billing"`
}

// TestTaskDeliver lets an owner test their own task's postapi.
// PHP: TestTaskService::deliver(botId, code, input)
func TestTaskDeliver(ctx context.Context, pool *pg.Pool, botID int64, code string, input map[string]interface{}) (*TestTaskResult, error) {
	// 1. Find task and verify ownership
	task, err := repository.FindTaskByCode(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	if task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}
	if task.BotID != botID {
		return nil, errors.New(403, "NOT_OWNER", "Only the task owner can test this task")
	}

	postapi := ""
	if task.PostAPI != nil {
		postapi = strings.TrimSpace(*task.PostAPI)
	}
	price := task.Price

	// 2. TaskCheck
	checkErr := RunTaskCheck(postapi, price, func() *TaskCheckError {
		return testEnsureBudgetAvailable(ctx, pool, code, price)
	})

	if checkErr != nil {
		testLogEvent(ctx, pool, code, botID, "kfcheck", input, false, nil, nil,
			checkErr.Rule.Code, checkErr.Rule.LogMsg)
		return nil, checkErr.ToAppError()
	}

	// 3. POST to owner's API
	payload := delivery.BuildPayload(code, input)
	payloadBytes, _ := json.Marshal(payload)

	postResult := delivery.PostJSON(postapi, payloadBytes, delivery.TestTaskErrorConfig())

	if !postResult.Success {
		testLogEvent(ctx, pool, code, botID, "post_failed", payload, false,
			postResult.ResponseCode, postResult.ResponseBody,
			postResult.ErrorCode, postResult.ErrorMessage)

		return nil, errors.NewWithDetails(424,
			ifEmpty(postResult.ErrorCode, "TESTTASK_POST_FAILED"),
			ifEmpty(postResult.ErrorMessage, "Task test delivery failed"),
			map[string]interface{}{
				"post": map[string]interface{}{
					"delivered":      false,
					"response_code":  postResult.ResponseCode,
					"response_body":  testTruncateResponse(derefStr(postResult.ResponseBody)),
				},
			})
	}

	// 4. Log success
	testLogEvent(ctx, pool, code, botID, "post_succeeded", payload, true,
		postResult.ResponseCode, postResult.ResponseBody, "", "")

	// 5. Settle budget (no credit reward for test)
	billing := testSettleBudget(ctx, pool, code, price)

	return &TestTaskResult{
		TaskCode: code,
		Post: map[string]interface{}{
			"delivered":     true,
			"response_code": postResult.ResponseCode,
			"response_body": testTruncateResponse(derefStr(postResult.ResponseBody)),
		},
		Billing: billing,
	}, nil
}

// testEnsureBudgetAvailable checks task budget/status without locking.
// PHP: TestTaskService::ensureBudgetAvailable
func testEnsureBudgetAvailable(ctx context.Context, pool *pg.Pool, taskCode string, price float64) *TaskCheckError {
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

// testSettleBudget decrements the task budget without awarding credits.
// PHP: TestTaskService::settleBudget
// Returns billing map: {cost, budget, status}
func testSettleBudget(ctx context.Context, pool *pg.Pool, taskCode string, price float64) map[string]interface{} {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return map[string]interface{}{
			"cost":   price,
			"budget": 0,
			"status": "error",
		}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := repository.FindTaskBudgetStatusByCodeForUpdate(ctx, tx, taskCode)
	if err != nil || task == nil || task.Status != "open" {
		return map[string]interface{}{
			"cost":   price,
			"budget": 0,
			"status": "TASK_NOT_OPEN",
		}
	}

	if task.Budget < price || task.Budget < MinOpenBudget {
		return map[string]interface{}{
			"cost":   price,
			"budget": task.Budget,
			"status": "TASK_BUDGET_EXHAUSTED",
		}
	}

	nextBudget := task.Budget - price
	nextStatus := task.Status
	if nextBudget < MinOpenBudget {
		nextStatus = "closed"
	}

	_ = repository.UpdateTaskBudgetAndStatus(ctx, tx, task.ID, nextBudget, nextStatus, nextStatus == "closed")

	if err := tx.Commit(ctx); err != nil {
		return map[string]interface{}{
			"cost":   price,
			"budget": nextBudget,
			"status": "error",
		}
	}

	return map[string]interface{}{
		"cost":   price,
		"budget": nextBudget,
		"status": nextStatus,
	}
}

// testLogEvent writes a task delivery log entry with PHP-compliant truncation.
// PHP: TestTaskService::logEvent
func testLogEvent(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	action string, payload map[string]interface{}, success bool,
	responseCode *int, responseBody *string, errorCode, errorMessage string) {

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
				preview := truncateByBytes(s, previewLen)
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

	var respBodyForLog *string
	if responseBody != nil {
		truncated := truncateByBytes(*responseBody, testDBResponseBodyMax)
		respBodyForLog = &truncated
	}

	var errMsgForLog *string
	if errorMessage != "" {
		truncated := truncateByBytes(errorMessage, testDBErrorMessageMax)
		errMsgForLog = &truncated
	}

	_ = repository.InsertTaskLog(ctx, pool, repository.NewTaskLogInput{
		TaskCode:     taskCode,
		BotID:        &botID,
		Action:       action,
		PayloadJSON:  payloadJSON,
		ResponseCode: responseCode,
		ResponseBody: respBodyForLog,
		Success:      success,
		ErrorCode:    strPtrOrNil(errorCode),
		ErrorMessage: errMsgForLog,
	})
}

// testTruncateResponse truncates the response for API output.
// PHP: TestTaskService::truncateResponse
func testTruncateResponse(value string) string {
	if len(value) <= testMaxResponseBytes {
		return value
	}
	return value[:testMaxResponseBytes] + "... [truncated]"
}

// truncateByBytes truncates a string to at most maxBytes.
// PHP: TestTaskService::truncateByBytes
func truncateByBytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes]
}

// derefStr safely dereferences a *string.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
