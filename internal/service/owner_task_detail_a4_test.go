package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// A4 regression tests: owner task detail read model.
// 1) detail stats equal list stats (real aggregation, not hardcoded 0);
// 2) task log timestamps scan correctly (PG timestamp -> "2006-01-02 15:04:05");
// 3) recent logs stay newest-first;
// 4) log query failure propagates as 500 INTERNAL_ERROR (cancelled ctx).

func a4TestPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func a4TestBot(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1, $2, 'x', 'active', 5000) RETURNING id`,
		"a4det_"+suffix, "kf_live_"+strings.ReplaceAll(suffix, ".", ""),
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

// a4SeedTask creates one task via the service and inserts log rows directly.
func a4SeedTask(t *testing.T, pool *pg.Pool, botID int64) string {
	t.Helper()
	res, err := CreateTask(context.Background(), pool, botID, &OwnerTaskConfig{}, &CreateTaskInput{
		Title:        "A4 Detail Task",
		Requirements: "requirements body",
		PostAPI:      "https://example.com/hook",
		Budget:       1000,
		Price:        1,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	code := res["task"].(map[string]interface{})["code"].(string)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_task_logs WHERE task_code = $1`, code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_tasks WHERE code = $1`, code)
	})
	return code
}

func a4InsertLog(t *testing.T, pool *pg.Pool, code string, action string, success bool, ageSecs int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO tb_task_logs (task_code, bot_id, action, success, created_at)
		VALUES ($1, NULL, $2, $3, NOW() - make_interval(secs => $4::int))`,
		code, action, success, ageSecs)
	if err != nil {
		t.Fatalf("insert log (%s): %v", action, err)
	}
}

// TestGetTaskStatsMatchListStats: detail stats are real and identical to list.
func TestGetTaskStatsMatchListStats(t *testing.T) {
	pool := a4TestPool(t)
	botID := a4TestBot(t, pool)
	ctx := context.Background()
	code := a4SeedTask(t, pool, botID)

	// 2 post_succeeded + 1 failure + 1 other
	a4InsertLog(t, pool, code, "post_succeeded", true, 10)
	a4InsertLog(t, pool, code, "post_succeeded", true, 20)
	a4InsertLog(t, pool, code, "post_failed", false, 30)
	a4InsertLog(t, pool, code, "kfcheck", true, 40)

	detail, err := GetTask(ctx, pool, botID, code)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	taskMap := detail["task"].(map[string]interface{})
	if got := taskMap["log_count"]; got != int64(4) {
		t.Fatalf("detail log_count = %v, want 4", got)
	}
	if got := taskMap["success_count"]; got != int64(2) {
		t.Fatalf("detail success_count = %v, want 2", got)
	}
	if got := taskMap["failure_count"]; got != int64(1) {
		t.Fatalf("detail failure_count = %v, want 1", got)
	}

	list, err := ListTasks(ctx, pool, botID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, tm := range list["tasks"].([]map[string]interface{}) {
		if tm["code"] == code {
			if tm["log_count"] != taskMap["log_count"] ||
				tm["success_count"] != taskMap["success_count"] ||
				tm["failure_count"] != taskMap["failure_count"] {
				t.Fatalf("list/detail stats mismatch: list=%v detail=%v",
					[]interface{}{tm["log_count"], tm["success_count"], tm["failure_count"]},
					[]interface{}{taskMap["log_count"], taskMap["success_count"], taskMap["failure_count"]})
			}
			return
		}
	}
	t.Fatalf("task %s not found in list", code)
}

// TestGetTaskLogTimestampsAndOrder: timestamps come back in the existing API
// format and newest-first ordering is preserved.
func TestGetTaskLogTimestampsAndOrder(t *testing.T) {
	pool := a4TestPool(t)
	botID := a4TestBot(t, pool)
	ctx := context.Background()
	code := a4SeedTask(t, pool, botID)

	a4InsertLog(t, pool, code, "post_succeeded", true, 5)
	a4InsertLog(t, pool, code, "post_failed", false, 100)
	a4InsertLog(t, pool, code, "kfcheck", true, 200)

	detail, err := GetTask(ctx, pool, botID, code)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	logs := detail["logs"].([]map[string]interface{})
	if len(logs) != 3 {
		t.Fatalf("logs len = %d, want 3", len(logs))
	}
	layout := "2006-01-02 15:04:05"
	var prev time.Time
	for i, l := range logs {
		createdAt, ok := l["created_at"].(string)
		if !ok {
			t.Fatalf("log %d created_at not a string: %T", i, l["created_at"])
		}
		ts, err := time.Parse(layout, createdAt)
		if err != nil {
			t.Fatalf("log %d created_at %q not in expected format: %v", i, createdAt, err)
		}
		if i > 0 && ts.After(prev) {
			t.Fatalf("logs not newest-first: %s after %s", createdAt, prev.Format(layout))
		}
		prev = ts
	}
	// Newest (age 5s) must be first.
	first := logs[0]["action"].(string)
	if first != "Delivery accepted" { // post_succeeded label
		t.Fatalf("first log action = %q, want newest (post_succeeded)", first)
	}
}

// TestGetTaskTaskNotFound: unknown code -> 404 (not 500).
func TestGetTaskTaskNotFound(t *testing.T) {
	pool := a4TestPool(t)
	botID := a4TestBot(t, pool)
	_, err := GetTask(context.Background(), pool, botID, "nonexistent0")
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 404 || ae.Code != "NOT_FOUND" {
		t.Fatalf("want 404 NOT_FOUND, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// failLogsQuerier wraps a Querier and fails only the tb_task_logs query,
// letting the task-stats phase succeed (two-phase failure isolation; no schema
// mutation, parallel-safe). GetTask accepts pg.Querier for exactly this seam.
type failLogsQuerier struct {
	pg.Querier
}

func (f failLogsQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	// The stats join also mentions tb_task_logs; target only the recent-logs
	// read (phase 2) by its ORDER BY shape.
	if strings.Contains(sql, "tb_task_logs") && strings.Contains(sql, "ORDER BY created_at DESC") {
		return nil, errA4LogQuery
	}
	return f.Querier.Query(ctx, sql, args...)
}

var errA4LogQuery = fmt.Errorf("a4: task log query failure")

// TestGetTaskLogQueryFailureIsInternal: the log query fails after the task
// query succeeded -> 500 INTERNAL_ERROR with the dedicated message, never a
// silent empty logs list or a partial detail payload.
func TestGetTaskLogQueryFailureIsInternal(t *testing.T) {
	pool := a4TestPool(t)
	botID := a4TestBot(t, pool)
	code := a4SeedTask(t, pool, botID)
	a4InsertLog(t, pool, code, "post_succeeded", true, 1)

	_, err := GetTask(context.Background(), failLogsQuerier{Querier: pool}, botID, code)
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
	if ae.Message != "Error retrieving task logs" {
		t.Fatalf("message = %q, want %q", ae.Message, "Error retrieving task logs")
	}
}

// TestGetTaskTaskQueryFailureIsInternal: the task query itself fails
// (cancelled context before any query runs) -> 500 INTERNAL_ERROR.
func TestGetTaskTaskQueryFailureIsInternal(t *testing.T) {
	pool := a4TestPool(t)
	botID := a4TestBot(t, pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := GetTask(ctx, pool, botID, "anycode12345")
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
}
