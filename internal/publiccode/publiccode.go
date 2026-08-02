package publiccode

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const Length = 12

var codePattern = regexp.MustCompile(`^[a-f0-9]{12}$`)

// Require validates a code parameter. Returns error if invalid.
func Require(value *string, field string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("MISSING_FIELD: Missing URL parameter: %s", field)
	}
	v := *value
	normalized := toLower(trim(v))
	if normalized == "" {
		return "", fmt.Errorf("MISSING_FIELD: Missing URL parameter: %s", field)
	}
	if !codePattern.MatchString(normalized) {
		return "", fmt.Errorf("INVALID_CODE: Invalid code format")
	}
	return normalized, nil
}

// Generate creates a random 12-hex-char code.
func Generate() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateUnique generates a code that doesn't exist in the given table.
// The checkFunc should query the database to see if the code exists.
func GenerateUnique(checkFunc func(string) (bool, error)) (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		code := Generate()
		exists, err := checkFunc(code)
		if err != nil {
			return "", err
		}
		if !exists {
			return code, nil
		}
	}
	return "", fmt.Errorf("CODE_GENERATION_FAILED: Could not generate a unique code")
}

func toLower(s string) string { return strings.ToLower(s) }
func trim(s string) string    { return strings.TrimSpace(s) }
