package service

import (
	"context"
	stderrors "errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// A7 regression tests: identity no longer carries balance; responses compose
// balance from the credits domain. Local PG via KF_TEST_DATABASE_URL.

func a7TestPool(t *testing.T) *pg.Pool {
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

func a7TestBot(t *testing.T, pool *pg.Pool, balance float64) (int64, string, string) {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	name := "a7id_" + suffix
	key := "kf_live_" + strings.ReplaceAll(suffix, ".", "")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1,$2,'x','active',$3) RETURNING id`, name, key, balance).Scan(&botID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, botID) })
	return botID, name, key
}

// failBalanceQuerier fails only balance reads (SELECT balance ... FROM tb_bots
// WHERE id =), letting identity queries pass. Parallel-safe.
type failBalanceQuerier struct{ pg.Querier }

var errA7Balance = stderrors.New("a7: balance read failure")

func (f failBalanceQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "SELECT balance") && strings.Contains(sql, "tb_bots") {
		return failRow{err: errA7Balance}
	}
	return f.Querier.QueryRow(ctx, sql, args...)
}

// TestAPIKeyAuthIndependentOfBalance: FindActiveBotByAPIKey returns identity
// and never reads the balance column (Bot.Balance stays zero even when the
// DB balance is 7).
func TestAPIKeyAuthIndependentOfBalance(t *testing.T) {
	pool := a7TestPool(t)
	botID, _, key := a7TestBot(t, pool, 7)
	_ = botID

	bot, err := repository.FindActiveBotByAPIKey(context.Background(), pool, key)
	if err != nil || bot == nil {
		t.Fatalf("auth lookup failed: %v", err)
	}
	if bot.Balance != 0 {
		t.Fatalf("identity lookup returned balance %v — identity must not carry balance", bot.Balance)
	}
}

// TestOwnerSessionAuthIndependentOfBalance: same for the session lookup.
func TestOwnerSessionAuthIndependentOfBalance(t *testing.T) {
	pool := a7TestPool(t)
	botID, _, _ := a7TestBot(t, pool, 7)

	bot, err := repository.FindOwnerSessionBotByID(context.Background(), pool, botID)
	if err != nil || bot == nil {
		t.Fatalf("session lookup failed: %v", err)
	}
	if bot.Balance != 0 {
		t.Fatalf("session identity returned balance %v", bot.Balance)
	}
}

// TestAccountOverviewComposesCreditsBalance: overview balance equals the real
// credits balance (42), not an identity snapshot.
func TestAccountOverviewComposesCreditsBalance(t *testing.T) {
	pool := a7TestPool(t)
	botID, _, _ := a7TestBot(t, pool, 42)

	res, err := AccountOverview(context.Background(), pool, botID)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if res["balance"] != 42.0 {
		t.Fatalf("overview balance = %v, want 42", res["balance"])
	}
}

// TestAccountOverviewBalanceFailureIs500: balance read fails -> 500, not fake 0.
func TestAccountOverviewBalanceFailureIs500(t *testing.T) {
	pool := a7TestPool(t)
	botID, _, _ := a7TestBot(t, pool, 42)

	_, err := AccountOverview(context.Background(), failBalanceQuerier{Querier: pool}, botID)
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// TestCurrentOwnerKeyComposesCreditsBalance.
func TestCurrentOwnerKeyComposesCreditsBalance(t *testing.T) {
	pool := a7TestPool(t)
	botID, _, key := a7TestBot(t, pool, 13)
	_ = key

	res, err := CurrentOwnerKey(context.Background(), pool, botID)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if res["balance"] != 13.0 {
		t.Fatalf("key balance = %v, want 13", res["balance"])
	}

	// failure path
	_, err = CurrentOwnerKey(context.Background(), failBalanceQuerier{Querier: pool}, botID)
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("want 500 INTERNAL_ERROR on balance failure, got %v", err)
	}
}

// TestKungfuListAndGetCarryNoBalance: the storage contract no longer
// carries balance — push/list/get responses have no balance key, and list
// works even when balance reads fail entirely (pure storage projection).
func TestKungfuListAndGetCarryNoBalance(t *testing.T) {
	pool := a7TestPool(t)
	botID, _, _ := a7TestBot(t, pool, 5)

	pushed, err := Push(context.Background(), pool, botID, map[string]interface{}{
		"title": "A7 Compose Test", "tags": []interface{}{"t"},
		"content": strings.Repeat("y", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE target_type='kungfu' AND target_id=$1`, pushed.Code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_kungfus WHERE code=$1`, pushed.Code)
	})

	// Push consumed 1 credit via consumption despite no balance in result.
	var balance float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 4.0 {
		t.Fatalf("balance = %v, want 4 (5 - 1 storage.create)", balance)
	}

	list, err := ListKungfusForBot(context.Background(), pool, botID, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, has := list["balance"]; has {
		t.Fatal("list response carries balance — storage contract must not")
	}

	// Owner reads own kungfu: no charge, no balance key.
	detail, err := GetKungfuForBot(context.Background(), pool, botID, pushed.Code)
	if err != nil {
		t.Fatalf("owner get: %v", err)
	}
	if _, has := detail["balance"]; has {
		t.Fatal("owner get response carries balance — storage contract must not")
	}
	if balance != 4.0 {
		t.Fatalf("owner get charged: balance = %v, want 4", balance)
	}

	// List survives a total balance-read failure (credits outage does not
	// take storage down).
	if _, err := ListKungfusForBot(context.Background(), failBalanceQuerier{Querier: pool}, botID, 10, 0); err != nil {
		t.Fatalf("list must not depend on credits: %v", err)
	}
}

// TestPublicKungfuGetCharges: non-owner get spends 1 credit via
// consumption, returns the kungfu without a balance key.
func TestPublicKungfuGetCharges(t *testing.T) {
	pool := a7TestPool(t)
	ownerID, _, _ := a7TestBot(t, pool, 5)
	readerID, _, _ := a7TestBot(t, pool, 3)

	pushed, err := Push(context.Background(), pool, ownerID, map[string]interface{}{
		"title": "A7 Public Get", "tags": []interface{}{"t"},
		"content": strings.Repeat("z", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE target_type='kungfu' AND target_id=$1`, pushed.Code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_kungfus WHERE code=$1`, pushed.Code)
	})
	if _, err := Share(context.Background(), pool, ownerID, pushed.Code); err != nil {
		t.Fatalf("share: %v", err)
	}

	detail, err := GetKungfuForBot(context.Background(), pool, readerID, pushed.Code)
	if err != nil {
		t.Fatalf("public get: %v", err)
	}
	if _, has := detail["balance"]; has {
		t.Fatal("public get response carries balance — storage contract must not")
	}

	var balance float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, readerID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 2.0 {
		t.Fatalf("reader balance = %v, want 2 (3 - 1 spend_get)", balance)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_get' AND amount=-1 AND ref_type='kungfu' AND ref_id=$2`,
		readerID, pushed.Code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("spend_get rows = %d, want 1", n)
	}
}
