package service

import (
	"context"
	"encoding/json"
	"math"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// OwnerLogService mirrors PHP services/OwnerLogService.php.
// It provides paginated log views for task owners.

// GetOwnerLogs returns paginated log data for a bot owner.
// PHP: OwnerLogService::getLogs(botId, type, page, pageSize, taskCode)
//
// type can be "credits", "agent", or "task" — each returns a different structure.
func GetOwnerLogs(ctx context.Context, pool *pg.Pool, botID int64, logType string, page, pageSize int, taskCode string) (map[string]interface{}, error) {
	offset := (page - 1) * pageSize

	switch logType {
	case "credits":
		return getCreditLogs(ctx, pool, botID, page, pageSize, offset)
	case "agent":
		return getAgentLogs(ctx, pool, botID, page, pageSize, offset)
	case "task":
		return getTaskLogs(ctx, pool, botID, page, pageSize, offset, taskCode)
	default:
		return nil, errors.New(400, "INVALID_TYPE", "Log type must be one of: credits, agent, task")
	}
}

// getCreditLogs returns credit transaction logs.
// PHP: OwnerLogService::getLogs type='credits'
func getCreditLogs(ctx context.Context, pool *pg.Pool, botID int64, page, pageSize, offset int) (map[string]interface{}, error) {
	total, _ := repository.CountCreditLogs(ctx, pool, botID)
	balance, _ := repository.FindBalanceByBotID(ctx, pool, botID)
	rows, err := repository.ListCreditLogs(ctx, pool, botID, pageSize, offset)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing credit logs")
	}

	items := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		items = append(items, creditLogRow(&rows[i]))
	}

	return map[string]interface{}{
		"type":       "credits",
		"balance":    balance,
		"items":      items,
		"pagination": paginationMap(page, pageSize, total),
	}, nil
}

// getAgentLogs returns operation (agent) logs.
// PHP: OwnerLogService::getLogs type='agent'
func getAgentLogs(ctx context.Context, pool *pg.Pool, botID int64, page, pageSize, offset int) (map[string]interface{}, error) {
	total, _ := repository.CountAgentLogs(ctx, pool, botID)
	rows, err := repository.ListAgentLogs(ctx, pool, botID, pageSize, offset)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing agent logs")
	}

	items := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		items = append(items, agentLogRow(&rows[i]))
	}

	return map[string]interface{}{
		"type":       "agent",
		"items":      items,
		"pagination": paginationMap(page, pageSize, total),
	}, nil
}

// getTaskLogs returns task delivery logs with optional task_code filter.
// PHP: OwnerLogService::getLogs type='task'
func getTaskLogs(ctx context.Context, pool *pg.Pool, botID int64, page, pageSize, offset int, taskCode string) (map[string]interface{}, error) {
	total, _ := repository.CountTaskLogs(ctx, pool, botID, taskCode)

	tasks, _ := repository.ListOwnerTasksForFilter(ctx, pool, botID)
	taskFilters := make([]map[string]interface{}, 0, len(tasks))
	for i := range tasks {
		taskFilters = append(taskFilters, map[string]interface{}{
			"code":  tasks[i].Code,
			"title": tasks[i].Title,
		})
	}

	rows, err := repository.ListTaskLogs(ctx, pool, botID, pageSize, offset, taskCode)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing task logs")
	}

	items := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		items = append(items, taskLogRow(&rows[i]))
	}

	var taskFilter interface{}
	if taskCode != "" {
		taskFilter = taskCode
	}

	return map[string]interface{}{
		"type":        "task",
		"task_filter": taskFilter,
		"tasks":       taskFilters,
		"items":       items,
		"pagination":  paginationMap(page, pageSize, total),
	}, nil
}

// --- presenter helpers (mirror PHP OwnerLogPresenter) ---

// paginationMap mirrors PHP OwnerLogPresenter::pagination.
func paginationMap(page, pageSize int, total int64) map[string]interface{} {
	totalPages := int64(1)
	if total > 0 {
		totalPages = int64(math.Ceil(float64(total) / float64(pageSize)))
	}
	return map[string]interface{}{
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
	}
}

// creditLogRow mirrors PHP OwnerLogPresenter::creditLogRow.
func creditLogRow(t *model.Transaction) map[string]interface{} {
	return map[string]interface{}{
		"id":            t.ID,
		"type":          t.Type,
		"amount":        t.Amount,
		"balance_after": t.BalanceAfter,
		"ref_type":      t.RefType,
		"ref_id":        t.RefID,
		"created_at":    t.CreatedAt,
	}
}

// agentLogRow mirrors PHP OwnerLogPresenter::agentLogRow.
func agentLogRow(l *model.LogEntry) map[string]interface{} {
	var requestData interface{}
	if l.RequestData != nil && *l.RequestData != "" {
		var decoded interface{}
		if err := json.Unmarshal([]byte(*l.RequestData), &decoded); err == nil {
			requestData = decoded
		}
	}
	return map[string]interface{}{
		"id":           l.ID,
		"action":       l.Action,
		"target_type":  l.TargetType,
		"target_id":    l.TargetID,
		"ip_address":   l.IPAddress,
		"user_agent":   l.UserAgent,
		"request_data": requestData,
		"success":      l.Success,
		"error_code":   l.ErrorCode,
		"error_msg":    l.ErrorMsg,
		"created_at":   l.CreatedAt,
	}
}

// taskLogRow mirrors PHP OwnerLogPresenter::taskLogRow.
func taskLogRow(r *repository.TaskLogRow) map[string]interface{} {
	var payload interface{}
	if r.PayloadJSON != nil && *r.PayloadJSON != "" {
		var decoded interface{}
		if err := json.Unmarshal([]byte(*r.PayloadJSON), &decoded); err == nil {
			payload = decoded
		}
	}
	return map[string]interface{}{
		"id":            r.ID,
		"task_code":     r.TaskCode,
		"bot_id":        r.BotID,
		"action":        taskLogActionLabel(r.Action),
		"payload_json":  payload,
		"response_code": r.ResponseCode,
		"response_body": r.ResponseBody,
		"success":       r.Success,
		"error_code":    r.ErrorCode,
		"error_message": r.ErrorMessage,
		"created_at":    r.CreatedAt,
	}
}
