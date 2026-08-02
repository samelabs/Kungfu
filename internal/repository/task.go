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

// TaskRepository mirrors PHP repositories/TaskRepository.php.
// Every method accepts a pg.Querier so it works with both *pgxpool.Pool and pgx.Tx.

// minOpenBudget mirrors PHP TaskUtils::MIN_OPEN_BUDGET.
const minOpenBudget = 1000.0

// openBudgetWhereClause mirrors PHP TaskUtils::openBudgetWhereClause($alias).
// It returns the SQL fragment scoped to the given table alias.
// PHP: "{$alias}.status = 'open' AND {$alias}.price > 0
//
//	AND {$alias}.budget >= 1000.0 AND {$alias}.budget >= {$alias}.price"
func openBudgetWhereClause(alias string) string {
	if alias == "" {
		return "status = 'open' AND price > 0 AND budget >= 1000.0 AND budget >= price"
	}
	return alias + ".status = 'open' AND " + alias + ".price > 0 AND " +
		alias + ".budget >= 1000.0 AND " + alias + ".budget >= " + alias + ".price"
}

// -- 1. countOpenTasks --
// PHP: SELECT COUNT(*) AS count FROM tb_tasks t WHERE {$openWhere}
func CountOpenTasks(ctx context.Context, q pg.Querier) (int64, error) {
	var count int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) AS count
		FROM tb_tasks t
		WHERE `+openBudgetWhereClause("t")).Scan(&count)
	return count, err
}

// -- 2. listOpenTasks --
// PHP: SELECT t.* FROM tb_tasks t WHERE {$openWhere} ORDER BY t.pinned DESC, t.created_at DESC
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
// PHP: SELECT t.* FROM tb_tasks t WHERE t.code = :code AND {$openWhere}
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
// PHP: SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = :table
// PostgreSQL: current_schema() instead of DATABASE().
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
// PHP: SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE()
//
//	AND table_name = :table AND column_name = :column
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
// PHP: LEFT JOIN tb_task_logs aggregated by task_code, plus a no-logs fallback.
func ListOwnerTasksWithStats(ctx context.Context, q pg.Querier, botID int64) ([]TaskWithStats, error) {
	// Use a single query with a LEFT JOIN; the PHP branching on tableExists was a
	// MySQL migration guard. tb_task_logs exists in the PG schema (001_schema.sql).
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
		if err := scanTask(&tw.Task, rows); err != nil {
			return nil, err
		}
		if err := rows.Scan(&tw.LogCount, &tw.SuccessCount, &tw.FailureCount); err != nil {
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
// PHP: SELECT code, bot_id, title, requirements, postapi, budget, price, pinned, status,
//
//	review_note, created_at, updated_at, reviewed_at, opened_at, closed_at
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
// PHP: SELECT * FROM tb_tasks WHERE code = :code
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
// PHP: SELECT id, budget, status FROM tb_tasks WHERE code = :code
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
// PHP: SELECT id, budget, status FROM tb_tasks WHERE code = :code FOR UPDATE
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
// PHP: SELECT ... FROM tb_tasks WHERE code = :code AND bot_id = :bot_id FOR UPDATE
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
// PHP: SELECT id, bot_id, action, response_code, success, error_code, error_message, created_at
//
//	FROM tb_task_logs WHERE task_code = :code ORDER BY created_at DESC LIMIT :limit
func FindRecentLogsByTaskCode(ctx context.Context, q pg.Querier, code string, limit int) ([]TaskLogEntry, error) {
	if limit <= 0 {
		limit = 50 // matches PHP default
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
// Mirrors the PHP insertTask([...]) array in OwnerTaskService.
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
// PHP: Database::insert('tb_tasks', $taskData)
// Code generation uses PublicCode::generateUnique('tb_tasks'); the code is passed in here.
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
// PHP: UPDATE tb_tasks SET status='open', opened_at=COALESCE(opened_at,NOW()), closed_at=NULL
//
//	WHERE code = :code AND bot_id = :bot_id
func OpenOwnerTask(ctx context.Context, q pg.Querier, botID int64, code string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET status = 'open', opened_at = COALESCE(opened_at, NOW()), closed_at = NULL, updated_at = NOW()
		WHERE code = $1 AND bot_id = $2`, code, botID)
	return err
}

// -- 15. closeOwnerTask --
// PHP: UPDATE tb_tasks SET status='closed', closed_at=NOW() WHERE code=:code AND bot_id=:bot_id
func CloseOwnerTask(ctx context.Context, q pg.Querier, botID int64, code string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET status = 'closed', closed_at = NOW(), updated_at = NOW()
		WHERE code = $1 AND bot_id = $2`, code, botID)
	return err
}

// -- 16. addOwnerTaskBudget --
// PHP: UPDATE tb_tasks SET budget = budget + :amount WHERE code=:code AND bot_id=:bot_id
func AddOwnerTaskBudget(ctx context.Context, q pg.Querier, botID int64, code string, amount float64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks SET budget = budget + $1, updated_at = NOW()
		WHERE code = $2 AND bot_id = $3`, amount, code, botID)
	return err
}

// -- 17. clearOwnerTaskBudget --
// PHP: UPDATE tb_tasks SET budget = 0 WHERE code=:code AND bot_id=:bot_id
func ClearOwnerTaskBudget(ctx context.Context, q pg.Querier, botID int64, code string) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks SET budget = 0, updated_at = NOW()
		WHERE code = $1 AND bot_id = $2`, code, botID)
	return err
}

// -- 18. updateOwnerTaskBasics --
// PHP: UPDATE tb_tasks SET title, requirements, postapi, price
func UpdateOwnerTaskBasics(ctx context.Context, q pg.Querier, botID int64, code, title, requirements string, postAPI *string, price float64) error {
	_, err := q.Exec(ctx, `
		UPDATE tb_tasks
		SET title = $1, requirements = $2, postapi = $3, price = $4, updated_at = NOW()
		WHERE code = $5 AND bot_id = $6`,
		title, requirements, postAPI, price, code, botID)
	return err
}

// -- 18b. updateTaskBudgetAndStatus --
// PHP: UPDATE tb_tasks SET budget=:budget, status=:status,
//
//	closed_at = CASE WHEN :should_close = 1 THEN NOW() ELSE closed_at END WHERE id = :id
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
// PHP: budget = budget - :price, then conditionally close if post-debit budget < minOpenBudget.
// PostgreSQL evaluates the RHS once (budget - $1), so we can reference it directly.
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

// GenerateUniqueTaskCode mirrors PHP PublicCode::generateUnique('tb_tasks').
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
// Mirrors the PHP index.tpl.php inline query.
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
