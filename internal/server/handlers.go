package server

import (
	"net/http"

	authImpl "kungfu.md/internal/auth"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
)

// -- Kungfu Handlers --

// -- Task Handlers (Agent) --

// -- Placeholder handlers for web routes and owner routes --

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, r, "home", "")
}

func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, r, "credits", "")
}

func (s *Server) handleOwnerPage(section string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.renderTemplate(w, r, "owner", section)
	}
}

// handleLegalPage renders /terms or /privacy from the single i18n
// template source.
func (s *Server) handleLegalPage(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.renderTemplate(w, r, kind, "")
	}
}

// -- Owner API Handlers (stubs - to be filled after service subagent completes) --

func (s *Server) handleOwnerSessionGet(w http.ResponseWriter, r *http.Request) {
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, map[string]interface{}{
		"bot_id":   bot.ID,
		"bot_name": bot.BotName,
		"status":   bot.Status,
	}, "Owner session active")
}

func (s *Server) handleOwnerSessionLogin(w http.ResponseWriter, r *http.Request) {
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON")
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
	rlResult := s.RateLimiter.CheckOwnerLogin(ip)
	if !rlResult.Allowed {
		RateLimitResponse(w, rlResult.RetryAfter, rlResult.Limit, rlResult.Window)
		return
	}

	result, err := service.OwnerLogin(r.Context(), s.Pool, name, password)
	if err != nil {
		handleAppError(w, err)
		return
	}

	// Set session cookie
	setOwnerCookie(w, result.BotID, result.PasswordHash, s.Config.SessionSecret, middleware.IsHTTPS(r, s.TrustedProxies))

	SuccessResponse(w, map[string]interface{}{
		"bot_id":   result.BotID,
		"bot_name": result.BotName,
		"status":   result.Status,
	}, "Owner login successful")
}

func (s *Server) handleOwnerSessionLogout(w http.ResponseWriter, r *http.Request) {
	// Logout must always succeed and clear the cookie, regardless of session
	// validity — a stale/expired session must not block the client from
	// removing its local login cookie. Audit owner_logout only when a valid
	// owner identity can be resolved from the session.
	if bot, err := s.requireOwnerAuth(r); err == nil && bot != nil {
		service.OwnerLogout(r.Context(), s.Pool, bot.ID)
	}
	clearOwnerCookie(w, middleware.IsHTTPS(r, s.TrustedProxies))
	SuccessResponse(w, map[string]interface{}{}, "Owner logout successful")
}

// -- Remaining owner API handlers wired to services --

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	result, err := service.AccountOverview(r.Context(), s.Pool, bot.ID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerOverviewWire(result), "")
}

func (s *Server) handleKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	result, err := service.CurrentOwnerKey(r.Context(), s.Pool, bot.ID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	// Same browser economic wire authority as /api/account: the owner
	// browser must never meet a JSON numeric balance (int64 > 2^53
	// corrupts in JS Number). One converter, no second helper.
	SuccessResponse(w, ownerOverviewWire(result), "Key retrieved")
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	password, _ := input["password"].(string)
	newPassword, _ := input["new_password"].(string)
	if password == "" || newPassword == "" {
		MissingField(w, "password or new_password")
		return
	}
	result, err := service.ChangePassword(r.Context(), s.Pool, bot.BotName, password, newPassword)
	if err != nil {
		handleAppError(w, err)
		return
	}
	// A2: the password just changed, so every outstanding session cookie
	// (including this request's) stopped validating — re-issue one bound
	// to the NEW password version so the signed-in owner stays signed in.
	if fresh, ferr := repository.FindOwnerSessionBotByID(r.Context(), s.Pool, bot.ID); ferr == nil && fresh != nil {
		setOwnerCookie(w, bot.ID, fresh.PasswordHash, s.Config.SessionSecret, middleware.IsHTTPS(r, s.TrustedProxies))
	}
	SuccessResponse(w, result, "Password changed")
}

// handleResetKey rotates the signed-in owner's Agent key. No request
// body is read: the owner session (ownerMutation gate) is the whole
// authority; the browser sends an empty JSON object.
func (s *Server) handleResetKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	result, err := service.ResetKey(r.Context(), s.Pool, s.RateLimiter, bot.ID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Key reset successful")
}

func (s *Server) handleOwnerLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	logType := r.URL.Query().Get("type")
	if logType == "" {
		logType = "credits"
	}
	allowed := map[string]bool{"credits": true, "agent": true}
	if !allowed[logType] {
		ErrorResponse(w, 400, "INVALID_TYPE", "Type must be one of: credits, agent", nil)
		return
	}
	page := getQueryInt(r, "page", 1)
	if page < 1 {
		page = 1
	}
	pageSize := getQueryInt(r, "page_size", 20)
	if pageSize < 1 {
		pageSize = 20
	}
	pageSize = clampInt(pageSize, 1, 100)
	taskCode := r.URL.Query().Get("task_code")

	result, err := service.GetOwnerLogs(r.Context(), s.Pool, bot.ID, logType, page, pageSize, taskCode)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerLogsWire(result), "")
}

// handleAppError sends the appropriate error response for an AppError.
func handleAppError(w http.ResponseWriter, err error) {
	if ae, ok := apperrors.IsAppError(err); ok {
		ErrorResponse(w, ae.HTTPCode, ae.Code, ae.Message, ae.Details)
		return
	}
	if rle, ok := apperrors.IsRateLimitError(err); ok {
		RateLimitResponse(w, rle.RetryAfter, rle.Limit, rle.Window)
		return
	}
	ErrorResponse(w, 500, "INTERNAL_ERROR", "An internal error occurred", nil)
}

// setOwnerCookie wraps auth.SetOwnerSessionCookie. The password hash
// seeds the cookie's pv claim: a later password change invalidates the
// cookie (WO-17b A2).
func setOwnerCookie(w http.ResponseWriter, botID int64, passwordHash, secret string, isHTTPS bool) {
	authImpl.SetOwnerSessionCookie(w, botID, passwordHash, secret, isHTTPS)
}

// clearOwnerCookie wraps auth.ClearOwnerSessionCookie.
func clearOwnerCookie(w http.ResponseWriter, isHTTPS bool) {
	authImpl.ClearOwnerSessionCookie(w, isHTTPS)
}
