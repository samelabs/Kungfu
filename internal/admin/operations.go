package admin

// Platform operations (014): dashboard and memory governance. Reads
// are permission-gated; every mutation is audited in the same
// transaction (WithAuditTx). Task governance pages are removed with
// the v1 task model and return with the Task 1.0 console (WO-8).

import (
	"context"

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
