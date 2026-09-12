package service

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// A2 regression tests: Submit(taskCode, ...) owns the task business authority.
// Runs against the local dev PostgreSQL (KF_TEST_DATABASE_URL); outbound POST
// targets an httptest.Server so delivery is observable without external calls.

func a2TestPool(t *testing.T) *pg.Pool {
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

func a2TestBot(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1, $2, 'x', 'active', 0) RETURNING id`,
		"a2sub_"+suffix, "kf_live_"+strings.ReplaceAll(suffix, ".", ""),
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

// a2TestTask inserts a task and cleans it up.
func a2TestTask(t *testing.T, pool *pg.Pool, botID int64, status string, postapi string, price, budget float64) string {
	t.Helper()
	code := "a2" + strings.ReplaceAll(time.Now().Format("150405.0000"), ".", "")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO tb_tasks (code, bot_id, title, requirements, postapi, budget, price, status)
		 VALUES ($1, $2, 'A2 Test Task', 'req', $3, $4, $5, $6)`,
		code, botID, postapi, budget, price, status)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_tasks WHERE code = $1`, code)
	})
	return code
}

// postRecorder is an httptest.Server that records whether it was hit.
type postRecorder struct {
	server *httptest.Server
	hits   int
}

func newPostRecorder() *postRecorder {
	pr := &postRecorder{}
	pr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr.hits++
		w.WriteHeader(http.StatusOK)
	}))
	return pr
}

// TestSubmitTaskNotFound: no task row -> 404 NOT_FOUND, no outbound POST.
func TestSubmitTaskNotFound(t *testing.T) {
	pool := a2TestPool(t)
	botID := a2TestBot(t, pool)
	pr := newPostRecorder()
	t.Cleanup(pr.server.Close)

	_, err := Submit(context.Background(), pool, "nonexistent0", botID, map[string]interface{}{"a": 1})

	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 404 || ae.Code != "NOT_FOUND" {
		t.Fatalf("want 404 NOT_FOUND, got %d %s", ae.HTTPCode, ae.Code)
	}
	if pr.hits != 0 {
		t.Fatalf("outbound POST happened for missing task: %d", pr.hits)
	}
}

// TestSubmitTaskNotOpen: non-open task -> 409 TASK_NOT_OPEN, no outbound POST.
func TestSubmitTaskNotOpen(t *testing.T) {
	pool := a2TestPool(t)
	botID := a2TestBot(t, pool)
	pr := newPostRecorder()
	t.Cleanup(pr.server.Close)

	code := a2TestTask(t, pool, botID, "pending", pr.server.URL, 1.0, 10.0)

	_, err := Submit(context.Background(), pool, code, botID, map[string]interface{}{"a": 1})

	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 409 || ae.Code != "TASK_NOT_OPEN" {
		t.Fatalf("want 409 TASK_NOT_OPEN, got %d %s", ae.HTTPCode, ae.Code)
	}
	if pr.hits != 0 {
		t.Fatalf("outbound POST happened for non-open task: %d", pr.hits)
	}
}

// TestSubmitTaskDBLookupFailure: the initial FindTaskByCode fails -> 500
// INTERNAL_ERROR, NOT disguised as NOT_FOUND. Failure is injected by dropping
// the table privilege-free way: a prepared BAD SQL via a broken pool wrapper
// is not possible (Submit takes *pg.Pool), so we point the pool at a closed
// database connection target using an invalid task code length is not enough —
// instead we close the pool's underlying connections by connecting to a
// database that doesn't exist is overkill; simplest reliable failure: rename
// the table for the duration of the call.
func TestSubmitTaskDBLookupFailure(t *testing.T) {
	pool := a2TestPool(t)
	botID := a2TestBot(t, pool)
	pr := newPostRecorder()
	t.Cleanup(pr.server.Close)

	code := a2TestTask(t, pool, botID, "open", pr.server.URL, 1.0, 10.0)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE tb_tasks RENAME TO tb_tasks_a2broken`); err != nil {
		t.Fatalf("rename table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE tb_tasks_a2broken RENAME TO tb_tasks`)
		// restore original name cleanup ordering: a2TestTask cleanup deletes by code,
		// which requires the original table name.
	})

	_, err := Submit(ctx, pool, code, botID, map[string]interface{}{"a": 1})

	// restore before asserting so cleanup works
	if _, rerr := pool.Exec(ctx, `ALTER TABLE tb_tasks_a2broken RENAME TO tb_tasks`); rerr != nil {
		t.Fatalf("restore table: %v", rerr)
	}

	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
	if pr.hits != 0 {
		t.Fatalf("outbound POST happened despite DB failure: %d", pr.hits)
	}
}

// TestSubmitOpenTaskDeliversWithDBTaskFields: an open task flows into the
// existing delivery; the POST hits the task's postapi URL from the DB row.
func TestSubmitOpenTaskDeliversWithDBTaskFields(t *testing.T) {
	pool := a2TestPool(t)
	botID := a2TestBot(t, pool)
	pr := newPostRecorder()
	t.Cleanup(pr.server.Close)

	code := a2TestTask(t, pool, botID, "open", pr.server.URL, 2.0, 1000.0)

	result, err := Submit(context.Background(), pool, code, botID, map[string]interface{}{"a": 1})

	if err != nil {
		t.Fatalf("open task submit failed: %v", err)
	}
	if pr.hits != 1 {
		t.Fatalf("outbound POST count = %d, want 1 (URL must come from the DB task row)", pr.hits)
	}
	if result.TaskCode != code {
		t.Fatalf("result task code = %s, want %s", result.TaskCode, code)
	}
	// Reward price comes from the freshly queried DB row, not caller input.
	if result.Billing["reward"] != 2.0 {
		t.Fatalf("reward = %v, want 2.0 (price from DB row)", result.Billing["reward"])
	}
	// Settlement applied: budget decremented by price.
	var budget float64
	if err := pool.QueryRow(context.Background(),
		`SELECT budget FROM tb_tasks WHERE code = $1`, code).Scan(&budget); err != nil {
		t.Fatalf("read budget: %v", err)
	}
	if budget != 998.0 {
		t.Fatalf("budget = %v, want 998.0 (1000 - 2)", budget)
	}
}

// compile guard: stderrors kept for symmetry with sibling test files.
var _ = stderrors.New
