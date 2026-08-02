package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
func FindBalanceByBotID(ctx context.Context, q pg.Querier, botID int64) (float64, error) {
	var balance pgtype.Numeric
	err := q.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id = $1`, botID).Scan(&balance)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return numericToFloat(balance), nil
}

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
			t            model.Transaction
			botID        int32
			amount       pgtype.Numeric
			balanceAfter pgtype.Numeric
			createdAt    time.Time
		)
		if err := rows.Scan(&t.ID, &botID, &t.Type, &amount, &balanceAfter,
			&t.RefType, &t.RefID, &createdAt); err != nil {
			return nil, err
		}
		t.BotID = int64(botID)
		t.Amount = numericToFloat(amount)
		t.BalanceAfter = numericToFloat(balanceAfter)
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

// OwnerTaskFilterItem holds the code+title pair used to populate task filter dropdowns.
type OwnerTaskFilterItem struct {
	Code  string
	Title string
}

// -- 7. listOwnerTasksForFilter --
func ListOwnerTasksForFilter(ctx context.Context, q pg.Querier, botID int64) ([]OwnerTaskFilterItem, error) {
	rows, err := q.Query(ctx, `
		SELECT code, title
		FROM tb_tasks
		WHERE bot_id = $1
		ORDER BY created_at DESC`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []OwnerTaskFilterItem
	for rows.Next() {
		var it OwnerTaskFilterItem
		if err := rows.Scan(&it.Code, &it.Title); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// TaskLogRow holds the full projection of tb_task_logs used by the owner-log views.
type TaskLogRow struct {
	ID           int64
	TaskCode     string
	BotID        *int64
	Action       string
	PayloadJSON  *string
	ResponseCode *int
	ResponseBody *string
	Success      bool
	ErrorCode    *string
	ErrorMessage *string
	CreatedAt    string
}

// -- 8. countTaskLogs / listTaskLogs --
func CountTaskLogs(ctx context.Context, q pg.Querier, botID int64, taskCode string) (int64, error) {
	if taskCode == "" {
		var count int64
		err := q.QueryRow(ctx, `
			SELECT COUNT(*) AS total
			FROM tb_task_logs l
			INNER JOIN tb_tasks t ON t.code = l.task_code
			WHERE t.bot_id = $1`, botID).Scan(&count)
		return count, err
	}
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS total
		FROM tb_task_logs l
		INNER JOIN tb_tasks t ON t.code = l.task_code
		WHERE t.bot_id = $1 AND l.task_code = $2`, botID, taskCode).Scan(&count)
	return count, err
}

func ListTaskLogs(ctx context.Context, q pg.Querier, botID int64, pageSize, offset int, taskCode string) ([]TaskLogRow, error) {
	if taskCode == "" {
		rows, err := q.Query(ctx, `
			SELECT l.id, l.task_code, l.bot_id, l.action, l.payload_json::text, l.response_code,
			       l.response_body, l.success, l.error_code, l.error_message, l.created_at
			FROM tb_task_logs l
			INNER JOIN tb_tasks t ON t.code = l.task_code
			WHERE t.bot_id = $1
			ORDER BY l.id DESC
			LIMIT $2 OFFSET $3`, botID, pageSize, offset)
		if err != nil {
			return nil, err
		}
		return collectTaskLogRows(rows)
	}
	rows, err := q.Query(ctx, `
		SELECT l.id, l.task_code, l.bot_id, l.action, l.payload_json::text, l.response_code,
		       l.response_body, l.success, l.error_code, l.error_message, l.created_at
		FROM tb_task_logs l
		INNER JOIN tb_tasks t ON t.code = l.task_code
		WHERE t.bot_id = $1 AND l.task_code = $2
		ORDER BY l.id DESC
		LIMIT $3 OFFSET $4`, botID, taskCode, pageSize, offset)
	if err != nil {
		return nil, err
	}
	return collectTaskLogRows(rows)
}

func collectTaskLogRows(rows pgx.Rows) ([]TaskLogRow, error) {
	defer rows.Close()
	var out []TaskLogRow
	for rows.Next() {
		var (
			r         TaskLogRow
			dbBotID   *int32
			createdAt time.Time
		)
		if err := rows.Scan(&r.ID, &r.TaskCode, &dbBotID, &r.Action, &r.PayloadJSON,
			&r.ResponseCode, &r.ResponseBody, &r.Success, &r.ErrorCode,
			&r.ErrorMessage, &createdAt); err != nil {
			return nil, err
		}
		if dbBotID != nil {
			bid := int64(*dbBotID)
			r.BotID = &bid
		}
		r.CreatedAt = timeToStr(createdAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
