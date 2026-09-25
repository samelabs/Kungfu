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
// Task counters read the Task 1.0 model (tb_task / tb_task_submission);
// the review-pending counter of the v1 model has no successor — tasks
// open without platform review.
type AdminDashboardCounts struct {
	Accounts              int64
	AccountsDisabled      int64
	AccountsNew7d         int64
	TasksOpen             int64
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
		  (SELECT COUNT(*) FROM tb_task WHERE status = 'closed'),
		  (SELECT COUNT(*) FROM tb_task_submission WHERE state = 'settled' AND settled_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_task_submission WHERE state IN ('delivering', 'uncertain', 'under_review')),
		  (SELECT COUNT(*) FROM tb_task_submission WHERE state = 'rejected' AND updated_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_kungfus WHERE status = 'active'),
		  (SELECT COUNT(*) FROM tb_kungfus WHERE status = 'active' AND visibility = 'public'),
		  (SELECT COALESCE(SUM(balance), 0) FROM tb_bots),
		  (SELECT COALESCE(SUM(amount), 0) FROM tb_transactions WHERE type = 'earn_task' AND created_at >= NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM tb_redemptions WHERE status = 'pending_review')`).Scan(
		&c.Accounts, &c.AccountsDisabled, &c.AccountsNew7d,
		&c.TasksOpen, &c.TasksClosed,
		&c.SubmissionsSettled7d, &c.SubmissionsInFlight, &c.SubmissionsRejected7d,
		&c.Memories, &c.MemoriesPublic,
		&c.CreditsOutstanding, &c.CreditsEarned7d, &c.RedemptionsPending)
	if err != nil {
		return nil, err
	}
	return &c, nil
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
