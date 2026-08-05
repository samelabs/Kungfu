package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"time"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/publiccode"
)

// TaskRepository persists task rows.
// Every method accepts a pg.Querier so it works with both *pgxpool.Pool and pgx.Tx.

// minOpenBudget is the minimum budget required for a task to be considered "open" on the board.
const minOpenBudget = 1000.0

// openBudgetWhereClause returns the SQL fragment that defines an "open, fundable" task:
// open status, positive price, and budget >= both the minimum and the per-unit price.
// When alias is empty the columns are unqualified; otherwise they are prefixed with "alias.".
func openBudgetWhereClause(alias string) string {
	if alias == "" {
		return "status = 'open' AND price > 0 AND budget >= 1000.0 AND budget >= price"
	}
	return alias + ".status = 'open' AND " + alias + ".price > 0 AND " +
		alias + ".budget >= 1000.0 AND " + alias + ".budget >= " + alias + ".price"
}

// -- 1. countOpenTasks --
// CountOpenTasks returns the number of tasks currently visible on the open board.
func CountOpenTasks(ctx context.Context, q pg.Querier) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS count
		FROM tb_tasks t
		WHERE `+openBudgetWhereClause("t")).Scan(&count)
	return count, err
}

// -- 2. listOpenTasks --
// ListOpenTasks returns all open, fundable tasks, pinned first then newest.
func ListOpenTasks(ctx context.Context, q pg.Querier) ([]model.Task, error) {
	rows, err := q.Query(ctx, `
		SELECT id, code, bot_id, title, requirements, postapi, budget, price, pinned, status,
		       review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
		FROM tb_tasks t
		WHERE `+openBudgetWhereClause("t")+`
		ORDER BY t.pinned DESC, t.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var t model.Task
		if err := scanTask(&t, rows); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

// -- 3. findOpenTaskByCode --
// FindOpenTaskByCode returns the open, fundable task with the given code, or nil.
func FindOpenTaskByCode(ctx context.Context, q pg.Querier, code string) (*model.Task, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, requirements, postapi, budget, price, pinned, status,
		       review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
		FROM tb_tasks t
		WHERE t.code = $1 AND `+openBudgetWhereClause("t"), code)
	var t model.Task
	if err := scanTask(&t, row); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// -- 4. tableExists --
// TableExists reports whether a table exists in the current schema.
func TableExists(ctx context.Context, q pg.Querier, table string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1
		)`, table).Scan(&exists)
	return exists, err
}

// -- 5. columnExists --
// ColumnExists reports whether a column exists on a table in the current schema.
func ColumnExists(ctx context.Context, q pg.Querier, table, column string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1
			  AND column_name = $2
		)`, table, column).Scan(&exists)
	return exists, err
}

// TaskWithStats holds a task row joined with aggregated log counts.
type TaskWithStats struct {
	model.Task
	LogCount     int64
	SuccessCount int64
	FailureCount int64
}

// -- 6. listOwnerTasksWithStats --
// ListOwnerTasksWithStats returns an owner's tasks joined with aggregated log counts.
// A LEFT JOIN is used so tasks with no logs still appear (log_count defaults to 0).
func ListOwnerTasksWithStats(ctx context.Context, q pg.Querier, botID int64) ([]TaskWithStats, error) {
	// tb_task_logs always exists in the PG schema (001_schema.sql), so no existence guard is needed.
	rows, err := q.Query(ctx, `
		SELECT t.id, t.code, t.bot_id, t.title, t.requirements, t.postapi, t.budget, t.price,
		       t.pinned, t.status, t.review_note, t.created_at, t.updated_at, t.reviewed_at,
		       t.opened_at, t.closed_at,
		       COALESCE(ls.log_count, 0) AS log_count,
		       COALESCE(ls.success_count, 0) AS success_count,
		       COALESCE(ls.failure_count, 0) AS failure_count
		FROM tb_tasks t
		LEFT JOIN (
			SELECT task_code,
			       COUNT(*) AS log_count,
			       SUM(CASE WHEN action = 'post_succeeded' THEN 1 ELSE 0 END) AS success_count,
			       SUM(CASE WHEN success = FALSE THEN 1 ELSE 0 END) AS failure_count
			FROM tb_task_logs
			GROUP BY task_code
		) ls ON ls.task_code = t.code
		WHERE t.bot_id = $1
		ORDER BY t.created_at DESC`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TaskWithStats
	for rows.Next() {
		var tw TaskWithStats
		if err := scanTaskWithStats(&tw, rows); err != nil {
			return nil, err
		}
		out = append(out, tw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// -- 7. findOwnerTaskByCode --
// FindOwnerTaskByCode returns the owner's task with the given code, or nil if not found.
func FindOwnerTaskByCode(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Task, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, requirements, postapi, budget, price, pinned, status,
		       review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
		FROM tb_tasks
		WHERE code = $1 AND bot_id = $2`, code, botID)
	var t model.Task
	if err := scanTask(&t, row); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// -- 8. findTaskByCode --
// FindTaskByCode returns the task with the given code regardless of owner/status, or nil.
func FindTaskByCode(ctx context.Context, q pg.Querier, code string) (*model.Task, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, requirements, postapi, budget, price, pinned, status,
		       review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
		FROM tb_tasks
		WHERE code = $1`, code)
	var t model.Task
	if err := scanTask(&t, row); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// TaskBudgetStatus holds the id, budget, and status projection used by budget operations.
type TaskBudgetStatus struct {
	ID     int64
	Budget float64
	Status string
}

// -- 9. findTaskBudgetStatusByCode --
// FindTaskBudgetStatusByCode returns the id/budget/status projection for budget checks.
func FindTaskBudgetStatusByCode(ctx context.Context, q pg.Querier, code string) (*TaskBudgetStatus, error) {
	row := q.QueryRow(ctx, `
		SELECT id, budget, status
		FROM tb_tasks
		WHERE code = $1`, code)
	var bs TaskBudgetStatus
	if err := row.Scan(&bs.ID, &bs.Budget, &bs.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &bs, nil
}

// -- 10. findTaskBudgetStatusByCodeForUpdate --
// FindTaskBudgetStatusByCodeForUpdate is like FindTaskBudgetStatusByCode but locks the row (FOR UPDATE).
func FindTaskBudgetStatusByCodeForUpdate(ctx context.Context, q pg.Querier, code string) (*TaskBudgetStatus, error) {
	row := q.QueryRow(ctx, `
		SELECT id, budget, status
		FROM tb_tasks
		WHERE code = $1
		FOR UPDATE`, code)
	var bs TaskBudgetStatus
	if err := row.Scan(&bs.ID, &bs.Budget, &bs.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &bs, nil
}

// -- 11. findOwnerTaskByCodeForUpdate --
// FindOwnerTaskByCodeForUpdate returns the owner's task locked for update (FOR UPDATE).
func FindOwnerTaskByCodeForUpdate(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Task, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, title, requirements, postapi, budget, price, pinned, status,
		       review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
		FROM tb_tasks
		WHERE code = $1 AND bot_id = $2
		FOR UPDATE`, code, botID)
	var t model.Task
	if err := scanTask(&t, row); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// TaskLogEntry is the trimmed projection of tb_task_logs used in recent-log listings.
type TaskLogEntry struct {
	ID           int64
	BotID        *int64
	Action       string
	ResponseCode *int
	Success      bool
	ErrorCode    *string
	ErrorMessage *string
	CreatedAt    string
}

// -- 12. findRecentLogsByTaskCode --
// FindRecentLogsByTaskCode returns the most recent task-log entries for a task, newest first.
func FindRecentLogsByTaskCode(ctx context.Context, q pg.Querier, code string, limit int) ([]TaskLogEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT id, bot_id, action, response_code, success, error_code, error_message, created_at
		FROM tb_task_logs
		WHERE task_code = $1
		ORDER BY created_at DESC
		LIMIT $2`, code, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TaskLogEntry
	for rows.Next() {
		var e TaskLogEntry
		if err := rows.Scan(&e.ID, &e.BotID, &e.Action, &e.ResponseCode, &e.Success,
			&e.ErrorCode, &e.ErrorMessage, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// NewTaskInput holds the fields needed to insert a new task row.
type NewTaskInput struct {
	Code         string
	BotID        int64
	Title        string
	Requirements string
	PostAPI      *string
	Budget       float64
	Price        float64
	Pinned       bool
	Status       string
	OpenedAt     *string
}

// -- 13. insertTask --
// InsertTask inserts a new task row. The code must already be generated (see GenerateUniqueTaskCode).
func InsertTask(ctx context.Context, q pg.Querier, in NewTaskInput) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_tasks
		    (code, bot_id, title, requirements, postapi, budget, price, pinned, status, opened_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())`,
		in.Code, in.BotID, in.Title, in.Requirements, in.PostAPI,
		in.Budget, in.Price, in.Pinned, in.Status, in.OpenedAt)
	return err
}

// -- 14. openOwnerTask --
// OpenOwnerTask marks a task open: sets status, preserves the first opened_at, clears closed_at.
func OpenOwnerTask(ctx context.Context, q pg.Querier, botID int64, code string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET status = 'open', opened_at = COALESCE(opened_at, NOW()), closed_at = NULL, updated_at = NOW()
		WHERE code = $1 AND bot_id = $2`, code, botID)
	return err
}

// -- 15. closeOwnerTask --
// CloseOwnerTask marks a task closed and records closed_at.
func CloseOwnerTask(ctx context.Context, q pg.Querier, botID int64, code string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET status = 'closed', closed_at = NOW(), updated_at = NOW()
		WHERE code = $1 AND bot_id = $2`, code, botID)
	return err
}

// -- 16. addOwnerTaskBudget --
// AddOwnerTaskBudget increments a task's budget by the given amount.
func AddOwnerTaskBudget(ctx context.Context, q pg.Querier, botID int64, code string, amount float64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks SET budget = budget + $1, updated_at = NOW()
		WHERE code = $2 AND bot_id = $3`, amount, code, botID)
	return err
}

// -- 17. clearOwnerTaskBudget --
// ClearOwnerTaskBudget zeroes a task's budget.
func ClearOwnerTaskBudget(ctx context.Context, q pg.Querier, botID int64, code string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks SET budget = 0, updated_at = NOW()
		WHERE code = $1 AND bot_id = $2`, code, botID)
	return err
}

// -- 18. updateOwnerTaskBasics --
// UpdateOwnerTaskBasics updates the editable basics: title, requirements, postapi, price.
func UpdateOwnerTaskBasics(ctx context.Context, q pg.Querier, botID int64, code, title, requirements string, postAPI *string, price float64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET title = $1, requirements = $2, postapi = $3, price = $4, updated_at = NOW()
		WHERE code = $5 AND bot_id = $6`,
		title, requirements, postAPI, price, code, botID)
	return err
}

// -- 18b. updateTaskBudgetAndStatus --
// UpdateTaskBudgetAndStatus sets budget/status and conditionally stamps closed_at.
func UpdateTaskBudgetAndStatus(ctx context.Context, q pg.Querier, id int64, budget float64, status string, shouldClose bool) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET budget = $1,
		    status = $2,
		    closed_at = CASE WHEN $3 THEN NOW() ELSE closed_at END,
		    updated_at = NOW()
		WHERE id = $4`,
		budget, status, shouldClose, id)
	return err
}

// -- 18c. decrementTaskBudgetForDelivery --
// DecrementTaskBudgetForDelivery debits price from a task's budget and auto-closes the
// task if the resulting budget drops below minOpenBudget. PostgreSQL evaluates the RHS
// expression (budget - $1) once per reference, so it is safe to repeat in the CASE.
func DecrementTaskBudgetForDelivery(ctx context.Context, q pg.Querier, id int64, price float64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET budget = budget - $1,
		    status = CASE WHEN budget - $1 < $2 THEN 'closed' ELSE status END,
		    closed_at = CASE WHEN budget - $1 < $2 THEN NOW() ELSE closed_at END,
		    updated_at = NOW()
		WHERE id = $3`,
		price, minOpenBudget, id)
	return err
}

// GenerateUniqueTaskCode returns a unique 12-hex code not yet present in tb_tasks.
func GenerateUniqueTaskCode(ctx context.Context, q pg.Querier) (string, error) {
	return publiccode.GenerateUnique(func(code string) (bool, error) {
		var exists bool
		err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM tb_tasks WHERE code = $1)`, code).Scan(&exists)
		return exists, err
	})
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTaskWithStats scans all 19 columns (16 task + 3 stats) in a single call.
// pgx Rows.Scan can only be called once per row, so this replaces the broken
// two-step scan (scanTask + rows.Scan) that caused "Error listing tasks".
func scanTaskWithStats(tw *TaskWithStats, s rowScanner) error {
	var (
		botID      int32
		budget     pgtype.Numeric
		price      pgtype.Numeric
		createdAt  time.Time
		updatedAt  time.Time
		reviewedAt *time.Time
		openedAt   *time.Time
		closedAt   *time.Time
	)
	if err := s.Scan(
		&tw.ID, &tw.Code, &botID, &tw.Title, &tw.Requirements, &tw.PostAPI,
		&budget, &price, &tw.Pinned, &tw.Status, &tw.ReviewNote,
		&createdAt, &updatedAt, &reviewedAt, &openedAt, &closedAt,
		&tw.LogCount, &tw.SuccessCount, &tw.FailureCount,
	); err != nil {
		return err
	}

	tw.BotID = int64(botID)
	if bf, err := budget.Float64Value(); err == nil {
		tw.Budget = bf.Float64
	}
	if pf, err := price.Float64Value(); err == nil {
		tw.Price = pf.Float64
	}
	tw.CreatedAt = createdAt.Format("2006-01-02 15:04:05")
	tw.UpdatedAt = updatedAt.Format("2006-01-02 15:04:05")
	if closedAt != nil {
		s := closedAt.Format("2006-01-02 15:04:05")
		tw.ClosedAt = &s
	}
	if reviewedAt != nil {
		s := reviewedAt.Format("2006-01-02 15:04:05")
		tw.ReviewedAt = &s
	}
	if openedAt != nil {
		s := openedAt.Format("2006-01-02 15:04:05")
		tw.OpenedAt = &s
	}

	return nil
}

// scanTask scans all 16 columns of tb_tasks into *model.Task.
// Uses intermediate pgx types and converts to Go-native types.
func scanTask(t *model.Task, s rowScanner) error {
	var (
		botID      int32
		budget     pgtype.Numeric
		price      pgtype.Numeric
		createdAt  time.Time
		updatedAt  time.Time
		reviewedAt *time.Time
		openedAt   *time.Time
		closedAt   *time.Time
	)
	if err := s.Scan(
		&t.ID, &t.Code, &botID, &t.Title, &t.Requirements, &t.PostAPI,
		&budget, &price, &t.Pinned, &t.Status, &t.ReviewNote,
		&createdAt, &updatedAt, &reviewedAt, &openedAt, &closedAt,
	); err != nil {
		return err
	}

	t.BotID = int64(botID)
	if bf, err := budget.Float64Value(); err == nil {
		t.Budget = bf.Float64
	}
	if pf, err := price.Float64Value(); err == nil {
		t.Price = pf.Float64
	}
	t.CreatedAt = createdAt.Format("2006-01-02 15:04:05")
	t.UpdatedAt = updatedAt.Format("2006-01-02 15:04:05")
	if closedAt != nil {
		s := closedAt.Format("2006-01-02 15:04:05")
		t.ClosedAt = &s
	}
	if reviewedAt != nil {
		s := reviewedAt.Format("2006-01-02 15:04:05")
		t.ReviewedAt = &s
	}
	if openedAt != nil {
		s := openedAt.Format("2006-01-02 15:04:05")
		t.OpenedAt = &s
	}

	return nil
}

// HomepageTask holds the projection for the homepage task board.
type HomepageTask struct {
	Code         string
	Title        string
	Pinned       bool
	Requirements string
	Price        float64
	Budget       float64
	SuccessCount int64
}

// QueryHomepageTasks returns up to 8 open tasks for the homepage board.
func QueryHomepageTasks(ctx context.Context, q pg.Querier) ([]HomepageTask, error) {
	rows, err := q.Query(ctx, `
		SELECT t.code, t.title, t.pinned, t.requirements, t.price, t.budget,
		       COALESCE(ls.success_count, 0) AS success_count
		FROM tb_tasks t
		LEFT JOIN (
		    SELECT task_code, SUM(CASE WHEN action = 'post_succeeded' THEN 1 ELSE 0 END) AS success_count
		    FROM tb_task_logs
		    GROUP BY task_code
		) ls ON ls.task_code = t.code
		WHERE t.status = 'open' AND t.price > 0
		  AND t.budget >= 1000.0 AND t.budget >= t.price
		ORDER BY t.pinned DESC, t.created_at DESC
		LIMIT 8`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []HomepageTask
	for rows.Next() {
		var t HomepageTask
		var price, budget pgtype.Numeric
		if err := rows.Scan(&t.Code, &t.Title, &t.Pinned, &t.Requirements, &price, &budget, &t.SuccessCount); err != nil {
			return nil, err
		}
		t.Price = numericToFloat(price)
		t.Budget = numericToFloat(budget)
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
