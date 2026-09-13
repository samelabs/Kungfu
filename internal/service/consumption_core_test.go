package service

// Storage-free integration tests: all storage operations are currently
// free (consumption policy amount 0) — no 402s, no ledger rows, balance
// untouched. Private non-owner still 403. Local PG via KF_TEST_DATABASE_URL.

import (
	"context"
	"strings"
	"testing"
)

// TestPublicGetBalanceZeroSucceeds: public non-owner get with zero balance
// succeeds and books nothing.
func TestPublicGetBalanceZeroSucceeds(t *testing.T) {
	pool := a7TestPool(t)
	ownerID, _, _ := a7TestBot(t, pool, 5)
	readerID, _, _ := a7TestBot(t, pool, 0)

	pushed, err := Push(context.Background(), pool, ownerID, map[string]interface{}{
		"title": "CC zero-balance get", "tags": []interface{}{"t"},
		"content": strings.Repeat("a", 60),
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
		t.Fatalf("public get at balance 0 must succeed (free): %v", err)
	}
	if _, has := detail["balance"]; has {
		t.Fatal("response carries balance — storage contract must not")
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, readerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reader ledger rows = %d, want 0", n)
	}
	var balance float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, readerID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		t.Fatalf("reader balance = %v, want 0 (free)", balance)
	}
}

// TestPrivateGetStillRejected: private non-owner get is rejected 403
// before any consumption — access control is independent of pricing.
func TestPrivateGetStillRejected(t *testing.T) {
	pool := a7TestPool(t)
	ownerID, _, _ := a7TestBot(t, pool, 5)
	readerID, _, _ := a7TestBot(t, pool, 3)

	pushed, err := Push(context.Background(), pool, ownerID, map[string]interface{}{
		"title": "CC private", "tags": []interface{}{"t"},
		"content": strings.Repeat("b", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE target_type='kungfu' AND target_id=$1`, pushed.Code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_kungfus WHERE code=$1`, pushed.Code)
	})
	// stays private (default visibility)

	_, err = GetKungfuForBot(context.Background(), pool, readerID, pushed.Code)
	ae, ok := apperrIs(err)
	if !ok || ae.HTTPCode != 403 {
		t.Fatalf("want 403 PRIVATE_KUNGFU, got %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, readerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reader ledger rows = %d, want 0", n)
	}
}

// TestUpdateListOwnerGetNoRegressions: update / list / owner get all still
// work and stay free; no ledger rows at all for the bot.
func TestUpdateListOwnerGetNoRegressions(t *testing.T) {
	pool := a7TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 10)
	t.Cleanup(func() { a5CleanupBot(t, pool, botID) })

	created, err := Push(context.Background(), pool, botID, map[string]interface{}{
		"title": "CC free ops 1", "tags": []interface{}{"t"},
		"content": strings.Repeat("c", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE target_type='kungfu' AND target_id=$1`, created.Code)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_kungfus WHERE code=$1`, created.Code)
	})

	updated, err := Push(context.Background(), pool, botID, map[string]interface{}{
		"code":    created.Code,
		"title":   "CC free ops 2",
		"tags":    []interface{}{"t"},
		"content": strings.Repeat("d", 70),
	}, 128, 10, 24, 500, 102400)
	if err != nil || updated.Action != "updated" {
		t.Fatalf("update: %v action=%s", err, updated.Action)
	}

	list, err := ListKungfusForBot(context.Background(), pool, botID, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, has := list["balance"]; has {
		t.Fatal("list carries balance — storage contract must not")
	}

	detail, err := GetKungfuForBot(context.Background(), pool, botID, created.Code)
	if err != nil {
		t.Fatalf("owner get: %v", err)
	}
	if _, has := detail["balance"]; has {
		t.Fatal("owner get carries balance — storage contract must not")
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("ledger rows = %d, want 0 (all storage ops free)", n)
	}
	var balance float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 10.0 {
		t.Fatalf("balance = %v, want 10 (untouched)", balance)
	}
}

// TestCreateFreeInsertFailureStillAtomic: even free, a failing kungfu
// insert must not leave a half-written row (transaction discipline intact).
func TestCreateFreeInsertFailureStillAtomic(t *testing.T) {
	pool := a7TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 10)
	t.Cleanup(func() { a5CleanupBot(t, pool, botID) })

	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE tb_kungfus ADD CONSTRAINT cc_free_ins_fail_chk CHECK (title <> 'CC-FAILINSERT-FREE')`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE tb_kungfus DROP CONSTRAINT IF EXISTS cc_free_ins_fail_chk`)
	})

	_, err := Push(ctx, pool, botID, map[string]interface{}{
		"title": "CC-FAILINSERT-FREE", "tags": []interface{}{"t"},
		"content": strings.Repeat("e", 60),
	}, 128, 10, 24, 500, 102400)
	if err == nil {
		t.Fatal("push must fail when the kungfu insert fails")
	}

	var k int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_kungfus WHERE bot_id=$1`, botID).Scan(&k); err != nil {
		t.Fatal(err)
	}
	if k != 0 {
		t.Fatalf("kungfu rows = %d, want 0", k)
	}
}
