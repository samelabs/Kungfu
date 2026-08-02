package service

import (
	"context"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// TaskBoardService provides task delivery testing for owners.
// It exposes open tasks to agents (list + single-task detail).

// ListOpenTasks returns all currently-open tasks for agents.
func ListOpenTasks(ctx context.Context, pool *pg.Pool) (map[string]interface{}, error) {
	rows, err := repository.ListOpenTasks(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing open tasks")
	}

	tasks := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		tasks = append(tasks, agentTaskDetail(&rows[i]))
	}

	total, _ := repository.CountOpenTasks(ctx, pool)

	return map[string]interface{}{
		"tasks": tasks,
		"meta": map[string]interface{}{
			"total":    total,
			"returned": len(tasks),
		},
	}, nil
}

// GetOpenTask returns a single open task by code.
func GetOpenTask(ctx context.Context, pool *pg.Pool, code string) (map[string]interface{}, error) {
	t, err := repository.FindOpenTaskByCode(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving task")
	}
	if t == nil {
		return nil, errors.New(404, "NOT_FOUND", "Task not found")
	}

	return map[string]interface{}{
		"task": agentTaskDetail(t),
	}, nil
}

// agentTaskDetail
// Output shape: {code, title, requirements, price, pinned(int), status, created_at, updated_at}
func agentTaskDetail(t *model.Task) map[string]interface{} {
	pinned := 0
	if t.Pinned {
		pinned = 1
	}
	return map[string]interface{}{
		"code":         t.Code,
		"title":        t.Title,
		"requirements": t.Requirements,
		"price":        t.Price,
		"pinned":       pinned,
		"status":       t.Status,
		"created_at":   t.CreatedAt,
		"updated_at":   t.UpdatedAt,
	}
}
