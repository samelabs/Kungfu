package service

import (
	"context"
	"encoding/json"
	"log"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// TestTaskService: the owner test path now runs on the SAME durable
// submission mechanism as agent submissions — TestTaskDeliver (in
// task_submission.go) goes through acceptAndProcess with kind=owner_test:
// no earn_task, same accept/claim/outcome/settle authority, no second
// "HTTP -> budget commit" mechanism. This file keeps only the TestTask
// log-sink contract (payload/response budgets).

const (
	testMaxResponseBytes  = 16000 // MAX_RESPONSE_BYTES
	testDBResponseBodyMax = 65535 // DB_RESPONSE_BODY_MAX
	testDBErrorMessageMax = 256   // DB_ERROR_MESSAGE_MAX
	testDBPayloadJSONMax  = 60000 // DB_PAYLOAD_JSON_MAX
)

// testLogEvent writes a task delivery log entry with the TestTask-specific
// budgets. Log-sink hardening: persisted copies pass redaction BEFORE any
// truncation; the actual PostAPI payload is untouched.
func testLogEvent(ctx context.Context, pool *pg.Pool, taskCode string, botID int64,
	action string, payload map[string]interface{}, success bool,
	responseCode *int, responseBody *string, errorCode, errorMessage string) {

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

	// DB columns are marker-free budgets; the API preview paths keep their
	// "... [truncated]" marker via testTruncateResponse.
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
