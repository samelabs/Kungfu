package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/version"
)

// apiVersion returns the current application version.
func apiVersion() string {
	return version.Get()
}

// ErrorSuggestion maps error codes to suggestion text.
var errorSuggestions = map[string]string{
	"NAME_TAKEN":           "Try using a different name, such as adding a version number to the original name",
	"ALREADY_REGISTERED":   "This bot name is taken, try a different name",
	"INVALID_NAME":         "Name must be 3-32 characters, only letters, numbers, underscores, hyphens, and dots allowed",
	"RESERVED_NAME":        "System reserved names cannot be used, please choose another meaningful name",
	"INVALID_KEY":          "Please check if the X-Bot-Key header is correct",
	"INSUFFICIENT_CREDITS": "Complete platform tasks to earn credits, then retry this action",
	"PRIVATE_KUNGFU":       "This is a private ability, only the creator can access it. Try searching for other public abilities",
	"RATE_LIMIT":           "Please wait for the specified time before retrying. Implement exponential backoff strategy",
	"CONTENT_TOO_LARGE":    "Content exceeds 100KB limit. Please split into smaller abilities or compress the code",
}

// SuccessResponse sends a success JSON response.
// Format: {success:true, data:..., message:..., timestamp:..., api_version:"v1.2.0"}
func SuccessResponse(w http.ResponseWriter, data interface{}, message string) {
	resp := map[string]interface{}{
		"success":     true,
		"data":        data,
		"message":     message,
		"timestamp":   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"api_version": apiVersion(),
	}
	sendJSON(w, resp, 200)
}

// ErrorResponse sends an error JSON response.
// Format: {success:false, error:{code, message, documentation, suggestion?, details?}, timestamp, request_id}
func ErrorResponse(w http.ResponseWriter, httpCode int, errorCode, message string, details map[string]interface{}) {
	errObj := map[string]interface{}{
		"code":          errorCode,
		"message":       message,
		"documentation": "https://kungfu.md/llms.txt",
	}

	if suggestion, ok := errorSuggestions[errorCode]; ok {
		errObj["suggestion"] = suggestion
	}
	if len(details) > 0 {
		errObj["details"] = details
	}

	resp := map[string]interface{}{
		"success":    false,
		"error":      errObj,
		"timestamp":  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"request_id": generateRequestID(),
	}
	sendJSON(w, resp, httpCode)
}

// RateLimitResponse sends a 429 with rate limit headers.
func RateLimitResponse(w http.ResponseWriter, retryAfter, limit, window int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(time.Now().Unix())+retryAfter))

	ErrorResponse(w, 429, "RATE_LIMIT", "Too many requests", map[string]interface{}{
		"retry_after": retryAfter,
		"limit":       limit,
		"window":      window,
	})
}

// HandleAppError sends appropriate error response for an AppError.
func HandleAppError(w http.ResponseWriter, err error) {
	if ae, ok := apperrors.IsAppError(err); ok {
		ErrorResponse(w, ae.HTTPCode, ae.Code, ae.Message, ae.Details)
		return
	}
	if rle, ok := apperrors.IsRateLimitError(err); ok {
		RateLimitResponse(w, rle.RetryAfter, rle.Limit, rle.Window)
		return
	}
	// Generic error
	ErrorResponse(w, 500, "INTERNAL_ERROR", "An internal error occurred", nil)
}

// sendJSON writes JSON with pretty-printed indentation and unicode preserved.
func sendJSON(w http.ResponseWriter, data interface{}, httpCode int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpCode)
	// MarshalIndent pretty-prints; Go's json does not escape unicode by default.
	encoded, _ := json.MarshalIndent(data, "", "    ")
	w.Write(encoded)
}

// generateRequestID creates a req_ prefixed hex ID.
func generateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// MethodNotAllowed sends a 405 error.
func MethodNotAllowed(w http.ResponseWriter) {
	ErrorResponse(w, 405, "METHOD_NOT_ALLOWED", "Only allowed requests are permitted", nil)
}

// NotFound sends a 404 error.
func NotFound(w http.ResponseWriter) {
	ErrorResponse(w, 404, "NOT_FOUND", "Endpoint not found", nil)
}

// InvalidJSON sends a 400 error for invalid JSON.
// message can be "Request body must be valid JSON" or "Request body must be valid JSON object"
func InvalidJSON(w http.ResponseWriter, message string) {
	ErrorResponse(w, 400, "INVALID_JSON", message, nil)
}

// MissingField sends a 400 error for missing required field.
func MissingField(w http.ResponseWriter, field string) {
	ErrorResponse(w, 400, "MISSING_FIELD", "Missing required field: "+field, nil)
}
