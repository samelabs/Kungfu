package store

// Store / Redemption integration tests. They run against the local dev
// PostgreSQL (KF_TEST_DATABASE_URL); CI provides it with the full
// migration chain (001 -> 002 -> 003) applied.
//
// Write-failure injection follows the established A1/payment pattern:
// real DB CHECK constraints make the actual statements fail — no seams in
// the service.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// -- helpers --

func testPool(t *testing.T) *pg.Pool {
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

var nameCounter int64

func uniq() string {
	return time.Now().Format("150405.000000000") + fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
}

func seedBot(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	suffix := uniq()
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"srbot_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func seedProduct(t *testing.T, pool *pg.Pool, price float64) string {
	t.Helper()
	p, err := CreateProduct(context.Background(), pool, ProductInput{
		Title:        "SR Product " + uniq(),
		CreditsPrice: price,
	})
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return p.Code
}

func redeem(t *testing.T, pool *pg.Pool, botID int64, productCode, key string) *RedeemResult {
	t.Helper()
	res, err := Redeem(context.Background(), pool, botID, productCode, key)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return res
}

func balanceOf(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	b, err := credits.Balance(context.Background(), pool, botID)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b
}

// ledger returns the spend/refund ledger rows for a redemption code:
// spends, spendSum, refunds, refundSum.
func ledger(t *testing.T, pool *pg.Pool, code string) (int, float64, int, float64) {
	t.Helper()
	var spends, refunds int
	var spendSum, refundSum float64
	err := pool.QueryRow(context.Background(), `
		SELECT
		  COUNT(*) FILTER (WHERE type = 'spend_redemption'),
		  COALESCE(SUM(amount) FILTER (WHERE type = 'spend_redemption'), 0),
		  COUNT(*) FILTER (WHERE type = 'refund_redemption'),
		  COALESCE(SUM(amount) FILTER (WHERE type = 'refund_redemption'), 0)
		FROM tb_transactions WHERE ref_type = 'redemption' AND ref_id = $1`, code).
		Scan(&spends, &spendSum, &refunds, &refundSum)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return spends, spendSum, refunds, refundSum
}

func statusOf(t *testing.T, pool *pg.Pool, code string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM tb_redemptions WHERE code = $1`, code).Scan(&status); err != nil {
		t.Fatalf("load redemption status: %v", err)
	}
	return status
}

func withConstraint(t *testing.T, pool *pg.Pool, table, name, expr string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s)`, table, name, expr)); err != nil {
		t.Fatalf("add constraint %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`, table, name))
	})
}

func wantAppErr(t *testing.T, err error, httpCode int) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != httpCode {
		t.Fatalf("want HTTP %d, got %v", httpCode, err)
	}
}

// -- 1. active product redemption --

func TestRedeemActiveProduct(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	res := redeem(t, pool, botID, product, "rk_active_1")
	if !res.Created {
		t.Fatal("first redeem must create")
	}
	if res.Redemption.Status != "pending_review" {
		t.Fatalf("status = %s", res.Redemption.Status)
	}

	spends, spendSum, refunds, _ := ledger(t, pool, res.Redemption.Code)
	if spends != 1 || spendSum != -30 || refunds != 0 {
		t.Fatalf("ledger: spends=%d/%v refunds=%d", spends, spendSum, refunds)
	}
	if got := balanceOf(t, pool, botID); got != 70 {
		t.Fatalf("balance = %v, want 70", got)
	}
}

// -- 2. inactive product cannot be redeemed --

func TestRedeemInactiveProduct(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	if ok, err := SetProductInactive(context.Background(), pool, product); err != nil || !ok {
		t.Fatalf("deactivate: ok=%v err=%v", ok, err)
	}

	_, err := Redeem(context.Background(), pool, botID, product, "rk_inactive_1")
	wantAppErr(t, err, 409)

	// nothing created, nothing debited
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = $2`,
		botID, "rk_inactive_1").Scan(&n)
	if n != 0 {
		t.Fatalf("redemption leaked: %d", n)
	}
	if got := balanceOf(t, pool, botID); got != 100 {
		t.Fatalf("balance = %v, want 100", got)
	}
}

// -- 3. snapshot survives product changes --

func TestSnapshotSurvivesProductChanges(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	res := redeem(t, pool, botID, product, "rk_snap_1")

	// Change price and title directly (no product-update service this
	// round by design; SQL mirrors a future catalog edit).
	_, err := pool.Exec(context.Background(),
		`UPDATE tb_store_products SET title = 'RENAMED', credits_price = 999,
		 status = 'inactive', updated_at = NOW() WHERE code = $1`, product)
	if err != nil {
		t.Fatalf("mutate product: %v", err)
	}

	r, err := GetRedemption(context.Background(), pool, res.Redemption.Code)
	if err != nil {
		t.Fatalf("reload redemption: %v", err)
	}
	if r.CreditsCost != 30 {
		t.Fatalf("snapshot cost = %v, want 30", r.CreditsCost)
	}
	if r.ProductTitle == "RENAMED" || r.ProductTitle == "" {
		t.Fatalf("snapshot title = %q", r.ProductTitle)
	}
}

// -- 4. insufficient credits -> nothing happens --

func TestRedeemInsufficientCredits(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 10)
	product := seedProduct(t, pool, 30)

	_, err := Redeem(context.Background(), pool, botID, product, "rk_insuf_1")
	wantAppErr(t, err, 402)

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = $2`,
		botID, "rk_insuf_1").Scan(&n)
	if n != 0 {
		t.Fatalf("redemption leaked: %d", n)
	}
	if got := balanceOf(t, pool, botID); got != 10 {
		t.Fatalf("balance = %v, want 10", got)
	}
}

// -- 5. debit DB failure -> redemption rolls back --

func TestDebitFailureRollsBackRedemption(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	// Poison the spend insert — scoped to THIS bot only: any other bot's
	// spend_redemption of any amount stays untouched (parallel-package
	// isolation is structural, not based on magic amounts).
	withConstraint(t, pool, "tb_transactions", "sr_spend_chk",
		fmt.Sprintf("bot_id <> %d OR type <> 'spend_redemption' OR amount <> %g", botID, -30.0))

	// Isolation proof: while the constraint is mounted, ANOTHER bot doing
	// a normal spend_redemption of the same -30 amount must succeed.
	otherBot := seedBot(t, pool, 100)
	otherProduct := seedProduct(t, pool, 30)
	if res := redeem(t, pool, otherBot, otherProduct, "rk_debitfail_iso_other"); !res.Created {
		t.Fatal("other bot's -30 spend_redemption must succeed during mounted constraint")
	}
	if got := balanceOf(t, pool, otherBot); got != 70 {
		t.Fatalf("other bot balance = %v, want 70 (constraint must not leak)", got)
	}

	_, err := Redeem(context.Background(), pool, botID, product, "rk_debitfail_1")
	if err == nil {
		t.Fatal("redeem must fail when the debit insert fails")
	}

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = $2`,
		botID, "rk_debitfail_1").Scan(&n)
	if n != 0 {
		t.Fatalf("redemption committed despite debit failure")
	}
	if got := balanceOf(t, pool, botID); got != 100 {
		t.Fatalf("balance = %v, want 100", got)
	}
}

// -- 6. redemption insert failure -> credits stay intact --

func TestRedemptionInsertFailureNoDebit(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)

	// Poison the redemption insert via the title snapshot.
	withConstraint(t, pool, "tb_redemptions", "sr_insert_chk",
		"product_title <> 'SR-FAILINSERT'")

	// A product whose title trips the constraint (code is 12 hex chars).
	_, err := pool.Exec(context.Background(),
		`INSERT INTO tb_store_products (code, title, credits_price, status)
		 VALUES ('fai1in500001', 'SR-FAILINSERT', 30, 'active')`)
	if err != nil {
		t.Fatalf("seed fail product: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_redemptions WHERE product_id = (SELECT id FROM tb_store_products WHERE code = 'fai1in500001')`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_store_products WHERE code = 'fai1in500001'`)
	})

	_, err = Redeem(context.Background(), pool, botID, "fai1in500001", "rk_insfail_1")
	if err == nil {
		t.Fatal("redeem must fail when the redemption insert fails")
	}

	// No ledger row for any redemption of this bot (code never allocated).
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1 AND type = 'spend_redemption'`,
		botID).Scan(&n)
	if n != 0 {
		t.Fatalf("debit committed despite redemption insert failure: %d", n)
	}
	if got := balanceOf(t, pool, botID); got != 100 {
		t.Fatalf("balance = %v, want 100", got)
	}
}

// -- 7. request_key serial repeat debits once --

func TestRequestKeySerialIdempotent(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	first := redeem(t, pool, botID, product, "rk_serial_1")
	for i := 0; i < 9; i++ {
		res := redeem(t, pool, botID, product, "rk_serial_1")
		if res.Created {
			t.Fatalf("repeat #%d created a new redemption", i+2)
		}
		if res.Redemption.Code != first.Redemption.Code {
			t.Fatalf("repeat #%d returned a different redemption", i+2)
		}
	}

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = $2`,
		botID, "rk_serial_1").Scan(&n)
	if n != 1 {
		t.Fatalf("redemptions = %d, want 1", n)
	}
	spends, spendSum, _, _ := ledger(t, pool, first.Redemption.Code)
	if spends != 1 || spendSum != -30 {
		t.Fatalf("ledger: spends=%d/%v", spends, spendSum)
	}
	if got := balanceOf(t, pool, botID); got != 70 {
		t.Fatalf("balance = %v, want 70", got)
	}
}

// -- 8. request_key concurrent repeat debits once --

func TestRequestKeyConcurrentIdempotent(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	const workers = 8
	results := make([]*RedeemResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = Redeem(context.Background(), pool, botID, product, "rk_conc_1")
		}(i)
	}
	wg.Wait()

	created := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i].Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want exactly 1", created)
	}

	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = $2`,
		botID, "rk_conc_1").Scan(&n)
	if n != 1 {
		t.Fatalf("redemptions = %d, want 1", n)
	}
	spends, spendSum, _, _ := ledger(t, pool, results[0].Redemption.Code)
	if spends != 1 || spendSum != -30 {
		t.Fatalf("ledger: spends=%d/%v", spends, spendSum)
	}
	if got := balanceOf(t, pool, botID); got != 70 {
		t.Fatalf("balance = %v, want 70", got)
	}
}

// -- 9. same key different product -> 409 --

func TestRequestKeyDifferentProductConflict(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	p1 := seedProduct(t, pool, 30)
	p2 := seedProduct(t, pool, 50)

	redeem(t, pool, botID, p1, "rk_conflict_1")

	_, err := Redeem(context.Background(), pool, botID, p2, "rk_conflict_1")
	wantAppErr(t, err, 409)

	if got := balanceOf(t, pool, botID); got != 70 {
		t.Fatalf("balance = %v, want 70 (no second debit)", got)
	}
}

// -- 10-12. approve / reject+refund / double reject --

func TestApproveRejectTransitions(t *testing.T) {
	pool := testPool(t)

	// approve
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_ap_1")

		out, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok")
		if err != nil || !out.Transitioned {
			t.Fatalf("approve: %v transitioned=%v", err, out.Transitioned)
		}
		if statusOf(t, pool, res.Redemption.Code) != "approved" {
			t.Fatal("status not approved")
		}
		// double approve idempotent, no economic action
		out2, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok")
		if err != nil || out2.Transitioned {
			t.Fatalf("double approve: %v transitioned=%v", err, out2.Transitioned)
		}
		_, _, refunds, refundSum := ledger(t, pool, res.Redemption.Code)
		if refunds != 0 || refundSum != 0 {
			t.Fatalf("approve must not refund: %d/%v", refunds, refundSum)
		}
		if got := balanceOf(t, pool, botID); got != 70 {
			t.Fatalf("balance = %v, want 70", got)
		}
	}

	// reject + refund, then double reject without second refund
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_rj_1")

		out, err := RejectRedemption(context.Background(), pool, res.Redemption.Code, "bad")
		if err != nil || !out.Transitioned {
			t.Fatalf("reject: %v", err)
		}
		if statusOf(t, pool, res.Redemption.Code) != "rejected" {
			t.Fatal("status not rejected")
		}
		if got := balanceOf(t, pool, botID); got != 100 {
			t.Fatalf("balance = %v, want 100 (refunded)", got)
		}
		spends, spendSum, refunds, refundSum := ledger(t, pool, res.Redemption.Code)
		if spends != 1 || spendSum != -30 || refunds != 1 || refundSum != 30 {
			t.Fatalf("ledger: %d/%v %d/%v", spends, spendSum, refunds, refundSum)
		}

		out2, err := RejectRedemption(context.Background(), pool, res.Redemption.Code, "bad")
		if err != nil || out2.Transitioned {
			t.Fatalf("double reject: %v transitioned=%v", err, out2.Transitioned)
		}
		_, _, refunds2, _ := ledger(t, pool, res.Redemption.Code)
		if refunds2 != 1 {
			t.Fatalf("refunds = %d, want 1", refunds2)
		}
		if got := balanceOf(t, pool, botID); got != 100 {
			t.Fatalf("balance = %v, want 100", got)
		}
	}
}

// -- 13-15. cancel paths + double cancel --

func TestCancelTransitions(t *testing.T) {
	pool := testPool(t)

	// pending_review -> cancelled + refund
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_cx_1")

		out, err := CancelRedemption(context.Background(), pool, res.Redemption.Code)
		if err != nil || !out.Transitioned {
			t.Fatalf("cancel pending: %v", err)
		}
		if statusOf(t, pool, res.Redemption.Code) != "cancelled" {
			t.Fatal("status not cancelled")
		}
		if got := balanceOf(t, pool, botID); got != 100 {
			t.Fatalf("balance = %v, want 100", got)
		}

		// double cancel idempotent
		out2, err := CancelRedemption(context.Background(), pool, res.Redemption.Code)
		if err != nil || out2.Transitioned {
			t.Fatalf("double cancel: %v transitioned=%v", err, out2.Transitioned)
		}
		_, _, refunds, _ := ledger(t, pool, res.Redemption.Code)
		if refunds != 1 {
			t.Fatalf("refunds = %d, want 1", refunds)
		}
	}

	// approved -> cancelled + refund
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_cx_2")
		if _, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok"); err != nil {
			t.Fatalf("approve: %v", err)
		}

		out, err := CancelRedemption(context.Background(), pool, res.Redemption.Code)
		if err != nil || !out.Transitioned {
			t.Fatalf("cancel approved: %v", err)
		}
		if statusOf(t, pool, res.Redemption.Code) != "cancelled" {
			t.Fatal("status not cancelled")
		}
		if got := balanceOf(t, pool, botID); got != 100 {
			t.Fatalf("balance = %v, want 100", got)
		}
	}
}

// -- 16-18. fulfillment --

func TestFulfillment(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)

	// pending -> fulfilled directly is illegal
	res := redeem(t, pool, botID, product, "rk_ff_1")
	_, err := FulfillRedemption(context.Background(), pool, res.Redemption.Code, "delivered")
	wantAppErr(t, err, 409)

	// approved -> fulfilled
	if _, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	out, err := FulfillRedemption(context.Background(), pool, res.Redemption.Code, "delivered")
	if err != nil || !out.Transitioned {
		t.Fatalf("fulfill: %v", err)
	}
	if statusOf(t, pool, res.Redemption.Code) != "fulfilled" {
		t.Fatal("status not fulfilled")
	}

	// double fulfill idempotent, no second economic action
	out2, err := FulfillRedemption(context.Background(), pool, res.Redemption.Code, "delivered")
	if err != nil || out2.Transitioned {
		t.Fatalf("double fulfill: %v transitioned=%v", err, out2.Transitioned)
	}

	// fulfilled cannot be cancelled nor refunded
	_, err = CancelRedemption(context.Background(), pool, res.Redemption.Code)
	wantAppErr(t, err, 409)
	_, _, refunds, _ := ledger(t, pool, res.Redemption.Code)
	if refunds != 0 {
		t.Fatalf("refunds = %d, want 0", refunds)
	}
	if got := balanceOf(t, pool, botID); got != 70 {
		t.Fatalf("balance = %v, want 70", got)
	}
}

// -- 19. concurrent races: one legal outcome, at most one refund --

func TestConcurrentStateRaces(t *testing.T) {
	pool := testPool(t)

	// reject vs approve
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_race_rej_ap")
		raceTwo(t, pool,
			func() error {
				_, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok")
				return err
			},
			func() error {
				_, err := RejectRedemption(context.Background(), pool, res.Redemption.Code, "bad")
				return err
			},
		)
		final := statusOf(t, pool, res.Redemption.Code)
		if final != "approved" && final != "rejected" {
			t.Fatalf("reject-vs-approve final = %s", final)
		}
		_, _, _, refundSum := ledger(t, pool, res.Redemption.Code)
		wantRefunds := 0.0
		if final == "rejected" {
			wantRefunds = 30
		}
		if refundSum != wantRefunds {
			t.Fatalf("refunds = %v, want %v", refundSum, wantRefunds)
		}
	}

	// cancel vs approve
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_race_cx_ap")
		raceTwo(t, pool,
			func() error {
				_, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok")
				return err
			},
			func() error { _, err := CancelRedemption(context.Background(), pool, res.Redemption.Code); return err },
		)
		final := statusOf(t, pool, res.Redemption.Code)
		if final != "approved" && final != "cancelled" {
			t.Fatalf("cancel-vs-approve final = %s", final)
		}
	}

	// cancel vs fulfill
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_race_cx_ff")
		if _, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok"); err != nil {
			t.Fatalf("approve: %v", err)
		}
		raceTwo(t, pool,
			func() error {
				_, err := FulfillRedemption(context.Background(), pool, res.Redemption.Code, "d")
				return err
			},
			func() error { _, err := CancelRedemption(context.Background(), pool, res.Redemption.Code); return err },
		)
		final := statusOf(t, pool, res.Redemption.Code)
		if final != "fulfilled" && final != "cancelled" {
			t.Fatalf("cancel-vs-fulfill final = %s", final)
		}
		if final == "fulfilled" {
			if _, _, refunds, _ := ledger(t, pool, res.Redemption.Code); refunds != 0 {
				t.Fatal("fulfilled winner must not have refunded")
			}
		}
	}

	// double reject concurrent
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_race_2rj")
		raceTwo(t, pool,
			func() error {
				_, err := RejectRedemption(context.Background(), pool, res.Redemption.Code, "b")
				return err
			},
			func() error {
				_, err := RejectRedemption(context.Background(), pool, res.Redemption.Code, "b")
				return err
			},
		)
		if statusOf(t, pool, res.Redemption.Code) != "rejected" {
			t.Fatal("final not rejected")
		}
		_, _, refunds, refundSum := ledger(t, pool, res.Redemption.Code)
		if refunds != 1 || refundSum != 30 {
			t.Fatalf("double-reject refunds: %d/%v", refunds, refundSum)
		}
	}

	// double cancel concurrent
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_race_2cx")
		raceTwo(t, pool,
			func() error { _, err := CancelRedemption(context.Background(), pool, res.Redemption.Code); return err },
			func() error { _, err := CancelRedemption(context.Background(), pool, res.Redemption.Code); return err },
		)
		if statusOf(t, pool, res.Redemption.Code) != "cancelled" {
			t.Fatal("final not cancelled")
		}
		_, _, refunds, refundSum := ledger(t, pool, res.Redemption.Code)
		if refunds != 1 || refundSum != 30 {
			t.Fatalf("double-cancel refunds: %d/%v", refunds, refundSum)
		}
	}

	// double fulfill concurrent
	{
		botID := seedBot(t, pool, 100)
		product := seedProduct(t, pool, 30)
		res := redeem(t, pool, botID, product, "rk_race_2ff")
		if _, err := ApproveRedemption(context.Background(), pool, res.Redemption.Code, "ok"); err != nil {
			t.Fatalf("approve: %v", err)
		}
		raceTwo(t, pool,
			func() error {
				_, err := FulfillRedemption(context.Background(), pool, res.Redemption.Code, "d")
				return err
			},
			func() error {
				_, err := FulfillRedemption(context.Background(), pool, res.Redemption.Code, "d")
				return err
			},
		)
		if statusOf(t, pool, res.Redemption.Code) != "fulfilled" {
			t.Fatal("final not fulfilled")
		}
	}
}

// raceTwo runs two transition funcs concurrently and requires at least
// one to succeed.
func raceTwo(t *testing.T, pool *pg.Pool, a, b func() error) {
	t.Helper()
	errs := make([]error, 2)
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for i, fn := range []func() error{a, b} {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			start.Wait()
			errs[i] = fn()
		}(i, fn)
	}
	start.Done()
	wg.Wait()
	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
		} else {
			ae, ok := errors.IsAppError(err)
			if !ok || ae.HTTPCode != 409 {
				t.Fatalf("racer %d failed with non-409 error: %v", i, err)
			}
		}
	}
	if succeeded < 1 {
		t.Fatalf("no racer succeeded: %v / %v", errs[0], errs[1])
	}
}

// -- 20. ledger ref correctness (covered inline above, explicit re-check) --

func TestLedgerRefShape(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 100)
	product := seedProduct(t, pool, 30)
	res := redeem(t, pool, botID, product, "rk_ref_1")
	if _, err := RejectRedemption(context.Background(), pool, res.Redemption.Code, "bad"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var spendType, spendRefType, spendRefID string
	if err := pool.QueryRow(context.Background(), `
		SELECT type, ref_type, ref_id FROM tb_transactions
		WHERE ref_type = 'redemption' AND ref_id = $1 AND type = 'spend_redemption'`,
		res.Redemption.Code).Scan(&spendType, &spendRefType, &spendRefID); err != nil {
		t.Fatalf("spend row: %v", err)
	}
	if spendType != "spend_redemption" || spendRefType != "redemption" || spendRefID != res.Redemption.Code {
		t.Fatalf("spend ref wrong: %s/%s/%s", spendType, spendRefType, spendRefID)
	}
	var refundType string
	if err := pool.QueryRow(context.Background(), `
		SELECT type FROM tb_transactions
		WHERE ref_type = 'redemption' AND ref_id = $1 AND type = 'refund_redemption'`,
		res.Redemption.Code).Scan(&refundType); err != nil {
		t.Fatalf("refund row: %v", err)
	}
}

// -- 21. migration chain 001 -> 002 -> 003 --

func TestMigrationChainIncludes003(t *testing.T) {
	pool := testPool(t)
	for _, table := range []string{"tb_payments", "tb_store_products", "tb_redemptions"} {
		var ok bool
		if err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	for _, chk := range []string{
		"chk_store_price_positive",
		"chk_store_product_status",
		"uk_redemption_bot_request",
		"chk_redemption_cost_positive",
		"chk_redemption_status",
	} {
		var exists bool
		if err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, chk).
			Scan(&exists); err != nil || !exists {
			t.Fatalf("constraint %s missing: %v", chk, err)
		}
	}
	// rejected status cannot even be inserted directly with a bad status value.
	botID := seedBot(t, pool, 0)
	product := seedProduct(t, pool, 1)
	var prodID int64
	_ = pool.QueryRow(context.Background(),
		`SELECT id FROM tb_store_products WHERE code = $1`, product).Scan(&prodID)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tb_redemptions (code, bot_id, product_id, product_title, credits_cost, request_key, status)
		 VALUES ('badstatus0001', $1, $2, 'x', 1, 'rk_bad_status', 'shipped')`, botID, prodID); err == nil {
		t.Fatal("invalid status accepted by DB")
	}
}

// s61SeedKeyHash / s61SeedLast4: mechanical S6.1 fixture helpers —
// seed the SHA-256 digest + display last4 for a raw agent key.
func s61SeedKeyHash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func s61SeedLast4(raw string) string {
	if len(raw) < 4 {
		return raw
	}
	return raw[len(raw)-4:]
}
