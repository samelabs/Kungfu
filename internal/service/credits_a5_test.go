package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// A5 regression tests: registration genesis ledger + existing credit consumers
// all routed through the single credits primitive. Local PG via
// KF_TEST_DATABASE_URL.

func a5TestPool(t *testing.T) *pg.Pool {
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

func a5CleanupBot(t *testing.T, pool *pg.Pool, botID int64) {
	t.Helper()
	_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
}

// TestRegistrationGenesisLedger: after Register, the bot exists, balance=66,
// and tb_transactions has exactly one grant_signup row: +66 / balance_after 66.
func TestRegistrationGenesisLedger(t *testing.T) {
	pool := a5TestPool(t)
	suffix := time.Now().Format("150405.000000000")
	name := "a5reg_" + suffix
	password := "pw-" + suffix + "-secret"

	res, err := Register(context.Background(), pool, name, password, "127.0.0.1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.Balance != 0 {
		t.Fatalf("API balance = %d, want 0 (visible contract unchanged)", res.Balance)
	}

	ctx := context.Background()
	var botID int64
	var balance float64
	if err := pool.QueryRow(ctx,
		`SELECT id, balance::float8 FROM tb_bots WHERE bot_name = $1`, name).
		Scan(&botID, &balance); err != nil {
		t.Fatalf("bot not found: %v", err)
	}
	t.Cleanup(func() { a5CleanupBot(t, pool, botID) })

	if balance != 66.0 {
		t.Fatalf("DB balance = %v, want 66", balance)
	}

	var n int
	var amount, balanceAfter float64
	var txnType string
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), MIN(amount)::float8, MIN(balance_after)::float8, MIN(type)
		FROM tb_transactions WHERE bot_id = $1`, botID).
		Scan(&n, &amount, &balanceAfter, &txnType); err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	if n != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1 (grant_signup)", n)
	}
	if txnType != "grant_signup" {
		t.Fatalf("txn type = %s, want grant_signup", txnType)
	}
	if amount != 66.0 {
		t.Fatalf("amount = %v, want +66", amount)
	}
	if balanceAfter != 66.0 {
		t.Fatalf("balance_after = %v, want 66", balanceAfter)
	}
}

// TestKungfuCreateIsFree: storage is currently free — create books no
// spend_push, balance is unchanged, even at balance 0.
func TestKungfuCreateIsFree(t *testing.T) {
	pool := a5TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 0)

	res, err := Push(context.Background(), pool, botID, map[string]interface{}{
		"title":   "A5 Kungfu Free Test",
		"tags":    []interface{}{"test"},
		"content": strings.Repeat("x", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("Push at balance 0 must succeed (storage free): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE target_type='kungfu' AND target_id=$1`, res.Code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_kungfus WHERE code = $1`, res.Code)
		a5CleanupBot(t, pool, botID)
	})

	ctx := context.Background()
	var balance float64
	if err := pool.QueryRow(ctx, `SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		t.Fatalf("balance = %v, want 0 (create is free)", balance)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_push'`,
		botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("spend_push ledger rows = %d, want 0 (free)", n)
	}
}

// TestTaskPathStillUsesCreditsPrimitive: task creation locks budget through
// the same credits primitive (lock_task ledger row + balance decrement).
func TestTaskPathStillUsesCreditsPrimitive(t *testing.T) {
	pool := a5TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 2000)

	res, err := CreateTask(context.Background(), pool, botID, &OwnerTaskConfig{}, &CreateTaskInput{
		Title: "A5 Task Credit Test", Requirements: "req",
		PostAPI: "https://example.com/hook", Budget: 1500, Price: 1,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	code := res["task"].(map[string]interface{})["code"].(string)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_task_logs WHERE task_code=$1`, code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE target_type='task' AND target_id=$1`, code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_tasks WHERE code=$1`, code)
		a5CleanupBot(t, pool, botID)
	})

	ctx := context.Background()
	var balance float64
	if err := pool.QueryRow(ctx, `SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 500.0 {
		t.Fatalf("balance = %v, want 500 (2000 - 1500 locked)", balance)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='lock_task' AND amount=-1500 AND balance_after=500`,
		botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("lock_task ledger rows = %d, want 1", n)
	}
}

func a5TestBotWithBalance(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1, $2, 'x', 'active', $3) RETURNING id`,
		"a5bot_"+suffix, "kf_live_"+strings.ReplaceAll(suffix, ".", ""), balance,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	return botID
}

func apperrIs(err error) (*errors.AppError, bool) {
	return errors.IsAppError(err)
}
