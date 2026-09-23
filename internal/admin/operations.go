package admin

// Platform operations (014): dashboard, task governance, memory
// governance. Reads are permission-gated; every mutation is audited in
// the same transaction (WithAuditTx). Task governance changes status and
// pinning only — it never touches Credits, and the owner keeps the
// normal refund path for a closed task.

import (
	"context"
	"strings"

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

// -- tasks --

var taskStatuses = map[string]bool{"": true, "pending": true, "open": true, "closed": true}

// ListTasks requires tasks.read.
func ListTasks(ctx context.Context, pool *pg.Pool, principal *Principal, f repository.AdminTaskFilter) ([]repository.AdminTaskRow, int64, error) {
	if err := RequirePermission(ctx, pool, principal, "tasks.read"); err != nil {
		return nil, 0, err
	}
	if !taskStatuses[f.Status] {
		return nil, 0, errors.New(400, "INVALID_STATUS", "status must be one of: pending, open, closed")
	}
	rows, total, err := repository.AdminListTasks(ctx, pool, f)
	if err != nil {
		return nil, 0, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return rows, total, nil
}

// TaskDetail is a task with its recent submissions.
type TaskDetail struct {
	Task        *repository.AdminTaskRow
	Submissions []repository.AdminSubmissionRow
}

// GetTask requires tasks.read.
func GetTask(ctx context.Context, pool *pg.Pool, principal *Principal, code string) (*TaskDetail, error) {
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
	subs, err := repository.AdminListTaskSubmissions(ctx, pool, t.ID, 50)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	return &TaskDetail{Task: t, Submissions: subs}, nil
}

func taskFacts(t *repository.AdminTaskRow) map[string]interface{} {
	return map[string]interface{}{
		"status": t.Status, "pinned": t.Pinned, "review_note": t.ReviewNote,
		"owner_bot_id": t.BotID, "budget": t.Budget, "reserved_budget": t.ReservedBudget,
	}
}

// CloseTask closes a task on platform authority with a reason the owner
// can see. Status-only: budget and reservations are untouched; accepted
// submissions still settle; the owner may refund after the cooldown.
func CloseTask(ctx context.Context, pool *pg.Pool, principal *Principal, code, reason string) error {
	if err := RequirePermission(ctx, pool, principal, "tasks.manage"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New(400, "REASON_REQUIRED", "Give the owner a reason for closing the task")
	}
	if len(reason) > 500 {
		return errors.New(400, "REASON_TOO_LONG", "Reason must be at most 500 characters")
	}
	entry := &AuditEntry{Actor: principal.Admin, Action: "task.close", TargetType: "task", TargetID: code, Success: true}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		t, err := repository.AdminLockTask(ctx, tx, code)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if t == nil {
			return errors.New(404, "NOT_FOUND", "Task not found")
		}
		if t.Status == "closed" {
			return errors.New(409, "TASK_ALREADY_CLOSED", "Task is already closed")
		}
		entry.Before = taskFacts(t)
		if err := repository.CloseOwnerTask(ctx, tx, t.BotID, t.Code); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if err := repository.AdminSetTaskReview(ctx, tx, t.ID, reason); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		after := taskFacts(t)
		after["status"], after["review_note"] = "closed", reason
		entry.After = after
		return nil
	})
}

// SetTaskPinned pins or unpins a task on the homepage board.
func SetTaskPinned(ctx context.Context, pool *pg.Pool, principal *Principal, code string, pinned bool) error {
	if err := RequirePermission(ctx, pool, principal, "tasks.manage"); err != nil {
		return err
	}
	action := "task.unpin"
	if pinned {
		action = "task.pin"
	}
	entry := &AuditEntry{Actor: principal.Admin, Action: action, TargetType: "task", TargetID: code, Success: true}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		t, err := repository.AdminLockTask(ctx, tx, code)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if t == nil {
			return errors.New(404, "NOT_FOUND", "Task not found")
		}
		entry.Before = map[string]interface{}{"pinned": t.Pinned}
		entry.After = map[string]interface{}{"pinned": pinned}
		if t.Pinned == pinned {
			return nil
		}
		if err := repository.AdminSetTaskPinned(ctx, tx, t.ID, pinned); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		return nil
	})
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
