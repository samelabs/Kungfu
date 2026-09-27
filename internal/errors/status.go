package errors

import "net/http"

// statusByCode is the ONE protocol-layer code→HTTP status table
// (Task 1.0 spec §8.4 plus the account and storage tool codes the
// unified registry can emit). Every wire writer — /api/v1, the MCP
// registry, the owner/REST response helpers — resolves codes through
// StatusFor first; codes outside the table fall back to the
// AppError's own HTTPCode, and INTERNAL_ERROR is the last resort.
var statusByCode = map[string]int{
	// §8.4 executor + publisher task catalogue
	"UNAUTHORIZED":          http.StatusUnauthorized,
	"RATE_LIMIT":            http.StatusTooManyRequests, // the only 429
	"TASK_NOT_FOUND":        http.StatusNotFound,
	"SUBMISSION_NOT_FOUND":  http.StatusNotFound,
	"HARNESS_REF_NOT_FOUND": http.StatusNotFound,
	"UNKNOWN_TOOL":          http.StatusNotFound,
	"OWN_TASK":              http.StatusForbidden,
	"NOT_OWNER":             http.StatusForbidden,
	"INSUFFICIENT_CREDITS":  http.StatusPaymentRequired,
	"PAYLOAD_TOO_LARGE":     http.StatusRequestEntityTooLarge,
	"TASK_NOT_OPEN":         http.StatusConflict,
	"SLOTS_EXHAUSTED":       http.StatusConflict,
	"SUBMISSION_LIMIT":      http.StatusConflict,
	"CLAIM_REQUIRED":        http.StatusConflict,
	"CLAIM_INVALID":         http.StatusConflict,
	"IDEMPOTENCY_CONFLICT":  http.StatusConflict,
	"INVALID_STATE":         http.StatusConflict,
	"HAS_RESERVATIONS":      http.StatusConflict,
	"NOT_UNDER_REVIEW":      http.StatusConflict,
	"NOTHING_TO_REFUND":     http.StatusConflict,
	"SCHEMA_MISMATCH":       http.StatusUnprocessableEntity,
	"CREDENTIAL_IN_PAYLOAD": http.StatusUnprocessableEntity,
	"INVALID_REVISES":       http.StatusUnprocessableEntity,
	"INVALID_REQUEST_KEY":   http.StatusUnprocessableEntity,
	"VALIDATION_FAILED":     http.StatusUnprocessableEntity,
	"VERDICT_INVALID":       http.StatusUnprocessableEntity,
	"TEST_DELIVERY_FAILED":  http.StatusUnprocessableEntity,

	// account and storage tools (§8.4「账户与存储工具错误」)
	"NAME_TAKEN":           http.StatusConflict,
	"INVALID_NAME":         http.StatusUnprocessableEntity,
	"INVALID_PASSWORD":     http.StatusUnprocessableEntity,
	"INVALID_CREDENTIALS":  http.StatusUnauthorized,
	"RESERVED_NAME":        http.StatusUnprocessableEntity,
	"INVALID_CODE":         http.StatusUnprocessableEntity,
	"NOT_FOUND":            http.StatusNotFound,
	"PRIVATE_KUNGFU":       http.StatusForbidden,
	"CONTENT_TOO_LARGE":    http.StatusRequestEntityTooLarge,
	"CONTENT_TOO_SHORT":    http.StatusUnprocessableEntity,
	"TITLE_TOO_LONG":       http.StatusUnprocessableEntity,
	"DESCRIPTION_TOO_LONG": http.StatusUnprocessableEntity,
	"TOO_MANY_TAGS":        http.StatusUnprocessableEntity,
	"TAG_TOO_LONG":         http.StatusUnprocessableEntity,
	"INVALID_TAGS":         http.StatusUnprocessableEntity,
	"INVALID_TYPE":         http.StatusUnprocessableEntity,
	"SENSITIVE_CONTENT":    http.StatusUnprocessableEntity,
	"MISSING_FIELD":        http.StatusBadRequest,
	"INVALID_REQUEST":      http.StatusBadRequest,
	"OWNER_LOGIN_REQUIRED": http.StatusUnauthorized,
	"ADMIN_LOGIN_REQUIRED": http.StatusUnauthorized,
	"PASSWORD_UNCHANGED":   http.StatusUnprocessableEntity,
}

// StatusFor resolves a wire code to its HTTP status. The bool reports
// whether the code is in the table; callers fall back to the
// AppError's own HTTPCode when false.
func StatusFor(code string) (int, bool) {
	if status, ok := statusByCode[code]; ok {
		return status, true
	}
	return 0, false
}
