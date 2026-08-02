package repository

import (
	"context"

	"kungfu.md/internal/pg"
)

// TaskLogRepository mirrors PHP repositories/TaskLogRepository.php.
// Every method accepts a pg.Querier so it works with both *pgxpool.Pool and pgx.Tx.

// NewTaskLogInput holds the fields needed to insert a task-log row.
// Mirrors the PHP TaskLogRepository::insert([...]) array.
type NewTaskLogInput struct {
	TaskCode     string
	BotID        *int64
	Action       string
	PayloadJSON  *string
	ResponseCode *int
	ResponseBody *string
	Success      bool
	ErrorCode    *string
	ErrorMessage *string
}

// Insert mirrors PHP TaskLogRepository::insert(array $data).
// PHP: Database::insert('tb_task_logs', $data)
// success is a BOOLEAN column; PG accepts TRUE/FALSE directly.
func InsertTaskLog(ctx context.Context, q pg.Querier, in NewTaskLogInput) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_task_logs
		    (task_code, bot_id, action, payload_json, response_code, response_body,
		     success, error_code, error_message, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())`,
		in.TaskCode, in.BotID, in.Action, in.PayloadJSON, in.ResponseCode,
		in.ResponseBody, in.Success, in.ErrorCode, in.ErrorMessage)
	return err
}
