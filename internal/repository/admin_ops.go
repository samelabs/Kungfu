package repository

// Platform operations primitives for the admin plane: dashboard
// counters, task governance (list/detail/close/pin) and memory
// governance (list/detail/unshare/remove). internal/admin calls these;
// it never writes SQL itself.
//
// Task governance never moves money: closing reuses the owner close
// (status only), and the owner keeps the normal refund path.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
)

// -- dashboard --

// AdminDashboardCounts are the platform-wide operational counters.
type AdminDashboardCounts struct {
	Accounts              int64
	AccountsDisabled      int64
	AccountsNew7d         int64
	TasksOpen             int64
	TasksPending          int64
	TasksClosed           int64
	SubmissionsSettled7d  int64
	SubmissionsInFlight   int64
	SubmissionsRejected7d int64
	Memories              int64
	MemoriesPublic        int64
	CreditsOutstanding    int64
	CreditsEarned7d       int64
	RedemptionsPending    int64
}

// AdminDashboard computes the counters in one round trip.
func AdminDashboard(ctx context.Context, q pg.Querier) (*AdminDashboardCounts, error) {
	var c AdminDashboardCounts
	err := q.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM tb_bots),
		  (SELECT COUNT(*) FROM tb_bots WHERE status <> 'active'),
		  (SELECT COUNT(*) FROM tb_bots WHERE created_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_tasks WHERE status = 'open'),
		  (SELECT COUNT(*) FROM tb_tasks WHERE status = 'pending'),
		  (SELECT COUNT(*) FROM tb_tasks WHERE status = 'closed'),
		  (SELECT COUNT(*) FROM tb_task_submissions WHERE kind = 'agent' AND state = 'settled' AND settled_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_task_submissions WHERE state IN ('reserved','delivering','delivered','uncertain')),
		  (SELECT COUNT(*) FROM tb_task_submissions WHERE kind = 'agent' AND state = 'rejected' AND updated_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_kungfus WHERE status = 'active'),
		  (SELECT COUNT(*) FROM tb_kungfus WHERE status = 'active' AND visibility = 'public'),
		  (SELECT COALESCE(SUM(balance), 0) FROM tb_bots),
		  (SELECT COALESCE(SUM(amount), 0) FROM tb_transactions WHERE type = 'earn_task' AND created_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_redemptions WHERE status = 'pending_review')`).Scan(
		&c.Accounts, &c.AccountsDisabled, &c.AccountsNew7d,
		&c.TasksOpen, &c.TasksPending, &c.TasksClosed,
		&c.SubmissionsSettled7d, &c.SubmissionsInFlight, &c.SubmissionsRejected7d,
		&c.Memories, &c.MemoriesPublic,
		&c.CreditsOutstanding, &c.CreditsEarned7d, &c.RedemptionsPending)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// -- tasks --

// AdminTaskRow is the admin projection of a task.
type AdminTaskRow struct {
	ID             int64
	Code           string
	BotID          int64
	OwnerName      string
	Title          string
	Requirements   string
	PostAPI        string
	Budget         int64
	ReservedBudget int64
	Price          int64
	Pinned         bool
	Status         string
	ReviewNote     string
	Completed      int64
	Submissions    int64
	CreatedAt      time.Time
	OpenedAt       *time.Time
	ClosedAt       *time.Time
	ReviewedAt     *time.Time
}

// AdminTaskFilter carries the task list parameters.
type AdminTaskFilter struct {
	Status   string // "" | pending | open | closed
	Q        string // code / title substring
	BotID    int64
	Pinned   bool
	Page     int
	PageSize int
}

// Completed uses the same authority as the public Completed figure:
// earn_task transactions (legacy ref_type='task' + durable submissions).
const adminTaskSelect = `
	SELECT t.id, t.code, t.bot_id, b.bot_name, t.title, t.requirements, COALESCE(t.postapi, ''),
	       t.budget, t.reserved_budget, t.price, t.pinned, t.status, COALESCE(t.review_note, ''),
	       (SELECT COUNT(*) FROM tb_transactions x WHERE x.type = 'earn_task' AND x.ref_type = 'task' AND x.ref_id = t.code)
	     + (SELECT COUNT(*) FROM tb_transactions x JOIN tb_task_submissions s ON s.code = x.ref_id
	         WHERE x.type = 'earn_task' AND x.ref_type = 'task_submission' AND s.task_id = t.id),
	       (SELECT COUNT(*) FROM tb_task_submissions s WHERE s.task_id = t.id AND s.kind = 'agent'),
	       t.created_at, t.opened_at, t.closed_at, t.reviewed_at
	FROM tb_tasks t JOIN tb_bots b ON b.id = t.bot_id`

func scanAdminTask(row pgx.Row) (*AdminTaskRow, error) {
	var t AdminTaskRow
	err := row.Scan(&t.ID, &t.Code, &t.BotID, &t.OwnerName, &t.Title, &t.Requirements, &t.PostAPI,
		&t.Budget, &t.ReservedBudget, &t.Price, &t.Pinned, &t.Status, &t.ReviewNote,
		&t.Completed, &t.Submissions, &t.CreatedAt, &t.OpenedAt, &t.ClosedAt, &t.ReviewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func normPage(page, size int) (int, int) {
	if size <= 0 || size > 200 {
		size = 50
	}
	if page < 1 {
		page = 1
	}
	return page, size
}

// AdminListTasks lists tasks platform-wide, newest first (pinned first
// when filtering pinned).
func AdminListTasks(ctx context.Context, q pg.Querier, f AdminTaskFilter) ([]AdminTaskRow, int64, error) {
	page, size := normPage(f.Page, f.PageSize)
	where := []string{"TRUE"}
	args := []interface{}{}
	arg := func(v interface{}) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Status != "" {
		where = append(where, "t.status = "+arg(f.Status))
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		p := arg(s)
		where = append(where, "(t.code = "+p+" OR t.title ILIKE '%' || "+p+" || '%')")
	}
	if f.BotID > 0 {
		where = append(where, "t.bot_id = "+arg(f.BotID))
	}
	if f.Pinned {
		where = append(where, "t.pinned")
	}
	cond := strings.Join(where, " AND ")
	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_tasks t WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	lim, off := arg(size), arg((page-1)*size)
	rows, err := q.Query(ctx, adminTaskSelect+` WHERE `+cond+` ORDER BY t.created_at DESC, t.id DESC LIMIT `+lim+` OFFSET `+off, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AdminTaskRow
	for rows.Next() {
		t, err := scanAdminTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	return out, total, rows.Err()
}

// AdminGetTask returns one task by code (nil when absent).
func AdminGetTask(ctx context.Context, q pg.Querier, code string) (*AdminTaskRow, error) {
	return scanAdminTask(q.QueryRow(ctx, adminTaskSelect+` WHERE t.code = $1`, code))
}

// AdminLockTask locks a task row for a governance mutation.
func AdminLockTask(ctx context.Context, q pg.Querier, code string) (*AdminTaskRow, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM tb_tasks WHERE code = $1 FOR UPDATE`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return AdminGetTask(ctx, q, code)
}

// AdminSetTaskReview records the platform review note on a task.
func AdminSetTaskReview(ctx context.Context, q pg.Querier, id int64, note string) error {
	_, err := q.Exec(ctx, `UPDATE tb_tasks SET review_note = NULLIF($2, ''), reviewed_at = NOW(), updated_at = NOW() WHERE id = $1`, id, note)
	return err
}

// AdminSetTaskPinned toggles homepage pinning.
func AdminSetTaskPinned(ctx context.Context, q pg.Querier, id int64, pinned bool) error {
	_, err := q.Exec(ctx, `UPDATE tb_tasks SET pinned = $2, updated_at = NOW() WHERE id = $1`, id, pinned)
	return err
}

// AdminSubmissionRow is the admin projection of a task submission.
type AdminSubmissionRow struct {
	Code         string
	BotID        int64
	BotName      string
	Kind         string
	State        string
	Price        int64
	Attempts     int64
	ResponseCode *int64
	LastError    string
	CreatedAt    time.Time
	SettledAt    *time.Time
}

// AdminListTaskSubmissions returns the most recent submissions of a task.
func AdminListTaskSubmissions(ctx context.Context, q pg.Querier, taskID int64, limit int) ([]AdminSubmissionRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT s.code, s.bot_id, b.bot_name, s.kind, s.state, s.price_snapshot, s.attempt_count,
		       s.response_code, COALESCE(s.last_error_code, ''), s.created_at, s.settled_at
		FROM tb_task_submissions s JOIN tb_bots b ON b.id = s.bot_id
		WHERE s.task_id = $1
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminSubmissionRow
	for rows.Next() {
		var r AdminSubmissionRow
		var code *int32
		if err := rows.Scan(&r.Code, &r.BotID, &r.BotName, &r.Kind, &r.State, &r.Price, &r.Attempts,
			&code, &r.LastError, &r.CreatedAt, &r.SettledAt); err != nil {
			return nil, err
		}
		if code != nil {
			v := int64(*code)
			r.ResponseCode = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// -- memories --

// AdminMemoryRow is the admin projection of a memory (tb_kungfus).
type AdminMemoryRow struct {
	ID          int64
	Code        string
	BotID       int64
	OwnerName   string
	Title       string
	Tags        []string
	Description string
	Content     string
	Visibility  string
	Status      string
	Size        int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AdminMemoryFilter carries the memory list parameters.
type AdminMemoryFilter struct {
	Visibility string // "" | public | private
	Status     string // "" (active) | active | deleted | all
	Q          string
	BotID      int64
	Page       int
	PageSize   int
}

const adminMemorySelect = `
	SELECT k.id, k.code, k.bot_id, b.bot_name, k.title, k.tags_json, COALESCE(k.description, ''),
	       k.content, k.visibility, k.status, OCTET_LENGTH(k.content), k.created_at, k.updated_at
	FROM tb_kungfus k JOIN tb_bots b ON b.id = k.bot_id`

func scanAdminMemory(row pgx.Row) (*AdminMemoryRow, error) {
	var m AdminMemoryRow
	err := row.Scan(&m.ID, &m.Code, &m.BotID, &m.OwnerName, &m.Title, &m.Tags, &m.Description,
		&m.Content, &m.Visibility, &m.Status, &m.Size, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// AdminListMemories lists memories platform-wide, most recently updated first.
func AdminListMemories(ctx context.Context, q pg.Querier, f AdminMemoryFilter) ([]AdminMemoryRow, int64, error) {
	page, size := normPage(f.Page, f.PageSize)
	where := []string{}
	args := []interface{}{}
	arg := func(v interface{}) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	switch f.Status {
	case "all":
		where = append(where, "TRUE")
	case "deleted":
		where = append(where, "k.status = 'deleted'")
	default:
		where = append(where, "k.status = 'active'")
	}
	if f.Visibility == "public" || f.Visibility == "private" {
		where = append(where, "k.visibility = "+arg(f.Visibility))
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		p := arg(s)
		where = append(where, "(k.code = "+p+" OR k.title ILIKE '%' || "+p+" || '%')")
	}
	if f.BotID > 0 {
		where = append(where, "k.bot_id = "+arg(f.BotID))
	}
	cond := strings.Join(where, " AND ")
	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_kungfus k WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	lim, off := arg(size), arg((page-1)*size)
	rows, err := q.Query(ctx, adminMemorySelect+` WHERE `+cond+` ORDER BY k.updated_at DESC, k.id DESC LIMIT `+lim+` OFFSET `+off, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AdminMemoryRow
	for rows.Next() {
		m, err := scanAdminMemory(rows)
		if err != nil {
			return nil, 0, err
		}
		m.Content = "" // list view never carries bodies
		out = append(out, *m)
	}
	return out, total, rows.Err()
}

// AdminGetMemory returns one memory by code in any status.
func AdminGetMemory(ctx context.Context, q pg.Querier, code string) (*AdminMemoryRow, error) {
	return scanAdminMemory(q.QueryRow(ctx, adminMemorySelect+` WHERE k.code = $1`, code))
}

// AdminLockMemory locks a memory row for a governance mutation.
func AdminLockMemory(ctx context.Context, q pg.Querier, code string) (*AdminMemoryRow, error) {
	return scanAdminMemory(q.QueryRow(ctx, adminMemorySelect+` WHERE k.code = $1 FOR UPDATE OF k`, code))
}
