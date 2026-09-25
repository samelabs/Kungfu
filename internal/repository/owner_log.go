package repository

import (
	"context"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// -- 1. tableExists --
func OwnerLogTableExists(ctx context.Context, q pg.Querier, table string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1
		)`, table).Scan(&exists)
	return exists, err
}

// -- 2. findBalanceByBotId --
// -- 3. countCreditLogs --
func CountCreditLogs(ctx context.Context, q pg.Querier, botID int64) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total
		FROM tb_transactions
		WHERE bot_id = $1`, botID).Scan(&count)
	return count, err
}

// -- 4. listCreditLogs --
func ListCreditLogs(ctx context.Context, q pg.Querier, botID int64, pageSize, offset int) ([]model.Transaction, error) {
	rows, err := q.Query(ctx, `
		SELECT id, bot_id, type, amount, balance_after, ref_type, ref_id, created_at
		FROM tb_transactions
		WHERE bot_id = $1
		ORDER BY id DESC
		LIMIT $2 OFFSET $3`, botID, pageSize, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.Transaction
	for rows.Next() {
		var (
			t         model.Transaction
			botID     int32
			createdAt time.Time
		)
		if err := rows.Scan(&t.ID, &botID, &t.Type, &t.Amount, &t.BalanceAfter,
			&t.RefType, &t.RefID, &createdAt); err != nil {
			return nil, err
		}
		t.BotID = int64(botID)
		t.CreatedAt = timeToStr(createdAt)
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// -- 5. countAgentLogs --
func CountAgentLogs(ctx context.Context, q pg.Querier, botID int64) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total
		FROM tb_logs
		WHERE bot_id = $1`, botID).Scan(&count)
	return count, err
}

// -- 6. listAgentLogs --
func ListAgentLogs(ctx context.Context, q pg.Querier, botID int64, pageSize, offset int) ([]model.LogEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT id, bot_id, action, target_type, target_id, ip_address, user_agent,
		       request_data::text, success, error_code, error_msg, created_at
		FROM tb_logs
		WHERE bot_id = $1
		ORDER BY id DESC
		LIMIT $2 OFFSET $3`, botID, pageSize, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.LogEntry
	for rows.Next() {
		var (
			l         model.LogEntry
			dbBotID   *int32
			createdAt time.Time
		)
		if err := rows.Scan(&l.ID, &dbBotID, &l.Action, &l.TargetType, &l.TargetID,
			&l.IPAddress, &l.UserAgent, &l.RequestData, &l.Success,
			&l.ErrorCode, &l.ErrorMsg, &createdAt); err != nil {
			return nil, err
		}
		if dbBotID != nil {
			bid := int64(*dbBotID)
			l.BotID = &bid
		}
		l.CreatedAt = timeToStr(createdAt)
		items = append(items, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
