package security

import (
	"fmt"
	"regexp"
)

// APIKeyPattern matches kf_live_ followed by 64 hex chars.
// PHP: '/kf_live_[a-f0-9]{64}/i' (case-insensitive)
var apiKeyPattern = regexp.MustCompile(`(?i)kf_live_[a-f0-9]{64}`)

// ContainsAPIKey checks if a value (recursively for maps/slices) contains an API key pattern.
// PHP: Security::containsApiKey
func ContainsAPIKey(v interface{}) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		for _, item := range val {
			if ContainsAPIKey(item) {
				return true
			}
		}
		return false
	case []interface{}:
		for _, item := range val {
			if ContainsAPIKey(item) {
				return true
			}
		}
		return false
	case string:
		return apiKeyPattern.MatchString(val)
	default:
		return false
	}
}

// RejectAPIKeyInContent returns an error if the value contains an API key.
// PHP: Security::rejectApiKeyInContent
// Returns an error with code "SENSITIVE_CONTENT" matching PHP behavior.
func RejectAPIKeyInContent(v interface{}, field string) error {
	if ContainsAPIKey(v) {
		return fmt.Errorf("SENSITIVE_CONTENT: %s must not contain API keys", field)
	}
	return nil
}

// RedactSecrets recursively replaces all API key patterns in strings with masked versions.
// PHP: Security::redactSecrets
func RedactSecrets(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, item := range val {
			result[k] = RedactSecrets(item)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, item := range val {
			result[i] = RedactSecrets(item)
		}
		return result
	case string:
		return apiKeyPattern.ReplaceAllStringFunc(val, func(match string) string {
			return MaskKey(match)
		})
	default:
		return v
	}
}

// MaskKey masks an API key, showing first 8 and last 4 chars.
// PHP: Security::maskKey
// "kf_live_****1a2b"
func MaskKey(key string) string {
	if len(key) <= 12 {
		result := ""
		for range key {
			result += "*"
		}
		return result
	}
	return key[:8] + "****" + key[len(key)-4:]
}
