package repository

import (
	"context"
	"encoding/json"
	"kungfu.md/internal/pg"

	"kungfu.md/internal/security"
)

// LogInsertData holds parameters for inserting an operation log entry.
type LogInsertData struct {
	BotID       *int64
	Action      string
	TargetType  *string
	TargetID    *string
	IPAddress   *string
	UserAgent   *string
	Success     bool
	ErrorCode   *string
	ErrorMsg    *string
	RequestData map[string]interface{}
}

// InsertOperationLog inserts an operation log entry into tb_logs.
// It masks sensitive data (API keys) and truncates long fields to 1000 chars.
func InsertOperationLog(ctx context.Context, q pg.Querier, data LogInsertData) {
	var requestDataJSON *string
	if data.RequestData != nil {
		masked := maskSensitiveData(data.RequestData)
		encoded, err := json.Marshal(masked)
		if err == nil {
			s := string(encoded)
			requestDataJSON = &s
		}
	}

	_, _ = q.Exec(ctx, `
		INSERT INTO tb_logs (bot_id, action, target_type, target_id, ip_address, user_agent, request_data, success, error_code, error_msg, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())`,
		data.BotID, data.Action, data.TargetType, data.TargetID,
		data.IPAddress, data.UserAgent, requestDataJSON,
		data.Success, data.ErrorCode, data.ErrorMsg)
}

// maskSensitiveData masks API keys and truncates long fields.
func maskSensitiveData(data map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for key, value := range data {
		switch v := value.(type) {
		case string:
			// Check for API key pattern
			if security.ContainsAPIKey(v) {
				result[key] = security.RedactSecrets(v)
			} else if len(v) > 1000 {
				// Truncate long content fields
				result[key] = v[:1000] + "... [truncated]"
			} else {
				result[key] = v
			}
		case map[string]interface{}:
			result[key] = maskSensitiveData(v)
		case []interface{}:
			masked := make([]interface{}, len(v))
			for i, item := range v {
				if s, ok := item.(string); ok {
					if security.ContainsAPIKey(s) {
						masked[i] = security.RedactSecrets(s)
					} else if len(s) > 1000 {
						masked[i] = s[:1000] + "... [truncated]"
					} else {
						masked[i] = s
					}
				} else {
					masked[i] = item
				}
			}
			result[key] = masked
		default:
			result[key] = v
		}
	}
	return result
}

// MaskKey wraps security.MaskKey for logger compatibility.
func MaskKey(key string) string {
	return security.MaskKey(key)
}

// FileLog writes a line to the file-based log.
// This is a fire-and-forget operation that must not affect the main flow.
