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

// Push update/create write-failure regression tests, run against the local
// dev PostgreSQL (allowed by the A1 audit round). Write failures are injected
// with a real DB CHECK constraint on tb_kungfus.title, so the repository
// statements themselves fail — no architecture changes, no seams in Push.
// Without the DB available these tests are skipped.

func pushTestPool(t *testing.T) *pg.Pool {
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

// withFailTitleConstraint adds a CHECK constraint that rejects the magic
// failure title, and removes it on cleanup.
func withFailTitleConstraint(t *testing.T, pool *pg.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE tb_kungfus ADD CONSTRAINT a1_write_fail_chk CHECK (title <> 'A1-FAILWRITE')`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE tb_kungfus DROP CONSTRAINT IF EXISTS a1_write_fail_chk`)
	})
}

func pushTestBot(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1, $2, 'x', 'active', 10) RETURNING id`,
		"a1push_"+suffix, "kf_live_"+strings.ReplaceAll(suffix, ".", ""),
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func pushInput(title, code string) map[string]interface{} {
	return map[string]interface{}{
		"code":    code,
		"title":   title,
		"tags":    []interface{}{"test"},
		"content": strings.Repeat("x", 60), // min 50 chars
	}
}

// TestPushUpdateRepositoryWriteFailure: the UPDATE tb_kungfus statement fails
// (DB constraint) -> Push returns 500 INTERNAL_ERROR, not an updated success.
func TestPushUpdateRepositoryWriteFailure(t *testing.T) {
	pool := pushTestPool(t)
	botID := pushTestBot(t, pool)

	// Seed one existing kungfu to update.
	created, err := Push(context.Background(), pool, botID, pushInput("Seed Title", ""),
		128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("seed push: %v", err)
	}

	withFailTitleConstraint(t, pool)

	_, err = Push(context.Background(), pool, botID, pushInput("A1-FAILWRITE", created.Code),
		128, 10, 24, 500, 102400)

	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// TestPushCreateRepositoryWriteFailure: the INSERT INTO tb_kungfus statement
// fails inside the create transaction (the consumption charge rides the
// same tx) -> Push returns 500 INTERNAL_ERROR and nothing commits: no
// kungfu row, no spend_push transaction, balance unchanged.
func TestPushCreateRepositoryWriteFailure(t *testing.T) {
	pool := pushTestPool(t)
	botID := pushTestBot(t, pool)

	balanceBefore := getTestBalance(t, pool, botID)

	withFailTitleConstraint(t, pool)

	_, err := Push(context.Background(), pool, botID, pushInput("A1-FAILWRITE", ""),
		128, 10, 24, 500, 102400)

	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}

	ctx := context.Background()
	// Transaction must not have committed: no kungfu row for this bot.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_kungfus WHERE bot_id = $1`, botID).Scan(&n); err != nil {
		t.Fatalf("count kungfus: %v", err)
	}
	if n != 0 {
		t.Fatalf("kungfu row committed despite InsertNewKungfu failure: %d", n)
	}
	// No spend_push transaction row either.
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1 AND type = 'spend_push'`, botID).
		Scan(&n); err != nil {
		t.Fatalf("count txns: %v", err)
	}
	if n != 0 {
		t.Fatalf("spend_push transaction committed despite failure: %d", n)
	}
	// Balance untouched — no half-written state.
	if got := getTestBalance(t, pool, botID); got != balanceBefore {
		t.Fatalf("balance changed on failed create: %v -> %v", balanceBefore, got)
	}
}

func getTestBalance(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id = $1`, botID).Scan(&b); err != nil {
		t.Fatalf("get balance: %v", err)
	}
	return b
}
