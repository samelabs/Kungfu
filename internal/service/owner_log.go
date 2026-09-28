package service

import (
	"context"
	"encoding/json"
	"kungfu.md/internal/credits"
	"math"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// GetOwnerLogs returns paginated log data for a bot owner.
// type is "credits" or "agent" — each returns a different structure.
// taskCode is accepted for URL compatibility and ignored.
func GetOwnerLogs(ctx context.Context, pool *pg.Pool, botID int64, logType string, page, pageSize int, taskCode string) (map[string]interface{}, error) {
	// Clamp before the offset is derived: page=0 would produce a
	// negative OFFSET (P3-23); the bounds hold for any caller, not
	// just today's handler.
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	switch logType {
	case "credits":
		return getCreditLogs(ctx, pool, botID, page, pageSize, offset)
	case "agent":
		return getAgentLogs(ctx, pool, botID, page, pageSize, offset)
	default:
		return nil, errors.New(400, "INVALID_TYPE", "Log type must be one of: credits, agent")
	}
}

// getCreditLogs returns credit transaction logs.
func getCreditLogs(ctx context.Context, pool *pg.Pool, botID int64, page, pageSize, offset int) (map[string]interface{}, error) {
	total, err := repository.CountCreditLogs(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing credit logs")
	}
	balance, balErr := credits.Balance(ctx, pool, botID)
	if balErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing credit logs")
	}
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
func getAgentLogs(ctx context.Context, pool *pg.Pool, botID int64, page, pageSize, offset int) (map[string]interface{}, error) {
	total, err := repository.CountAgentLogs(ctx, pool, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing agent logs")
	}
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

// --- presenter helpers ---

// paginationMap formats log entries for the owner dashboard.
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

// creditLogRow formats log entries for the owner dashboard.
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

// agentLogRow formats log entries for the owner dashboard.
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
