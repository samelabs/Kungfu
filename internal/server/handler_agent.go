package server

import (
	"context"
	"net/http"
	"strings"

	"kungfu.md/internal/auth"
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

	ip := middleware.GetClientIP(r, s.TrustedProxies)

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

	// Single account-status authority: the same typed service the MCP
	// account_status tool calls. Output shape is unchanged.
	st, err := service.ComposeAgentAccountStatus(r.Context(), s.Pool, bot.ID)
	if err != nil {
		handleAppError(w, err)
		return
	}

	SuccessResponse(w, map[string]interface{}{
		"bot_id":   st.BotID,
		"bot_name": st.BotName,
		"balance":  st.Balance,
		"status":   st.Status,
	}, "Key is valid")
}

// requireBotAuth authenticates via X-Bot-Key header.
func (s *Server) requireBotAuth(r *http.Request) (*model.Bot, error) {
	ctx := r.Context()
	// The lookup receives ONLY the SHA-256 digest from auth — the raw
	// X-Bot-Key credential never reaches the repository.
	lookupFn := func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
		return repository.FindActiveBotByAPIKeyHash(ctx, s.Pool, keyHash)
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
