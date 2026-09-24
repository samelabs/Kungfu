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

// handleOwnerRegister is the Owner browser registration endpoint
// (POST /api/owner/register): the canonical registration path for the
// owner UI, guarded by the same owner-mutation gate as the other
// owner browser mutations. It reuses the shared service.Register
// identity authority (also used by the MCP account_register tool);
// the one-time key disclosure stays identical.
func (s *Server) handleOwnerRegister(w http.ResponseWriter, r *http.Request) {
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

	// IP-level registration rate limit (shared authority with the MCP
	// account_register tool).
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

// requireOwnerAuth authenticates via session cookie and returns the bot.
func (s *Server) requireOwnerAuth(r *http.Request) (*model.Bot, error) {
	lookupFn := func(ctx context.Context, botID int64) (*model.Bot, error) {
		return repository.FindOwnerSessionBotByID(ctx, s.Pool, botID)
	}
	return auth.RequireOwnerSession(r.Context(), lookupFn, r, s.Config.SessionSecret)
}

// requireBotAuth authenticates via X-Bot-Key header. This serves the
// Owner browser surface's testtask endpoint; the Agent execution
// protocol is MCP-only (Authorization: Bearer) and shares the same
// VerifyAgentKey identity authority.
func (s *Server) requireBotAuth(r *http.Request) (*model.Bot, error) {
	ctx := r.Context()
	lookupFn := func(ctx context.Context, keyHash []byte) (*model.Bot, error) {
		return repository.FindActiveBotByAPIKeyHash(ctx, s.Pool, keyHash)
	}
	bot, err := auth.VerifyBotAuth(ctx, lookupFn, r)
	if err != nil {
		return nil, err
	}
	updateFn := func(ctx context.Context, botID int64) error {
		return repository.UpdateLastActiveAt(ctx, s.Pool, botID)
	}
	auth.MaybeUpdateLastActive(ctx, updateFn, bot.ID)
	return bot, nil
}
