package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	authImpl "kungfu.md/internal/auth"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/model"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/service"
)

// -- Kungfu Handlers --

func (s *Server) handleKungfuList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	limit := clampInt(getQueryInt(r, "limit", 50), 1, 100)
	offset := clampInt(getQueryInt(r, "offset", 0), 0, 10000)

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

	result, err := service.ListKungfusForBot(r.Context(), s.Pool, bot.ID, bot.Balance, limit, offset)
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
	result, err := service.GetKungfuForBot(r.Context(), s.Pool, bot.ID, bot.Balance, code)
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

	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}

	task, err := repository.FindTaskByCode(r.Context(), s.Pool, code)
	if err != nil || task == nil {
		ErrorResponse(w, 404, "NOT_FOUND", "Task not found", nil)
		return
	}

	if task.Status != "open" {
		ErrorResponse(w, 409, "TASK_NOT_OPEN", "Task is not open for submissions", nil)
		return
	}

	result, err := service.Submit(r.Context(), s.Pool, taskToMap(task), bot.ID, input)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Task submission delivered")
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

	ip := middleware.GetClientIP(r)
	rlResult := s.RateLimiter.CheckOwnerLogin(ip)
	if !rlResult.Allowed {
		RateLimitResponse(w, rlResult.RetryAfter, rlResult.Limit, rlResult.Window)
		return
	}

	// Look up bot by name
	bot, err := repository.FindActiveBotCredentialsByName(r.Context(), s.Pool, name)
	if err != nil || bot == nil {
		ErrorResponse(w, 401, "INVALID_CREDENTIALS", "Bot name or password is incorrect", nil)
		return
	}

	// Verify password
	if !verifyPassword(password, bot.PasswordHash) {
		ErrorResponse(w, 401, "INVALID_CREDENTIALS", "Bot name or password is incorrect", nil)
		return
	}

	// Set session cookie
	isHTTPS := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	setOwnerCookie(w, bot.ID, s.Config.SessionSecret, isHTTPS)

	SuccessResponse(w, map[string]interface{}{
		"bot_id":   bot.ID,
		"bot_name": bot.BotName,
		"status":   bot.Status,
	}, "Owner login successful")
}

func (s *Server) handleOwnerSessionLogout(w http.ResponseWriter, r *http.Request) {
	isHTTPS := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	clearOwnerCookie(w, isHTTPS)
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
	SuccessResponse(w, result, "")
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
	SuccessResponse(w, result, "Key retrieved")
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
	SuccessResponse(w, result, "")
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
	SuccessResponse(w, result, "")
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
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
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
	if f, ok := input["budget"].(float64); ok {
		ctInput.Budget = f
	}
	if s, ok := input["budget"].(string); ok {
		ctInput.Budget, _ = strconv.ParseFloat(s, 64)
	}
	if f, ok := input["price"].(float64); ok {
		ctInput.Price = f
	}
	if s, ok := input["price"].(string); ok {
		ctInput.Price, _ = strconv.ParseFloat(s, 64)
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
	SuccessResponse(w, result, "Task created")
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
	SuccessResponse(w, result, msg)
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
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	code := chi.URLParam(r, "code")
	amountStr, _ := input["amount"].(string)
	if amountStr == "" {
		if f, ok := input["amount"].(float64); ok {
			amountStr = service.FormatMoney(f)
		}
	}
	amount, _ := strconv.ParseFloat(amountStr, 64)
	result, err := service.AddTaskBudget(r.Context(), s.Pool, bot.ID, code, amount)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Budget added")
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
	SuccessResponse(w, result, "Budget refunded")
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
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
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
	if v, ok := input["price"]; ok {
		var f float64
		if fv, ok := v.(float64); ok {
			f = fv
		} else if sv, ok := v.(string); ok {
			f, _ = strconv.ParseFloat(sv, 64)
		}
		utInput.Price = &f
	}
	cfg := &service.OwnerTaskConfig{MaxTitleLength: s.Config.MaxTitleLength}
	result, err := service.UpdateTaskBasics(r.Context(), s.Pool, bot.ID, code, cfg, utInput)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Task updated")
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
	input, err := parseJSONBodyRequired(r, true, "Request body must be valid JSON object")
	if err != nil {
		InvalidJSON(w, err.Error())
		return
	}
	result, err := service.TestTaskDeliver(r.Context(), s.Pool, bot.ID, code, input)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, result, "Task test delivered")
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
	SuccessResponse(w, result, "")
}

// Helper: taskToMap converts model.Task to map for service.Submit
func taskToMap(t *model.Task) map[string]interface{} {
	if t == nil {
		return make(map[string]interface{})
	}
	m := map[string]interface{}{
		"id":           t.ID,
		"code":         t.Code,
		"bot_id":       t.BotID,
		"title":        t.Title,
		"requirements": t.Requirements,
		"budget":       t.Budget,
		"price":        t.Price,
		"pinned":       t.Pinned,
		"status":       t.Status,
		"created_at":   t.CreatedAt,
		"updated_at":   t.UpdatedAt,
	}
	// PostAPI is *string — dereference so getString works
	if t.PostAPI != nil {
		m["postapi"] = *t.PostAPI
	} else {
		m["postapi"] = ""
	}
	if t.ReviewNote != nil {
		m["review_note"] = *t.ReviewNote
	}
	if t.OpenedAt != nil {
		m["opened_at"] = *t.OpenedAt
	}
	if t.ClosedAt != nil {
		m["closed_at"] = *t.ClosedAt
	}
	return m
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

// verifyPassword wraps auth.VerifyPassword.
func verifyPassword(password, hash string) bool {
	return authImpl.VerifyPassword(password, hash)
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
