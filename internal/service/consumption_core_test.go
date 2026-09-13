package service

// Consumption-core integration tests: the storage paths charge through
// internal/consumption, atomically with the storage write, and the storage
// responses carry no balance. Local PG via KF_TEST_DATABASE_URL.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestPublicGetBalanceZeroIs402: public non-owner get with zero balance
// fails 402 and books nothing.
func TestPublicGetBalanceZeroIs402(t *testing.T) {
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

	_, err = GetKungfuForBot(context.Background(), pool, readerID, pushed.Code)
	ae, ok := apperrIs(err)
	if !ok || ae.HTTPCode != 402 || ae.Code != "INSUFFICIENT_CREDITS" {
		t.Fatalf("want 402 INSUFFICIENT_CREDITS, got %v", err)
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

// TestPrivateGetConsumesNothing: private non-owner get is rejected 403
// before any consumption — zero ledger rows.
func TestPrivateGetConsumesNothing(t *testing.T) {
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

// TestUpdateExistingDoesNotConsume: pushing to an existing code is an
// update — no charge, no ledger row.
func TestUpdateExistingDoesNotConsume(t *testing.T) {
	pool := a7TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 10)
	t.Cleanup(func() { a5CleanupBot(t, pool, botID) })

	created, err := Push(context.Background(), pool, botID, map[string]interface{}{
		"title": "CC update 1", "tags": []interface{}{"t"},
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
		"title":   "CC update 2",
		"tags":    []interface{}{"t"},
		"content": strings.Repeat("d", 70),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Action != "updated" {
		t.Fatalf("action = %s, want updated", updated.Action)
	}

	var balance float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 9.0 {
		t.Fatalf("balance = %v, want 9 (only the create charged)", balance)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_push'`, botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("spend_push rows = %d, want 1 (update free)", n)
	}
}

// TestCreateChargeRollsBackWithInsert: kungfu insert failure rolls the
// consumption charge back — real CHECK constraint injection.
func TestCreateChargeRollsBackWithInsert(t *testing.T) {
	pool := a7TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 10)
	t.Cleanup(func() { a5CleanupBot(t, pool, botID) })

	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE tb_kungfus ADD CONSTRAINT cc_ins_fail_chk CHECK (title <> 'CC-FAILINSERT')`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `ALTER TABLE tb_kungfus DROP CONSTRAINT IF EXISTS cc_ins_fail_chk`)
	})

	_, err := Push(ctx, pool, botID, map[string]interface{}{
		"title": "CC-FAILINSERT", "tags": []interface{}{"t"},
		"content": strings.Repeat("e", 60),
	}, 128, 10, 24, 500, 102400)
	if err == nil {
		t.Fatal("push must fail when the kungfu insert fails")
	}

	var balance float64
	if err := pool.QueryRow(ctx,
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 10.0 {
		t.Fatalf("balance = %v, want 10 (charge rolled back with insert)", balance)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_push'`, botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("spend_push rows = %d, want 0 (rolled back)", n)
	}
	var k int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_kungfus WHERE bot_id=$1`, botID).Scan(&k); err != nil {
		t.Fatal(err)
	}
	if k != 0 {
		t.Fatalf("kungfu rows = %d, want 0", k)
	}
	_ = fmt.Sprint()
}
