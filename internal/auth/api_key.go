package auth

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
	apperr "kungfu.md/internal/errors"
	"kungfu.md/internal/model"
)

// API key format: kf_live_ + 64 hex chars = 72 chars total
const (
	keyPrefix   = "kf_live_"
	keyTotalLen = 72
	keyHexLen   = 64
)

var (
	keyHexPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	apiKeyDetect       = regexp.MustCompile(`(?i)kf_live_[a-f0-9]{64}`)
	botNameCharPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// BotLookupFunc is a function that finds a bot by API key.
type BotLookupFunc func(ctx context.Context, key string) (*model.Bot, error)

// BotActiveUpdateFunc is a function that updates last_active_at.
type BotActiveUpdateFunc func(ctx context.Context, botID int64) error

func GenerateKey() string {
	b := make([]byte, 32) // 32 bytes = 64 hex chars
	if _, err := cryptorand.Read(b); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	return keyPrefix + hex.EncodeToString(b)
}

// ValidateKeyFormat checks if a key matches the exact format.
// - Must be exactly 72 characters
// - Must start with kf_live_
// - Remaining 64 chars must be hex (case-insensitive)
func ValidateKeyFormat(key string) bool {
	if len(key) != keyTotalLen {
		return false
	}
	if !strings.HasPrefix(key, keyPrefix) {
		return false
	}
	hexPart := key[len(keyPrefix):]
	return keyHexPattern.MatchString(strings.ToLower(hexPart))
}

// ExtractAPIKeyFromHeader gets the X-Bot-Key header value from an HTTP request.
func ExtractAPIKeyFromHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Bot-Key"))
}

// VerifyBotAuth authenticates a request via X-Bot-Key header.
// Returns the Bot if valid, or an AppError if not.
func VerifyBotAuth(ctx context.Context, lookupFn BotLookupFunc, r *http.Request) (*model.Bot, error) {
	key := ExtractAPIKeyFromHeader(r)
	if key == "" {
		return nil, apperr.New(401, "INVALID_KEY", "API Key is invalid or expired, please use X-Bot-Key header")
	}

	if !ValidateKeyFormat(key) {
		return nil, apperr.New(401, "INVALID_KEY", "API Key is invalid or expired, please use X-Bot-Key header")
	}

	bot, err := lookupFn(ctx, key)
	if err != nil || bot == nil {
		return nil, apperr.New(401, "INVALID_KEY", "API Key is invalid or expired, please use X-Bot-Key header")
	}

	return bot, nil
}

// MaybeUpdateLastActive updates last_active_at with 10% probability (sampling)
// to reduce DB write load. The update runs in a fire-and-forget goroutine.
func MaybeUpdateLastActive(ctx context.Context, updateFn BotActiveUpdateFunc, botID int64) {
	// 10% sampling to reduce DB writes
	if rand.IntN(10) != 0 {
		return
	}
	if updateFn == nil {
		return
	}
	// Fire-and-forget, non-blocking
	go func() {
		bgCtx := context.Background()
		_ = updateFn(bgCtx, botID)
	}()
}

// HashPassword creates a bcrypt password hash using bcrypt.DefaultCost.
// Hashes are interoperable with hashes created with the $2y$ prefix convention
// (see VerifyPassword / normalizeBcryptPrefix).
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword checks a password against a hash. Accepts $2y$, $2a$, and $2b$
// bcrypt prefixes by normalizing $2y$ to $2a$ before comparison (legacy hashes in
// the DB may use the $2y$ prefix).
func VerifyPassword(password, hash string) bool {
	normalizedHash := normalizeBcryptPrefix(hash)
	return bcrypt.CompareHashAndPassword([]byte(normalizedHash), []byte(password)) == nil
}

// normalizeBcryptPrefix converts a $2y$ bcrypt prefix to $2a$ so Go's bcrypt can
// verify hashes that use the alternate (but algorithmically identical) prefix.
func normalizeBcryptPrefix(hash string) string {
	if len(hash) >= 4 && hash[:4] == "$2y$" {
		return "$2a$" + hash[4:]
	}
	return hash
}

// ValidatePassword validates a password against the rules.
// - 6-128 bytes
// - Must not contain API key pattern
func ValidatePassword(password string) (bool, []string) {
	var errs []string
	pLen := len(password)

	if pLen < 6 {
		errs = append(errs, "Password too short (minimum 6 characters)")
	}
	if pLen > 128 {
		errs = append(errs, "Password too long (maximum 128 characters)")
	}
	if apiKeyDetect.MatchString(password) {
		errs = append(errs, "Password must not contain an API key")
	}

	return len(errs) == 0, errs
}

// ValidateBotName validates a bot name against the rules.
// - 6-32 chars (minimum is 6, not the 3 stated in some older docs)
// - Only [a-zA-Z0-9_.-]
// - Not a reserved word (case-insensitive)
func ValidateBotName(name string) (bool, []string) {
	var errs []string
	nLen := len(name)

	if nLen < 6 {
		errs = append(errs, "Name too short (minimum 6 characters)")
	}
	if nLen > 32 {
		errs = append(errs, "Name too long (maximum 32 characters)")
	}
	if !botNameCharPattern.MatchString(name) {
		errs = append(errs, "Name contains invalid characters (only letters, numbers, _, ., - allowed)")
	}

	// Reserved words (lowercase comparison)
	reserved := map[string]bool{
		"admin": true, "root": true, "system": true, "api": true, "web": true,
	}
	if reserved[strings.ToLower(name)] {
		errs = append(errs, "Name is a system reserved word")
	}

	return len(errs) == 0, errs
}

// ContextKey is used for storing bot in request context.
type ContextKey string

const BotContextKey ContextKey = "bot"
