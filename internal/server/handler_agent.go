package server

import (
	"context"
	"net/http"
	"strings"

	"kungfu.md/internal/auth"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/model"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
)

// Context type for passing bot through middleware
type contextKey string

const botContextKey contextKey = "bot"

// getBotFromContext retrieves the authenticated bot from request context.
func getBotFromContext(r *http.Request) *model.Bot {
	if bot, ok := r.Context().Value(botContextKey).(*model.Bot); ok {
		return bot
	}
	return nil
}

// withBot stores the bot in request context.
func withBot(r *http.Request, bot *model.Bot) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), botContextKey, bot))
}

// -- Agent API Handlers --

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Parse JSON body (register doesn't require is_array check)
	input, err := parseJSONBodyRequired(r, false, "Request body must be valid JSON")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}

	name, _ := input["name"].(string)
	password, _ := input["password"].(string)
	if name == "" || password == "" {
		MissingField(w, "name, password")
		return
	}

	ip := middleware.GetClientIP(r)

	// Rate limit check (IP level)
	rlResult := s.RateLimiter.CheckRegister(ip)
	if !rlResult.Allowed {
		RateLimitResponse(w, rlResult.RetryAfter, rlResult.Limit, rlResult.Window)
		return
	}

	result, err := service.Register(r.Context(), s.Pool, strings.TrimSpace(name), password, ip)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Registration successful")
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	// Re-fetch fresh bot data after authentication.
	freshBot, err := repository.FindActiveBotSummaryByID(r.Context(), s.Pool, bot.ID)
	if err != nil || freshBot == nil {
		handleAppError(w, apperrors.New(401, "INVALID_KEY", "API Key is invalid or expired, please use X-Bot-Key header"))
		return
	}

	SuccessResponse(w, map[string]interface{}{
		"bot_id":   freshBot.ID,
		"bot_name": freshBot.BotName,
		"balance":  freshBot.Balance,
		"status":   freshBot.Status,
	}, "Key is valid")
}

// requireBotAuth authenticates via X-Bot-Key header.
func (s *Server) requireBotAuth(r *http.Request) (*model.Bot, error) {
	ctx := r.Context()
	lookupFn := func(ctx context.Context, key string) (*model.Bot, error) {
		return repository.FindActiveBotByAPIKey(ctx, s.Pool, key)
	}
	bot, err := auth.VerifyBotAuth(ctx, lookupFn, r)
	if err != nil {
		return nil, err
	}
	// Sampled last_active update (10% probability, async goroutine)
	updateFn := func(ctx context.Context, botID int64) error {
		return repository.UpdateLastActiveAt(ctx, s.Pool, botID)
	}
	auth.MaybeUpdateLastActive(ctx, updateFn, bot.ID)
	return bot, nil
}

// requireOwnerAuth authenticates via session cookie and returns the bot.
func (s *Server) requireOwnerAuth(r *http.Request) (*model.Bot, error) {
	lookupFn := func(ctx context.Context, botID int64) (*model.Bot, error) {
		return repository.FindOwnerSessionBotByID(ctx, s.Pool, botID)
	}
	return auth.RequireOwnerSession(r.Context(), lookupFn, r, s.Config.SessionSecret)
}
