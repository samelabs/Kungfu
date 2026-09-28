package security

import (
	"fmt"
	"regexp"
)

// APIKeyPattern matches kf_live_ followed by 64 hex chars (case-insensitive).
var apiKeyPattern = regexp.MustCompile(`(?i)kf_live_[a-f0-9]{64}`)

// credentialPatterns are the DISTINCTIVE credential shapes rejected
// wherever user content flows (task contracts, payloads, memories).
// Only shapes with an unambiguous prefix/structure are listed — loose
// guesses would reject ordinary prose. Length floors and exact
// prefixes keep near-misses (test keys, short look-alikes, ordinary
// words) out.
var credentialPatterns = []*regexp.Regexp{
	// Kungfu Agent keys — the platform's own secrets.
	apiKeyPattern,
	// AWS access key IDs: AKIA/ASIA + exactly 16 upper-case alnum.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// PEM private key headers (RSA / EC / OPENSSH / DSA / bare).
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`),
	// GitHub tokens: ghp_/gho_/ghu_/ghs_/ghr_ + 36+ alnum.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
	// Slack tokens: xox[abprs]- + 10+ token chars.
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),
	// OpenAI-style keys: sk- + 32+ alnum (no separators).
	regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`),
	// Anthropic keys: sk-ant- + 16+ key chars.
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`),
	// Stripe live secret / restricted keys (test keys stay allowed).
	regexp.MustCompile(`(?:sk|rk)_live_[A-Za-z0-9]{16,}`),
}

// ContainsCredential reports whether s contains any credential-shaped
// string: a Kungfu Agent key or one of the common provider token
// formats above. This is the one detector shared by contract
// validation, payload scanning and memory intake.
func ContainsCredential(s string) bool {
	for _, re := range credentialPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// ContainsCredentialValue walks a decoded JSON value (maps, slices,
// strings) and reports whether any string in it is credential-shaped.
func ContainsCredentialValue(v interface{}) bool {
	switch val := v.(type) {
	case map[string]interface{}:
		for _, item := range val {
			if ContainsCredentialValue(item) {
				return true
			}
		}
		return false
	case []interface{}:
		for _, item := range val {
			if ContainsCredentialValue(item) {
				return true
			}
		}
		return false
	case string:
		return ContainsCredential(val)
	default:
		return false
	}
}

// RejectCredentialInContent returns an error if the value contains any
// credential-shaped string. The error uses the machine code
// "SENSITIVE_CONTENT".
func RejectCredentialInContent(v interface{}, field string) error {
	if ContainsCredentialValue(v) {
		return fmt.Errorf("SENSITIVE_CONTENT: %s must not contain credential-shaped strings", field)
	}
	return nil
}

// ContainsAPIKey checks if a value (recursively for maps/slices) contains an API key pattern.
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
// The error uses the machine code "SENSITIVE_CONTENT".
func RejectAPIKeyInContent(v interface{}, field string) error {
	if ContainsAPIKey(v) {
		return fmt.Errorf("SENSITIVE_CONTENT: %s must not contain API keys", field)
	}
	return nil
}

// RedactSecrets recursively replaces all API key patterns in strings with masked versions.
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

// MaskKey masks an API key, showing the first 8 and last 4 chars, e.g. "kf_live_****1a2b".
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
