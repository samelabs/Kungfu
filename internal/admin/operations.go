package admin

// Platform operations (014): dashboard, task governance (WO-8b reads;
// the close action runs through service.PlatformCloseTask) and memory
// governance. Reads are permission-gated; every mutation is audited in
// the same transaction (WithAuditTx).

import (
	"context"
	"fmt"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// Dashboard returns the platform counters. Any signed-in admin may see
// them; finance detail stays behind finance.read on its own page.
func Dashboard(ctx context.Context, pool *pg.Pool, principal *Principal) (*repository.AdminDashboardCounts, error) {
	if principal == nil || principal.Admin == nil {
		return nil, errors.New(401, "ADMIN_LOGIN_REQUIRED", "Admin login required")
	}
	c, err := repository.AdminDashboard(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return c, nil
}

// -- memories --

// ListMemories requires memories.read.
func ListMemories(ctx context.Context, pool *pg.Pool, principal *Principal, f repository.AdminMemoryFilter) ([]repository.AdminMemoryRow, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "memories.read"); err != nil {
		return nil, 0, err
	}
	rows, total, err := repository.AdminListMemories(ctx, pool, f)
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return rows, total, nil
}

// GetMemory requires memories.read.
func GetMemory(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*repository.AdminMemoryRow, error) {
	if err := RequirePermission(ctx, pool, principal, "memories.read"); err != nil {
		return nil, err
	}
	m, err := repository.AdminGetMemory(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if m == nil {
		return nil, errors.New(404, "NOT_FOUND", "Memory not found")
	}
	return m, nil
}

// UnshareMemory forces a memory private (idempotent).
func UnshareMemory(ctx context.Context, pool *pg.Pool, principal *Principal, code string) error {
	return memoryMutation(ctx, pool, principal, code, "memory.unshare", func(ctx context.Context, tx pg.Querier, m *repository.AdminMemoryRow) (map[string]interface{}, error) {
		if m.Visibility != "private" {
			if err := repository.UpdateKungfuVisibilityByID(ctx, tx, m.ID, "private"); err != nil {
				return nil, err
			}
		}
		return map[string]interface{}{"visibility": "private", "status": m.Status}, nil
	})
}

// RemoveMemory soft-deletes a memory (same mechanism as the owner's
// delete), and makes it private so a restore can never re-publish it.
func RemoveMemory(ctx context.Context, pool *pg.Pool, principal *Principal, code string) error {
	return memoryMutation(ctx, pool, principal, code, "memory.remove", func(ctx context.Context, tx pg.Querier, m *repository.AdminMemoryRow) (map[string]interface{}, error) {
		if m.Visibility != "private" {
			if err := repository.UpdateKungfuVisibilityByID(ctx, tx, m.ID, "private"); err != nil {
				return nil, err
			}
		}
		if m.Status != "deleted" {
			if err := repository.SoftDeleteKungfuByID(ctx, tx, m.ID); err != nil {
				return nil, err
			}
		}
		return map[string]interface{}{"visibility": "private", "status": "deleted"}, nil
	})
}

func memoryMutation(ctx context.Context, pool *pg.Pool, principal *Principal, code, action string,
	fn func(ctx context.Context, tx pg.Querier, m *repository.AdminMemoryRow) (map[string]interface{}, error)) error {
	if err := RequirePermission(ctx, pool, principal, "memories.manage"); err != nil {
		return err
	}
	entry := &AuditEntry{Actor: principal.Admin, Action: action, TargetType: "memory", TargetID: code, Success: true}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		m, err := repository.AdminLockMemory(ctx, tx, code)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if m == nil {
			return errors.New(404, "NOT_FOUND", "Memory not found")
		}
		entry.Before = map[string]interface{}{"visibility": m.Visibility, "status": m.Status, "owner_bot_id": m.BotID, "title": m.Title}
		after, err := fn(ctx, tx, m)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		after["owner_bot_id"], after["title"] = m.BotID, m.Title
		entry.After = after
		return nil
	})
}

// -- tasks (WO-8b) --

// ListPlatformTasks requires tasks.read. The close action itself runs
// through service.PlatformCloseTask (the §4 kernel); the admin plane
// only reads and authorizes here — internal/admin must not import
// internal/task or internal/service (architecture guard). The view
// types below are admin-owned projections so the server handlers
// never touch repository types directly (server → admin → domain).

// TaskRow is the admin view of one task.
type TaskRow struct {
	ID            int64
	Code          string
	PublisherID   int64
	PublisherName string
	Status        string
	Version       int32
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
	Contract      []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TaskStats is the admin view of the §6.3 30-day statistics.
type TaskStats struct {
	Settled            int64
	Rejected           int64
	Failed             int64
	TerminalTotal      int64
	MedianReplySeconds *float64
}

// SubmissionFact is the admin view of one recent submission (no
// payload bodies cross into the console).
type SubmissionFact struct {
	SubmissionID int64
	Version      int32
	AgentID      int64
	Amount       int64
	State        string
	Failure      *string
	CreatedAt    time.Time
}

// TaskDetailView is the admin task detail: the task row with its
// effective contract, the 30-day statistics, per-state submission
// counts and the most recent submissions. Rates and the median are
// preformatted for the console templates ("—" when the denominator
// is 0).
type TaskDetailView struct {
	Task          *TaskRow
	Stats         TaskStats
	Counts        map[string]int64
	Submissions   []SubmissionFact
	AcceptRate    string
	FailureRate   string
	MedianSeconds string
}

// ReportRow is the admin view of one task report.
type ReportRow struct {
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

func adminTaskView(t *repository.AdminTaskRow) *TaskRow {
	return &TaskRow{
		ID: t.ID, Code: t.Code, PublisherID: t.PublisherID, PublisherName: t.PublisherName,
		Status: t.Status, Version: t.Version,
		BudgetLocked: t.BudgetLocked, Settled: t.Settled, Reserved: t.Reserved, Refunded: t.Refunded,
		Available: t.Available, Slots: t.Slots,
		PausedReason: t.PausedReason, ClosedReason: t.ClosedReason,
		Title: t.Title, Price: t.Price, Contract: t.Contract,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func adminTaskStats(s repository.TaskStats) TaskStats {
	return TaskStats{
		Settled: s.Settled, Rejected: s.Rejected, Failed: s.Failed,
		TerminalTotal:      s.TerminalTotal,
		MedianReplySeconds: s.MedianReplySeconds,
	}
}

func adminReportView(r *repository.AdminReportRow) *ReportRow {
	return &ReportRow{
		ID: r.ID, TaskID: r.TaskID, TaskCode: r.TaskCode, TaskStatus: r.TaskStatus,
		ReporterID: r.ReporterID, ReporterName: r.ReporterName,
		Reason: r.Reason, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// TaskFilter carries the admin task list parameters.
type TaskFilter struct {
	Status string // "" (all) | draft | open | paused | closed
	Page   int
	Size   int
}

// ReportFilter carries the admin report queue parameters.
type ReportFilter struct {
	Status string // "" (all) | open | dismissed | actioned
	Page   int
	Size   int
}

// ListPlatformTasks requires tasks.read.
func ListPlatformTasks(ctx context.Context, pool *pg.Pool, principal *Principal, f TaskFilter) ([]TaskRow, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "tasks.read"); err != nil {
		return nil, 0, err
	}
	rows, total, err := repository.AdminListTasks(ctx, pool, repository.AdminTaskFilter{
		Status: f.Status, Page: f.Page, Size: f.Size,
	})
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	out := make([]TaskRow, 0, len(rows))
	for i := range rows {
		out = append(out, *adminTaskView(&rows[i]))
	}
	return out, total, nil
}

// statsWindow is the §6.3 statistic window (30 days) — the same
// measure the executor's work_list applies.
const statsWindow = 30 * 24 * time.Hour

// GetPlatformTask requires tasks.read and assembles the detail view.
func GetPlatformTask(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*TaskDetailView, error) {
	if err := RequirePermission(ctx, pool, principal, "tasks.read"); err != nil {
		return nil, err
	}
	t, err := repository.AdminGetTask(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	if t == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}
	d := &TaskDetailView{Task: adminTaskView(t)}
	stats, err := repository.GetTaskStats(ctx, pool, t.ID, time.Now().Add(-statsWindow))
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	d.Stats = adminTaskStats(stats)
	if d.Counts, err = repository.CountTaskSubmissionsByState(ctx, pool, t.ID); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	subs, err := repository.ListTaskSubmissions(ctx, pool, t.ID, "", 10, 0)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	d.Submissions = make([]SubmissionFact, 0, len(subs))
	for i := range subs {
		d.Submissions = append(d.Submissions, SubmissionFact{
			SubmissionID: subs[i].SubmissionID, AgentID: subs[i].AgentID, Amount: subs[i].Amount,
			State: subs[i].State, Failure: subs[i].Failure, CreatedAt: subs[i].CreatedAt,
		})
	}
	pct := func(num, den int64) string {
		if den == 0 {
			return "—"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(num)/float64(den))
	}
	d.AcceptRate = pct(d.Stats.Settled, d.Stats.Settled+d.Stats.Rejected)
	d.FailureRate = pct(d.Stats.Failed, d.Stats.TerminalTotal)
	if d.Stats.MedianReplySeconds != nil {
		d.MedianSeconds = fmt.Sprintf("%.0fs", *d.Stats.MedianReplySeconds)
	} else {
		d.MedianSeconds = "—"
	}
	return d, nil
}

// -- reports (WO-8b) --

// ListPlatformReports requires reports.manage: the queue is the
// triage surface — reading it and acting on it share one permission.
func ListPlatformReports(ctx context.Context, pool *pg.Pool, principal *Principal, f ReportFilter) ([]ReportRow, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "reports.manage"); err != nil {
		return nil, 0, err
	}
	rows, total, err := repository.AdminListReports(ctx, pool, repository.AdminReportFilter{
		Status: f.Status, Page: f.Page, Size: f.Size,
	})
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	out := make([]ReportRow, 0, len(rows))
	for i := range rows {
		out = append(out, *adminReportView(&rows[i]))
	}
	return out, total, nil
}
