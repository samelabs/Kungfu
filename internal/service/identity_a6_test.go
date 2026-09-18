package service

import (
	"context"
	stderrors "errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// A6 regression tests: registration DB-error propagation + AccountOverview
// real-zero vs query-failure. Local PG via KF_TEST_DATABASE_URL.

func a6TestPool(t *testing.T) *pg.Pool {
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

func a6CleanupBot(t *testing.T, pool *pg.Pool, botID int64) {
	_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
}

// failNameExistsQuerier fails only the bot-name existence probe
// (SELECT ... FROM tb_bots WHERE bot_name = $1 ... LIMIT 1 shape), letting
// every other query pass. Parallel-safe: no schema mutation.
type failNameExistsQuerier struct{ pg.Querier }

var errA6NameExists = stderrors.New("a6: bot name exists query failure")

func (f failNameExistsQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "bot_name") && strings.Contains(sql, "tb_bots") {
		return failRow{err: errA6NameExists}
	}
	return f.Querier.QueryRow(ctx, sql, args...)
}

// failRow always errors on Scan.
type failRow struct{ err error }

func (r failRow) Scan(dest ...any) error { return r.err }

// failStatsQuerier fails the kungfu stats / platform task count queries
// (whichever substring matches), letting the account lookup pass.
type failStatsQuerier struct {
	pg.Querier
	failOn string
}

func (f failStatsQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, f.failOn) {
		return nil, stderrors.New("a6: stats query failure")
	}
	return f.Querier.Query(ctx, sql, args...)
}

func (f failStatsQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	// KungfuStatsByBotID may use QueryRow; route matching SQL to a failing row.
	if strings.Contains(sql, f.failOn) {
		return failRow{err: stderrors.New("a6: stats query failure")}
	}
	return f.Querier.QueryRow(ctx, sql, args...)
}

func (f failStatsQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.Querier.Exec(ctx, sql, args...)
}

// TestRegisterNameExistsDBError: the name-exists query fails -> 500
// INTERNAL_ERROR, no bot row, no grant_signup ledger. Injected through the
// nameExistsProbe seam (parallel-safe: no schema mutation).
func TestRegisterNameExistsDBError(t *testing.T) {
	pool := a6TestPool(t)
	suffix := time.Now().Format("150405.000000000")

	orig := nameExistsProbe
	nameExistsProbe = func(ctx context.Context, q pg.Querier, name string) (bool, error) {
		return false, errA6NameExists
	}
	t.Cleanup(func() { nameExistsProbe = orig })

	_, err := Register(context.Background(), pool, "a6reg_"+suffix, "pw-"+suffix+"-secret", "127.0.0.1")

	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}

	// No bot created, no genesis ledger.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_bots WHERE bot_name LIKE $1`, "a6reg\\_"+suffix).
		Scan(&n); err == nil && n != 0 {
		t.Fatalf("bot created despite name-exists failure: %d", n)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE type='grant_signup' AND bot_id IN
		 (SELECT id FROM tb_bots WHERE bot_name LIKE $1)`, "a6reg\\_"+suffix).
		Scan(&n); err == nil && n != 0 {
		t.Fatalf("grant_signup ledger written despite failure: %d", n)
	}
}

// TestRegisterNameTakenStill409: duplicate name keeps the 409 contract.
func TestRegisterNameTakenStill409(t *testing.T) {
	pool := a6TestPool(t)
	suffix := time.Now().Format("150405.000000000")
	name := "a6dup_" + suffix

	res, err := Register(context.Background(), pool, name, "pw-"+suffix+"-secret", "127.0.0.1")
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	_ = res
	var botID int64
	pool.QueryRow(context.Background(), `SELECT id FROM tb_bots WHERE bot_name=$1`, name).Scan(&botID)
	t.Cleanup(func() { a6CleanupBot(t, pool, botID) })

	_, err = Register(context.Background(), pool, name, "pw-other-secret-1", "127.0.0.1")
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 409 || ae.Code != "NAME_TAKEN" {
		t.Fatalf("want 409 NAME_TAKEN, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// TestAccountOverviewRealZero: fresh bot -> all stats genuinely 0 and returned.
func TestAccountOverviewRealZero(t *testing.T) {
	pool := a6TestPool(t)
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', 0) RETURNING id`,
		"a6ov_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", ""))).Scan(&botID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { a6CleanupBot(t, pool, botID) })

	res, err := AccountOverview(context.Background(), pool, botID)
	if err != nil {
		t.Fatalf("AccountOverview: %v", err)
	}
	stats := res["stats"].(map[string]interface{})
	if stats["kungfu_count"] != int64(0) || stats["public_kungfu_count"] != int64(0) || stats["platform_task_count"] != int64(0) {
		t.Fatalf("fresh bot stats not zero: %v", stats)
	}
}

// TestAccountOverviewKungfuStatsFailure: stats query fails -> 500, no partial.
func TestAccountOverviewKungfuStatsFailure(t *testing.T) {
	pool := a6TestPool(t)
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', 0) RETURNING id`,
		"a6ov_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", ""))).Scan(&botID)
	t.Cleanup(func() { a6CleanupBot(t, pool, botID) })

	_, err := AccountOverview(context.Background(), failStatsQuerier{Querier: pool, failOn: "tb_kungfus"}, botID)
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// TestAccountOverviewTaskCountFailure: task count query fails -> 500.
func TestAccountOverviewTaskCountFailure(t *testing.T) {
	pool := a6TestPool(t)
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', 0) RETURNING id`,
		"a6ov_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", ""))).Scan(&botID)
	t.Cleanup(func() { a6CleanupBot(t, pool, botID) })

	_, err := AccountOverview(context.Background(), failStatsQuerier{Querier: pool, failOn: "tb_tasks"}, botID)
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
}
