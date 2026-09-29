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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
)

// -- dashboard --

// AdminDashboardCounts are the platform-wide operational counters.
// Task counters read the Task 1.0 model (tb_task / tb_task_submission);
// the review-pending counter of the v1 model has no successor — tasks
// open without platform review.
type AdminDashboardCounts struct {
	Accounts              int64
	AccountsDisabled      int64
	AccountsNew7d         int64
	TasksOpen             int64
	TasksDraft            int64
	TasksPaused           int64
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
		  (SELECT COUNT(*) FROM tb_task WHERE status = 'open'),
		  (SELECT COUNT(*) FROM tb_task WHERE status = 'draft'),
		  (SELECT COUNT(*) FROM tb_task WHERE status = 'paused'),
		  (SELECT COUNT(*) FROM tb_task WHERE status = 'closed'),
		  (SELECT COUNT(*) FROM tb_task_submission WHERE state = 'settled' AND settled_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_task_submission WHERE state IN ('delivering', 'uncertain')),
		  (SELECT COUNT(*) FROM tb_task_submission WHERE state = 'rejected' AND updated_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_kungfus WHERE status = 'active'),
		  (SELECT COUNT(*) FROM tb_kungfus WHERE status = 'active' AND visibility = 'public'),
		  (SELECT COALESCE(SUM(balance), 0) FROM tb_bots),
		  (SELECT COALESCE(SUM(amount), 0) FROM tb_transactions WHERE type = 'earn_task' AND created_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_redemptions WHERE status = 'pending_review')`).Scan(
		&c.Accounts, &c.AccountsDisabled, &c.AccountsNew7d,
		&c.TasksOpen, &c.TasksDraft, &c.TasksPaused, &c.TasksClosed,
		&c.SubmissionsSettled7d, &c.SubmissionsInFlight, &c.SubmissionsRejected7d,
		&c.Memories, &c.MemoriesPublic,
		&c.CreditsOutstanding, &c.CreditsEarned7d, &c.RedemptionsPending)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// -- tasks (WO-8b) --

// AdminTaskFilter carries the admin task list parameters.
type AdminTaskFilter struct {
	Status string // "" (all) | draft | open | paused | closed
	Page   int
	Size   int
}

// AdminTaskRow is the admin projection of a task: the §2 identity and
// economic columns plus the title and unit price of the effective
// contract (the current version snapshot once one exists, else the
// draft — the same rule as the publisher's task view).
type AdminTaskRow struct {
	ID            int64
	Code          string
	PublisherID   int64
	PublisherName string
	Status        string
	BudgetLocked  int64
	Settled       int64
	Reserved      int64
	Refunded      int64
	Available     int64
	Slots         int64
	PausedReason  *string
	ClosedReason  *string
	Title         string
	Price         int64
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Contract is the effective contract JSON — filled by AdminGetTask
	// only; list rows never carry bodies.
	Contract []byte
}

const adminTaskColumns = `
	t.id, t.code, t.publisher_id, b.bot_name, t.status,
	t.budget_locked, t.settled, t.reserved, t.refunded,
	t.paused_reason, t.closed_reason,
	COALESCE(t.contract->>'title', ''),
	COALESCE((t.contract->>'price')::bigint, 0),
	t.created_at, t.updated_at`

const adminTaskFrom = `
	FROM tb_task t
	JOIN tb_bots b ON b.id = t.publisher_id
	`

func scanAdminTask(row pgx.Row) (*AdminTaskRow, error) {
	var t AdminTaskRow
	err := row.Scan(&t.ID, &t.Code, &t.PublisherID, &t.PublisherName, &t.Status,
		&t.BudgetLocked, &t.Settled, &t.Reserved, &t.Refunded,
		&t.PausedReason, &t.ClosedReason, &t.Title, &t.Price, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Available = t.BudgetLocked - t.Settled - t.Reserved - t.Refunded
	if t.Price > 0 {
		t.Slots = t.Available / t.Price
	}
	return &t, nil
}

// AdminListTasks lists tasks platform-wide, newest first, optionally
// filtered by status.
func AdminListTasks(ctx context.Context, q pg.Querier, f AdminTaskFilter) ([]AdminTaskRow, int64, error) {
	page, size := normPage(f.Page, f.Size)
	where := ""
	args := []interface{}{}
	if f.Status == "draft" || f.Status == "open" || f.Status == "paused" || f.Status == "closed" {
		where = " WHERE t.status = $1"
		args = append(args, f.Status)
	}
	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task t`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `SELECT `+adminTaskColumns+adminTaskFrom+where+
		fmt.Sprintf(` ORDER BY t.created_at DESC, t.id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		append(args, size, (page-1)*size)...)
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

// AdminGetTask loads one task by code with its effective contract
// (the current version snapshot once one exists, else the draft).
func AdminGetTask(ctx context.Context, q pg.Querier, code string) (*AdminTaskRow, error) {
	var t AdminTaskRow
	err := q.QueryRow(ctx, `SELECT `+adminTaskColumns+`,
		t.contract`+adminTaskFrom+` WHERE t.code = $1`, code).
		Scan(&t.ID, &t.Code, &t.PublisherID, &t.PublisherName, &t.Status,
			&t.BudgetLocked, &t.Settled, &t.Reserved, &t.Refunded,
			&t.PausedReason, &t.ClosedReason, &t.Title, &t.Price, &t.CreatedAt, &t.UpdatedAt,
			&t.Contract)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Available = t.BudgetLocked - t.Settled - t.Reserved - t.Refunded
	if t.Price > 0 {
		t.Slots = t.Available / t.Price
	}
	return &t, nil
}

// -- reports (WO-8b) --

// AdminReportRow is the admin projection of a task report with the
// task and reporter facts the queue needs.
type AdminReportRow struct {
	ID           int64
	TaskID       int64
	TaskCode     string
	TaskStatus   string
	ReporterID   int64
	ReporterName string
	Reason       string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AdminReportFilter carries the admin report queue parameters.
type AdminReportFilter struct {
	Status string // "" (all) | open | dismissed | actioned
	Page   int
	Size   int
}

const adminReportSelect = `
	SELECT r.id, r.task_id, t.code, t.status, r.reporter_id, b.bot_name,
	       r.reason, r.status, r.created_at, r.updated_at
	FROM tb_task_report r
	JOIN tb_task t ON t.id = r.task_id
	JOIN tb_bots b ON b.id = r.reporter_id`

func scanAdminReport(row pgx.Row) (*AdminReportRow, error) {
	var r AdminReportRow
	err := row.Scan(&r.ID, &r.TaskID, &r.TaskCode, &r.TaskStatus, &r.ReporterID, &r.ReporterName,
		&r.Reason, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AdminListReports lists reports newest first, optionally filtered by
// status. The queue defaults to open (WO-8b).
func AdminListReports(ctx context.Context, q pg.Querier, f AdminReportFilter) ([]AdminReportRow, int64, error) {
	page, size := normPage(f.Page, f.Size)
	where := ""
	args := []interface{}{}
	if f.Status == "open" || f.Status == "dismissed" || f.Status == "actioned" {
		where = " WHERE r.status = $1"
		args = append(args, f.Status)
	}
	var total int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task_report r`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, adminReportSelect+where+
		fmt.Sprintf(` ORDER BY r.created_at DESC, r.id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AdminReportRow
	for rows.Next() {
		r, err := scanAdminReport(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

// normPage clamps pagination before any LIMIT/OFFSET is built from
// it: page in [1, 10000], size in [1, 200]. The page cap also keeps
// (page-1)*size inside int64, so a huge page number can never spill
// into a negative OFFSET (P2-7).
func normPage(page, size int) (int, int) {
	if size <= 0 || size > 200 {
		size = 50
	}
	if page < 1 {
		page = 1
	}
	if page > 10000 {
		page = 10000
	}
	return page, size
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
