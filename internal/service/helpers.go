package service

import (
	"context"
	"log"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// logOperation is a best-effort audit wrapper: the main business flow is
// never failed by an audit write, but a failed audit write is logged.
func logOperation(ctx context.Context, q pg.Querier, botID *int64, action string,
	targetType, targetID *string, requestData map[string]interface{}, success bool) {
	if err := repository.InsertOperationLog(ctx, q, repository.LogInsertData{
		BotID:       botID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		RequestData: requestData,
		Success:     success,
	}); err != nil {
		log.Printf("audit log write failed: action=%s target=%v err=%v", action, targetID, err)
	}
}

func strPtr(s string) *string { return &s }
