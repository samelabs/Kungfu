package service

import (
	"context"
	"crypto/subtle"
	"regexp"
	"strings"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// Identity services mirror PHP:
//   - OwnerSessionService.php (login / current / logout)
//   - AccountService.php (overview)
//   - KeyService.php (current key)
//   - ChangePasswordService.php
//   - ResetKeyService.php

// -- OwnerSession --

// OwnerSessionResult mirrors PHP IdentityPresenter::ownerSession.
type OwnerSessionResult struct {
	BotID   int64  `json:"bot_id"`
	BotName string `json:"bot_name"`
	Status  string `json:"status"`
}

// OwnerLogin authenticates an owner by name+password.
// PHP: OwnerSessionService::login → OwnerSession::login
func OwnerLogin(ctx context.Context, pool *pg.Pool, name, password string) (*OwnerSessionResult, error) {
	name = strings.TrimSpace(name)

	// Validate name
	if valid, errs := auth.ValidateBotName(name); !valid {
		return nil, errors.New(400, "INVALID_NAME", errs[0])
	}

	// Validate password
	if valid, errs := auth.ValidatePassword(password); !valid {
		return nil, errors.New(400, "INVALID_PASSWORD", errs[0])
	}

	// Reject API keys in credentials
	if security.ContainsAPIKey([]interface{}{name, password}) {
		return nil, errors.New(400, "SENSITIVE_CONTENT", "human credentials must not contain API keys")
	}

	// Find bot by name
	bot, err := repository.FindActiveBotCredentialsByName(ctx, pool, name)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error during login")
	}
	if bot == nil || bot.PasswordHash == "" || !auth.VerifyPassword(password, bot.PasswordHash) {
		return nil, errors.New(401, "INVALID_CREDENTIALS", "Bot name or password is incorrect")
	}

	// In the PHP version, a session cookie is set here.
	// In Go, session management is handled at the HTTP layer (JWT/cookie middleware).
	// The service layer returns the identity; the handler issues the session token.

	logOperation(ctx, pool, &bot.ID, "owner_login", nil, nil,
		map[string]interface{}{"bot_name": bot.BotName}, true)

	return &OwnerSessionResult{
		BotID:   bot.ID,
		BotName: bot.BotName,
		Status:  bot.Status,
	}, nil
}

// OwnerCurrent returns the current owner session for a given botID.
// PHP: OwnerSessionService::current → OwnerSession::current
// In Go, the botID is resolved by the session middleware before calling this.
func OwnerCurrent(ctx context.Context, pool *pg.Pool, botID int64) (*OwnerSessionResult, error) {
	bot, err := repository.FindOwnerSessionBotByID(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving session")
	}
	if bot == nil {
		return nil, errors.New(401, "OWNER_LOGIN_REQUIRED", "Owner login required")
	}
	return &OwnerSessionResult{
		BotID:   bot.ID,
		BotName: bot.BotName,
		Status:  bot.Status,
	}, nil
}

// OwnerLogout invalidates the owner session.
// PHP: OwnerSessionService::logout
// In Go, session invalidation is handled at the HTTP layer (clear cookie/token).
// This is a no-op placeholder for service-layer symmetry.
func OwnerLogout(ctx context.Context, pool *pg.Pool, botID int64) map[string]interface{} {
	if botID > 0 {
		logOperation(ctx, pool, &botID, "owner_logout", nil, nil, nil, true)
	}
	return map[string]interface{}{}
}

// -- Account --

// AccountOverview mirrors PHP AccountService::overview.
// Returns the owner's account summary with kungfu/task stats.
func AccountOverview(ctx context.Context, pool *pg.Pool, botID int64) (map[string]interface{}, error) {
	bot, err := repository.FindActiveBotAccountByID(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving account")
	}
	if bot == nil {
		return nil, errors.New(404, "NOT_FOUND", "Bot not found")
	}

	stats, _ := repository.KungfuStatsByBotID(ctx, pool, botID)
	platformTaskCount, _ := repository.PlatformTaskCountByBotID(ctx, pool, botID)

	return map[string]interface{}{
		"bot_id":   botID,
		"bot_name": bot.BotName,
		"status":   bot.Status,
		"balance":  bot.Balance,
		"stats": map[string]interface{}{
			"kungfu_count":         stats.Total,
			"public_kungfu_count":  stats.PublicTotal,
			"platform_task_count":  platformTaskCount,
		},
	}, nil
}

// -- Key --

// CurrentOwnerKey mirrors PHP KeyService::currentOwnerKey.
// Returns the owner's current API key.
func CurrentOwnerKey(ctx context.Context, pool *pg.Pool, botID int64) (map[string]interface{}, error) {
	bot, err := repository.FindActiveBotKeyByID(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving key")
	}
	if bot == nil {
		return nil, errors.New(401, "OWNER_LOGIN_REQUIRED", "Owner login required")
	}

	logOperation(ctx, pool, &botID, "key_get", nil, nil,
		map[string]interface{}{
			"bot_name": bot.BotName,
			"source":   "owner_session",
		}, true)

	return map[string]interface{}{
		"bot_name":      bot.BotName,
		"key":           bot.APIKey,
		"balance":       bot.Balance,
		"status":        bot.Status,
		"key_issued_at": bot.KeyIssuedAt,
	}, nil
}

// -- ChangePassword --

// ChangePassword mirrors PHP ChangePasswordService::change.
// Requires name + current password + new password.
func ChangePassword(ctx context.Context, pool *pg.Pool, name, password, newPassword string) (map[string]interface{}, error) {
	name = strings.TrimSpace(name)

	// Validate name
	if valid, errs := auth.ValidateBotName(name); !valid {
		return nil, errors.New(400, "INVALID_NAME", errs[0])
	}

	// Validate current password
	if valid, errs := auth.ValidatePassword(password); !valid {
		return nil, errors.New(400, "INVALID_PASSWORD", errs[0])
	}

	// Reject API keys in content
	if security.ContainsAPIKey([]interface{}{name, password, newPassword}) {
		return nil, errors.New(400, "SENSITIVE_CONTENT", "human credentials must not contain API keys")
	}

	// Validate new password
	if valid, errs := auth.ValidatePassword(newPassword); !valid {
		return nil, errors.New(400, "INVALID_PASSWORD", errs[0])
	}

	// New must differ from old
	if subtle.ConstantTimeCompare([]byte(password), []byte(newPassword)) == 1 {
		return nil, errors.New(400, "PASSWORD_UNCHANGED", "New password must be different from current password")
	}

	// Verify credentials
	bot, err := repository.FindActiveBotCredentialsByName(ctx, pool, name)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error changing password")
	}
	if bot == nil || bot.PasswordHash == "" || !auth.VerifyPassword(password, bot.PasswordHash) {
		return nil, errors.New(401, "INVALID_CREDENTIALS", "Bot name or password is incorrect")
	}

	// Hash new password
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error hashing password")
	}

	if err := repository.UpdatePasswordHashByID(ctx, pool, bot.ID, newHash); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating password")
	}

	logOperation(ctx, pool, &bot.ID, "change_password", nil, nil,
		map[string]interface{}{"bot_name": bot.BotName}, true)

	return map[string]interface{}{
		"bot_name": bot.BotName,
		"message":  "Password changed. Current agent key remains valid until reset-key is called.",
	}, nil
}

// -- ResetKey --

var apiKeyFormatRegex = regexp.MustCompile(`(?i)^kf_live_[a-f0-9]{64}$`)

// ResetKey mirrors PHP ResetKeyService::reset.
// Requires the current key to match + rate limit check.
func ResetKey(ctx context.Context, pool *pg.Pool, limiter *ratelimit.Limiter, botID int64, currentKey string) (map[string]interface{}, error) {
	bot, err := repository.FindActiveBotKeyByID(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error resetting key")
	}
	if bot == nil {
		return nil, errors.New(401, "OWNER_LOGIN_REQUIRED", "Owner login required")
	}

	currentKey = strings.TrimSpace(currentKey)
	if currentKey == "" {
		return nil, errors.New(400, "MISSING_FIELD", "Missing required field: current_key")
	}

	// Validate key format
	if !apiKeyFormatRegex.MatchString(currentKey) {
		return nil, errors.New(400, "INVALID_KEY", "Current key format is invalid")
	}

	// Verify current key matches (constant-time)
	if subtle.ConstantTimeCompare([]byte(bot.APIKey), []byte(currentKey)) != 1 {
		return nil, errors.New(401, "INVALID_KEY", "Current key is incorrect")
	}

	// Rate limit check
	if limiter != nil && !limiter.CheckAPI(botID, "reset_key") {
		details := limiter.CheckAPIWithDetails(botID, "reset_key")
		return nil, errors.NewRateLimitError(details.RetryAfter, details.Limit, details.Window)
	}

	// Generate new key
	newKey := auth.GenerateKey()

	if err := repository.UpdateAPIKeyByID(ctx, pool, botID, newKey); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating key")
	}

	logOperation(ctx, pool, &botID, "reset_key", nil, nil,
		map[string]interface{}{"new_key_masked": security.MaskKey(newKey)}, true)

	return map[string]interface{}{
		"bot_name": bot.BotName,
		"new_key":  newKey,
		"message":  "Key has been reset. Old agent key is immediately invalid.",
		"warning":  "Give only the new key to agents. Never put it in URLs or business content.",
	}, nil
}
