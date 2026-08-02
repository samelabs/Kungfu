package service

import (
	"context"
	"math"
	"strings"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// OwnerTaskService provides task delivery testing for owners.
// It provides full CRUD + budget management for task owners.

// refundCooldownDays is the minimum wait before a closed task budget can be refunded
const refundCooldownDays = 7

// maxRequirementsLen is the maximum character count for task requirements
const maxRequirementsLen = 20000

// maxPostapiLen is the maximum byte length for a Post API URL
const maxPostapiLen = 2048

// OwnerTaskConfig holds the per-request owner config needed by task validation.
// OwnerTaskConfig holds per-request validation parameters.
type OwnerTaskConfig struct {
	MaxTitleLength int
}

// ListTasks returns all tasks owned by botID with aggregated log stats.
func ListTasks(ctx context.Context, pool *pg.Pool, botID int64) (map[string]interface{}, error) {
	rows, err := repository.ListOwnerTasksWithStats(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing tasks")
	}

	tasks := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		tasks = append(tasks, ownerTaskSummary(&rows[i].Task, rows[i].LogCount, rows[i].SuccessCount, rows[i].FailureCount))
	}

	return map[string]interface{}{
		"tasks": tasks,
		"meta": map[string]interface{}{
			"returned": len(tasks),
		},
	}, nil
}

// GetTask returns a single task with its recent logs.
func GetTask(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error) {
	task, err := ownerTask(ctx, pool, botID, code)
	if err != nil {
		return nil, err
	}

	logs, _ := repository.FindRecentLogsByTaskCode(ctx, pool, code, 50)

	logItems := make([]map[string]interface{}, 0, len(logs))
	for i := range logs {
		logItems = append(logItems, ownerTaskDetailLogRow(&logs[i]))
	}

	return map[string]interface{}{
		"task": ownerTaskDetail(task),
		"logs": logItems,
	}, nil
}

// CreateTaskInput holds the parsed input for CreateTask.
type CreateTaskInput struct {
	Title        string
	Requirements string
	PostAPI      string
	Budget       float64
	Price        float64
	OpenNow      bool
}

// CreateTask creates a new task owned by botID.
func CreateTask(ctx context.Context, pool *pg.Pool, botID int64, cfg *OwnerTaskConfig, input *CreateTaskInput) (map[string]interface{}, error) {
	title := strings.TrimSpace(input.Title)
	requirements := strings.TrimSpace(input.Requirements)
	postapi := strings.TrimSpace(input.PostAPI)
	budget := roundMoney(input.Budget)
	price := roundMoney(input.Price)
	openNow := input.OpenNow

	// Validate basics + budget
	if err := validateTaskBasics(title, requirements, postapi, price, cfg); err != nil {
		return nil, err
	}
	if err := validateBudget(budget); err != nil {
		return nil, err
	}
	if openNow {
		if err := assertOpenable(postapi, budget, price); err != nil {
			return nil, err
		}
	}

	// Generate unique code
	taskCode, err := repository.GenerateUniqueTaskCode(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error generating task code")
	}

	status := "pending"
	if openNow {
		status = "open"
	}

	var openedAt *string
	if openNow {
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		openedAt = &now
	}

	var postAPIPtr *string
	if postapi != "" {
		postAPIPtr = &postapi
	}

	// Transaction: lock budget + insert task
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error creating task")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock task budget (deduct credits)
	if budget > 0 {
		if _, err := Record(ctx, pool, tx, botID, "lock_task", -budget, strPtr("task"), &taskCode); err != nil {
			if isInsufficientCredits(err) {
				return nil, errors.New(402, "INSUFFICIENT_CREDITS",
					"Not enough credits to fund this task budget. Complete platform tasks to earn credits.")
			}
			return nil, errors.New(500, "INTERNAL_ERROR", "Error locking task budget")
		}
	}

	if err := repository.InsertTask(ctx, tx, repository.NewTaskInput{
		Code:         taskCode,
		BotID:        botID,
		Title:        title,
		Requirements: requirements,
		PostAPI:      postAPIPtr,
		Budget:       budget,
		Price:        price,
		Pinned:       false,
		Status:       status,
		OpenedAt:     openedAt,
	}); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error inserting task")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error creating task")
	}

	logOperation(ctx, pool, &botID, "owner_task_create", strPtr("task"), &taskCode,
		map[string]interface{}{"title": title, "status": status}, true)

	// Re-fetch for response
	task, err := ownerTask(ctx, pool, botID, taskCode)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"task": ownerTaskDetail(task)}, nil
}

// SetTaskStatus opens or closes a task.
func SetTaskStatus(ctx context.Context, pool *pg.Pool, botID int64, code, status string) (map[string]interface{}, error) {
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating task status")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := repository.FindOwnerTaskByCodeForUpdate(ctx, tx, botID, code)
	if err != nil || task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}

	if status == "open" {
		// Assert openable: postapi, budget, price
		var postapi string
		if task.PostAPI != nil {
			postapi = *task.PostAPI
		}
		if err := assertOpenable(postapi, task.Budget, task.Price); err != nil {
			return nil, err
		}
		if err := repository.OpenOwnerTask(ctx, tx, botID, code); err != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Error opening task")
		}
	} else {
		if err := repository.CloseOwnerTask(ctx, tx, botID, code); err != nil {
			return nil, errors.New(500, "INTERNAL_ERROR", "Error closing task")
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating task status")
	}

	action := "owner_task_close"
	if status == "open" {
		action = "owner_task_open"
	}
	logOperation(ctx, pool, &botID, action, strPtr("task"), &code, nil, true)

	task2, err := ownerTask(ctx, pool, botID, code)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"task": ownerTaskDetail(task2)}, nil
}

// AddTaskBudget adds budget to a task, locking additional credits.
func AddTaskBudget(ctx context.Context, pool *pg.Pool, botID int64, code string, amount float64) (map[string]interface{}, error) {
	amount = roundMoney(amount)
	if amount <= 0 {
		return nil, errors.New(400, "INVALID_AMOUNT", "Budget amount must be greater than zero")
	}

	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error adding budget")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := repository.FindOwnerTaskByCodeForUpdate(ctx, tx, botID, code)
	if err != nil || task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}

	// Lock budget
	if _, err := Record(ctx, pool, tx, botID, "lock_task", -amount, strPtr("task"), &code); err != nil {
		if isInsufficientCredits(err) {
			return nil, errors.New(402, "INSUFFICIENT_CREDITS",
				"Not enough credits to add this task budget. Complete platform tasks to earn credits.")
		}
		return nil, errors.New(500, "INTERNAL_ERROR", "Error locking budget")
	}

	if err := repository.AddOwnerTaskBudget(ctx, tx, botID, code, amount); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error adding budget")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error adding budget")
	}

	logOperation(ctx, pool, &botID, "owner_task_add_budget", strPtr("task"), &code,
		map[string]interface{}{"amount": amount}, true)

	t, err := ownerTask(ctx, pool, botID, code)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"task": ownerTaskDetail(t)}, nil
}

// UpdateTaskBasicsInput holds optional fields for UpdateTaskBasics.
// Pointer fields distinguish "absent" (nil) from "provided as empty".
type UpdateTaskBasicsInput struct {
	Title        *string
	Requirements *string
	PostAPI      *string
	Price        *float64
}

// UpdateTaskBasics edits the basic fields of a closed task.
func UpdateTaskBasics(ctx context.Context, pool *pg.Pool, botID int64, code string, cfg *OwnerTaskConfig, input *UpdateTaskBasicsInput) (map[string]interface{}, error) {
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating task")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := repository.FindOwnerTaskByCodeForUpdate(ctx, tx, botID, code)
	if err != nil || task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}

	if task.Status != "closed" {
		return nil, errors.New(409, "TASK_MUST_BE_CLOSED", "Task must be closed before editing.")
	}

	// Resolve field values: use input if provided, otherwise keep existing
	title := task.Title
	if input.Title != nil {
		title = strings.TrimSpace(*input.Title)
	}
	requirements := task.Requirements
	if input.Requirements != nil {
		requirements = strings.TrimSpace(*input.Requirements)
	}
	postapi := ""
	if task.PostAPI != nil {
		postapi = *task.PostAPI
	}
	if input.PostAPI != nil {
		postapi = strings.TrimSpace(*input.PostAPI)
	}
	price := task.Price
	if input.Price != nil {
		price = roundMoney(*input.Price)
	}

	if err := validateTaskBasics(title, requirements, postapi, price, cfg); err != nil {
		return nil, err
	}

	var postAPIPtr *string
	if postapi != "" {
		postAPIPtr = &postapi
	}

	if err := repository.UpdateOwnerTaskBasics(ctx, tx, botID, code, title, requirements, postAPIPtr, price); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating task")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error updating task")
	}

	logOperation(ctx, pool, &botID, "owner_task_edit", strPtr("task"), &code,
		map[string]interface{}{"title": title, "price": price}, true)

	t, err := ownerTask(ctx, pool, botID, code)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"task": ownerTaskDetail(t)}, nil
}

// RefundTaskBudget refunds the remaining budget of a closed task to the owner.
// Requires the task to be closed for at least refundCooldownDays (7) days.
func RefundTaskBudget(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error) {
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error refunding budget")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := repository.FindOwnerTaskByCodeForUpdate(ctx, tx, botID, code)
	if err != nil || task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}

	if task.Status != "closed" {
		return nil, errors.New(409, "TASK_MUST_BE_CLOSED", "Only closed tasks can refund budget.")
	}

	budget := task.Budget
	if budget <= 0 {
		return nil, errors.New(409, "TASK_BUDGET_EMPTY", "Task budget is already zero.")
	}

	closedAt := ""
	if task.ClosedAt != nil {
		closedAt = *task.ClosedAt
	}
	if !canRefundBudget(closedAt) {
		return nil, errors.New(409, "TASK_REFUND_NOT_READY", "Refund is available 7 days after closing.")
	}

	// Refund credits
	if _, err := Record(ctx, pool, tx, botID, "refund_task", budget, strPtr("task"), &code); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error refunding budget")
	}

	if err := repository.ClearOwnerTaskBudget(ctx, tx, botID, code); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error clearing budget")
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error refunding budget")
	}

	logOperation(ctx, pool, &botID, "owner_task_refund", strPtr("task"), &code,
		map[string]interface{}{"amount": budget}, true)

	t, err := ownerTask(ctx, pool, botID, code)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"task": ownerTaskDetail(t)}, nil
}

// --- internal helpers ---

// ownerTask finds a task owned by botID or returns 404.
func ownerTask(ctx context.Context, pool *pg.Pool, botID int64, code string) (*model.Task, error) {
	task, err := repository.FindOwnerTaskByCode(ctx, pool, botID, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	if task == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}
	return task, nil
}

// canRefundBudget checks if enough time has passed since closing.
func canRefundBudget(closedAt string) bool {
	if closedAt == "" {
		return false
	}
	closed, err := time.Parse("2006-01-02 15:04:05", closedAt)
	if err != nil {
		return false
	}
	return closed.Unix() <= (time.Now().Unix() - int64(refundCooldownDays)*86400)
}

// validateTaskBasics formats log entries for the owner dashboard.
func validateTaskBasics(title, requirements, postapi string, price float64, cfg *OwnerTaskConfig) error {
	if title == "" {
		return errors.New(400, "MISSING_FIELD", "Missing required field: title")
	}
	maxTitle := 128
	if cfg != nil && cfg.MaxTitleLength > 0 {
		maxTitle = cfg.MaxTitleLength
	}
	if len([]rune(title)) > maxTitle {
		return errors.New(400, "TITLE_TOO_LONG", "Title maximum 128 characters")
	}
	if requirements == "" {
		return errors.New(400, "MISSING_FIELD", "Missing required field: requirements")
	}
	if len([]rune(requirements)) > maxRequirementsLen {
		return errors.New(400, "REQUIREMENTS_TOO_LONG", "Requirements maximum 20000 characters")
	}
	if err := validatePostapiField(postapi); err != nil {
		return err
	}
	if price <= 0 {
		return errors.New(400, "INVALID_PRICE", "Price must be greater than zero")
	}
	return nil
}

// validateBudget
func validateBudget(budget float64) error {
	if budget <= 0 {
		return errors.New(400, "INVALID_BUDGET", "Budget must be greater than zero")
	}
	if budget < MinOpenBudget {
		return errors.New(400, "TASK_BUDGET_TOO_LOW", "Budget must be at least 1000 credits")
	}
	return nil
}

// validatePostapiField formats log entries for the owner dashboard.
func validatePostapiField(postapi string) error {
	if postapi == "" {
		return errors.New(400, "MISSING_FIELD", "Missing required field: postapi")
	}
	if len(postapi) > maxPostapiLen {
		return errors.New(400, "POSTAPI_TOO_LONG", "Postapi maximum 2048 characters")
	}
	return nil
}

// assertOpenable formats log entries for the owner dashboard.
func assertOpenable(postapi string, budget, price float64) error {
	if err := validatePostapiField(postapi); err != nil {
		return err
	}
	if price <= 0 {
		return errors.New(400, "INVALID_PRICE", "Price must be greater than zero")
	}
	if budget < MinOpenBudget || budget < price {
		return errors.New(400, "TASK_BUDGET_TOO_LOW", "Open tasks require enough budget")
	}
	return nil
}

// roundMoney)
func roundMoney(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// isInsufficientCredits checks if the error is the 402 INSUFFICIENT_CREDITS error.
func isInsufficientCredits(err error) bool {
	if err == nil {
		return false
	}
	ae, ok := errors.IsAppError(err)
	return ok && ae.HTTPCode == 402
}

// ownerTaskSummary formats log entries for the owner dashboard.
func ownerTaskSummary(t *model.Task, logCount, successCount, failureCount int64) map[string]interface{} {
	pinned := 0
	if t.Pinned {
		pinned = 1
	}
	return map[string]interface{}{
		"code":          t.Code,
		"title":         t.Title,
		"requirements":  t.Requirements,
		"budget":        t.Budget,
		"price":         t.Price,
		"pinned":        pinned,
		"status":        t.Status,
		"created_at":    t.CreatedAt,
		"updated_at":    t.UpdatedAt,
		"opened_at":     t.OpenedAt,
		"closed_at":     t.ClosedAt,
		"log_count":     logCount,
		"success_count": successCount,
		"failure_count": failureCount,
	}
}

// ownerTaskDetail formats log entries for the owner dashboard.
// summary + postapi + review_note + reviewed_at
func ownerTaskDetail(t *model.Task) map[string]interface{} {
	m := ownerTaskSummary(t, 0, 0, 0)
	m["postapi"] = ""
	if t.PostAPI != nil {
		m["postapi"] = *t.PostAPI
	}
	m["review_note"] = t.ReviewNote
	m["reviewed_at"] = t.ReviewedAt
	return m
}

// ownerTaskDetailLogRow formats log entries for the owner dashboard.
func ownerTaskDetailLogRow(e *repository.TaskLogEntry) map[string]interface{} {
	var botID interface{}
	if e.BotID != nil {
		botID = *e.BotID
	}
	var respCode interface{}
	if e.ResponseCode != nil {
		respCode = *e.ResponseCode
	}
	return map[string]interface{}{
		"id":            e.ID,
		"bot_id":        botID,
		"action":        taskLogActionLabel(e.Action),
		"response_code": respCode,
		"success":       e.Success,
		"error_code":    e.ErrorCode,
		"error_message": e.ErrorMessage,
		"created_at":    e.CreatedAt,
	}
}

// taskLogActionLabel formats log entries for the owner dashboard.
func taskLogActionLabel(action string) string {
	switch action {
	case "kfcheck":
		return "Task check"
	case "post_succeeded":
		return "Delivery accepted"
	case "post_failed":
		return "Delivery failed"
	default:
		return action
	}
}
