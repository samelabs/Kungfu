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
	keyHexPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	apiKeyDetect  = regexp.MustCompile(`(?i)kf_live_[a-f0-9]{64}`)
)

// BotLookupFunc is a function that finds a bot by API key.
type BotLookupFunc func(ctx context.Context, key string) (*model.Bot, error)

// BotActiveUpdateFunc is a function that updates last_active_at.
type BotActiveUpdateFunc func(ctx context.Context, botID int64) error

// GenerateKey creates a new API key: kf_live_ + 64 hex chars.
// PHP: Auth::generateKey()
func GenerateKey() string {
	b := make([]byte, 32) // 32 bytes = 64 hex chars
	_, _ = cryptorand.Read(b)
	return keyPrefix + hex.EncodeToString(b)
}

// ValidateKeyFormat checks if a key matches the exact format.
// PHP: AuthValidator::validateKeyFormat
// - Must be exactly 72 characters
// - Must start with kf_live_
// - Remaining 64 chars must be hex (case-insensitive, like PHP ctype_xdigit)
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
// PHP: Auth::getKeyFromRequest (checks HTTP_X_BOT_KEY, X_BOT_KEY, getallheaders)
// In Go under nginx, r.Header.Get("X-Bot-Key") covers all cases.
func ExtractAPIKeyFromHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Bot-Key"))
}

// VerifyBotAuth authenticates a request via X-Bot-Key header.
// Returns the Bot if valid, or an AppError if not.
// PHP: Auth::requireAuth()
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

// MaybeUpdateLastActive updates last_active_at with 10% probability (sampling).
// PHP: Auth::updateLastActiveAsync uses register_shutdown_function + mt_rand(1,10)
// In Go, we fire-and-forget a goroutine.
func MaybeUpdateLastActive(ctx context.Context, updateFn BotActiveUpdateFunc, botID int64) {
	// 10% sampling - matches PHP mt_rand(1, 10) === 1
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

// HashPassword creates a bcrypt password hash.
// Compatible with PHP password_hash(PASSWORD_DEFAULT).
// PHP uses $2y$ prefix; Go uses $2a$/$2b$. Both are valid bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword checks a password against a hash.
// PHP: password_verify(). Handles $2y$, $2a$, $2b$ prefixes.
func VerifyPassword(password, hash string) bool {
	// PHP produces $2y$ hashes; Go's bcrypt can verify them after prefix normalization
	normalizedHash := normalizeBcryptPrefix(hash)
	return bcrypt.CompareHashAndPassword([]byte(normalizedHash), []byte(password)) == nil
}

// normalizeBcryptPrefix converts PHP's $2y$ to Go-compatible $2a$/$2b$ if needed.
func normalizeBcryptPrefix(hash string) string {
	if len(hash) >= 4 && hash[:4] == "$2y$" {
		return "$2a$" + hash[4:]
	}
	return hash
}

// ValidatePassword validates a password against the rules.
// PHP: AuthValidator::validatePassword
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
// PHP: AuthValidator::validateBotName
// - 6-32 chars (PHP code uses strlen, min is 6 not 3 despite docs saying 3)
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
	// PHP regex: /^[a-zA-Z0-9_.-]+$/
	validChars := regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	if !validChars.MatchString(name) {
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
