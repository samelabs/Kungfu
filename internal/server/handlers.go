package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	authImpl "kungfu.md/internal/auth"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"

	"encoding/json"
)

// Kungfu list pagination defaults — server-owned (no global config coupling).
const (
	defaultKungfuListLimit = 50
	maxKungfuListLimit     = 100
	maxKungfuListOffset    = 10000
)

// -- Kungfu Handlers --

func (s *Server) handleKungfuList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	limit := clampInt(getQueryInt(r, "limit", defaultKungfuListLimit), 1, maxKungfuListLimit)
	offset := clampInt(getQueryInt(r, "offset", 0), 0, maxKungfuListOffset)

	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	// Rate limit
	if !s.RateLimiter.CheckAPI(bot.ID, "list") {
		d := s.RateLimiter.CheckAPIWithDetails(bot.ID, "list")
		RateLimitResponse(w, d.RetryAfter, d.Limit, d.Window)
		return
	}

	result, err := service.ListKungfusForBot(r.Context(), s.Pool, bot.ID, limit, offset)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "")
}

func (s *Server) handleKungfuPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}

	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	if !s.RateLimiter.CheckAPI(bot.ID, "push") {
		d := s.RateLimiter.CheckAPIWithDetails(bot.ID, "push")
		RateLimitResponse(w, d.RetryAfter, d.Limit, d.Window)
		return
	}

	result, err := service.Push(r.Context(), s.Pool, bot.ID, input,
		s.Config.MaxTitleLength, s.Config.MaxTags, s.Config.MaxTagLength,
		s.Config.MaxDescriptionLength, s.Config.MaxContentSize)
	if err != nil {
		handleAppError(w, err)
		return
	}

	msg := "Kungfu updated successfully"
	if result.Action == "created" {
		msg = "Kungfu published successfully"
	}
	SuccessResponse(w, result, msg)
}

func (s *Server) handleKungfuGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	if !s.RateLimiter.CheckAPI(bot.ID, "get") {
		d := s.RateLimiter.CheckAPIWithDetails(bot.ID, "get")
		RateLimitResponse(w, d.RetryAfter, d.Limit, d.Window)
		return
	}

	code := chi.URLParam(r, "code")
	result, err := service.GetKungfuForBot(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "")
}

func (s *Server) handleKungfuDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	code := chi.URLParam(r, "code")
	result, err := service.Delete(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Deletion successful")
}

func (s *Server) handleKungfuShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	code := chi.URLParam(r, "code")
	result, err := service.Share(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}

	msg := "Shared successfully"
	if result["message"] == "Already public. Share this code with other agents." {
		msg = "Already shared"
	}
	SuccessResponse(w, result, msg)
}

func (s *Server) handleKungfuUnshare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	code := chi.URLParam(r, "code")
	result, err := service.Unshare(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}

	msg := "Unshared successfully"
	if result["message"] == "Already private" {
		msg = "Already private"
	}
	SuccessResponse(w, result, msg)
}

// -- Task Handlers (Agent) --

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	_ = bot
	result, err := service.ListOpenTasks(r.Context(), s.Pool)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "")
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	_ = bot
	code := chi.URLParam(r, "code")
	result, err := service.GetOpenTask(r.Context(), s.Pool, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "")
}

func (s *Server) handleTaskSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}

	if !s.RateLimiter.CheckAPI(bot.ID, "task_submit") {
		d := s.RateLimiter.CheckAPIWithDetails(bot.ID, "task_submit")
		RateLimitResponse(w, d.RetryAfter, d.Limit, d.Window)
		return
	}

	code := chi.URLParam(r, "code")

	// Client idempotency contract: the caller supplies a stable
	// Idempotency-Key so a lost-response retry resumes the SAME durable
	// submission instead of creating a new one.
	requestKey := r.Header.Get("Idempotency-Key")
	if requestKey == "" {
		MissingField(w, "Idempotency-Key header is required")
		return
	}

	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}

	result, err := service.Submit(r.Context(), s.Pool, code, bot.ID, requestKey, input)
	if err != nil {
		handleAppError(w, err)
		return
	}
	// settled -> 200 with settlement facts; non-terminal durable states
	// (reserved/delivering/uncertain) -> 202 with the submission identity;
	// never pretend an unresolved outcome was delivered.
	if result.State == repository.SubStateSettled {
		SuccessResponse(w, result, "Task submission delivered")
		return
	}
	AcceptedResponse(w, result, "Task submission accepted; delivery in progress")
}

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

func (s *Server) handleTaskGuide(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, r, "task_guide", "")
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
	setOwnerCookie(w, result.BotID, s.Config.SessionSecret, middleware.IsHTTPS(r, s.TrustedProxies))

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
	SuccessResponse(w, result, "Password changed")
}

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
	input, err := parseJSONBodyRequired(r, false, "Request body must be valid JSON")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	currentKey, _ := input["current_key"].(string)
	result, err := service.ResetKey(r.Context(), s.Pool, s.RateLimiter, bot.ID, currentKey)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Key reset successful")
}

func (s *Server) handleOwnerTasksList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	result, err := service.ListTasks(r.Context(), s.Pool, bot.ID)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerTasksWire(result), "")
}

func (s *Server) handleOwnerTaskGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := chi.URLParam(r, "code")
	result, err := service.GetTask(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerTasksWire(result), "")
}

// parseCredits extracts a whole-integer Credits value from a decoded
// JSON body field. JSON numbers decode as float64: an integral value
// parseCredits extracts a whole-integer Credit value from a decoded
// JSON field. EXACT integer parsing only — no Credit value ever passes
// through float64 (which silently corrupts integers above 2^53).
// json.Number (UseNumber bodies) and canonical integer strings parse
// via strconv-grade exact integer semantics; fractional presentations
// ("1000.5", "0.0001", "9007199254740993.0") are REJECTED — never
// rounded, never float-converted. Returns (value, present, ok).
func parseCredits(v interface{}) (int64, bool, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false, true
	case json.Number:
		// Lossless path (body parsed with UseNumber): the exact source
		// text. Int64() rejects fractions and >int64 range alike.
		if n, err := t.Int64(); err == nil {
			return n, true, true
		}
		return 0, true, false
	case string:
		// The ONE canonical parser (shared with jsonCredits): no
		// trim, no float, no coercion. The browser may trim user
		// input once as UX normalization before sending; non-canonical
		// wire input is rejected here, never silently repaired.
		if n, err := parseCanonicalEconInt(t); err == nil {
			return n, true, true
		}
		return 0, true, false
	default:
		return 0, true, false
	}
}

func (s *Server) handleOwnerTaskCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	input, err := parseJSONBodyRequiredNumbers(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	// Parse typed input
	ctInput := &service.CreateTaskInput{
		Title:        getStr(input["title"]),
		Requirements: getStr(input["requirements"]),
		PostAPI:      getStr(input["postapi"]),
	}
	if v, present, ok := parseCredits(input["budget"]); !ok {
		ErrorResponse(w, 400, "INVALID_BUDGET", "Budget must be a whole number of credits", nil)
		return
	} else if present {
		ctInput.Budget = v
	}
	if v, present, ok := parseCredits(input["price"]); !ok {
		ErrorResponse(w, 400, "INVALID_PRICE", "Price must be a whole number of credits", nil)
		return
	} else if present {
		ctInput.Price = v
	}
	if b, ok := input["open_now"].(bool); ok {
		ctInput.OpenNow = b
	}
	cfg := &service.OwnerTaskConfig{MaxTitleLength: s.Config.MaxTitleLength}
	result, err := service.CreateTask(r.Context(), s.Pool, bot.ID, cfg, ctInput)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerTasksWire(result), "Task created")
}

func (s *Server) handleOwnerTaskOpen(w http.ResponseWriter, r *http.Request) {
	s.ownerTaskStatus(w, r, "open")
}

func (s *Server) handleOwnerTaskClose(w http.ResponseWriter, r *http.Request) {
	s.ownerTaskStatus(w, r, "closed")
}

func (s *Server) ownerTaskStatus(w http.ResponseWriter, r *http.Request, status string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := chi.URLParam(r, "code")
	result, err := service.SetTaskStatus(r.Context(), s.Pool, bot.ID, code, status)
	if err != nil {
		handleAppError(w, err)
		return
	}
	msg := "Task opened"
	if status == "closed" {
		msg = "Task closed"
	}
	SuccessResponse(w, ownerTasksWire(result), msg)
}

func (s *Server) handleOwnerTaskAddBudget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	input, err := parseJSONBodyRequiredNumbers(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := chi.URLParam(r, "code")
	amount, present, ok := parseCredits(input["amount"])
	if !ok || !present {
		ErrorResponse(w, 400, "INVALID_AMOUNT", "Budget amount must be a whole number of credits", nil)
		return
	}
	result, err := service.AddTaskBudget(r.Context(), s.Pool, bot.ID, code, amount)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerTasksWire(result), "Budget added")
}

func (s *Server) handleOwnerTaskRefund(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := chi.URLParam(r, "code")
	result, err := service.RefundTaskBudget(r.Context(), s.Pool, bot.ID, code)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerTasksWire(result), "Budget refunded")
}

func (s *Server) handleOwnerTaskEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireOwnerAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	input, err := parseJSONBodyRequiredNumbers(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := chi.URLParam(r, "code")
	// Parse typed input (optional fields)
	utInput := &service.UpdateTaskBasicsInput{}
	if v, ok := input["title"]; ok {
		s := getStr(v)
		utInput.Title = &s
	}
	if v, ok := input["requirements"]; ok {
		s := getStr(v)
		utInput.Requirements = &s
	}
	if v, ok := input["postapi"]; ok {
		s := getStr(v)
		utInput.PostAPI = &s
	}
	if v, present := input["price"]; present {
		f, _, ok := parseCredits(v)
		if !ok {
			ErrorResponse(w, 400, "INVALID_PRICE", "Price must be a whole number of credits", nil)
			return
		}
		utInput.Price = &f
	}
	cfg := &service.OwnerTaskConfig{MaxTitleLength: s.Config.MaxTitleLength}
	result, err := service.UpdateTaskBasics(r.Context(), s.Pool, bot.ID, code, cfg, utInput)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, ownerTasksWire(result), "Task updated")
}

func (s *Server) handleTestTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	bot, err := s.requireBotAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	code := chi.URLParam(r, "code")

	// Same client idempotency contract as agent submissions.
	requestKey := r.Header.Get("Idempotency-Key")
	if requestKey == "" {
		MissingField(w, "Idempotency-Key header is required")
		return
	}

	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	result, err := service.TestTaskDeliver(r.Context(), s.Pool, bot.ID, code, requestKey, input)
	if err != nil {
		handleAppError(w, err)
		return
	}
	if result.State == repository.SubStateSettled {
		SuccessResponse(w, result, "Task test delivered")
		return
	}
	AcceptedResponse(w, result, "Task test accepted; delivery in progress")
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
	allowed := map[string]bool{"credits": true, "agent": true, "task": true}
	if !allowed[logType] {
		ErrorResponse(w, 400, "INVALID_TYPE", "Type must be one of: credits, agent, task", nil)
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

// setOwnerCookie wraps auth.SetOwnerSessionCookie.
func setOwnerCookie(w http.ResponseWriter, botID int64, secret string, isHTTPS bool) {
	authImpl.SetOwnerSessionCookie(w, botID, secret, isHTTPS)
}

// clearOwnerCookie wraps auth.ClearOwnerSessionCookie.
func clearOwnerCookie(w http.ResponseWriter, isHTTPS bool) {
	authImpl.ClearOwnerSessionCookie(w, isHTTPS)
}

// getStr extracts a string from an interface{} value.
func getStr(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
