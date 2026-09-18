package payment

// Payment integration tests. These run against the local dev PostgreSQL
// (KF_TEST_DATABASE_URL); without it they skip, and CI provides it with the
// full migration chain applied.
//
// Write-failure injection follows the established A1 pattern: real DB CHECK
// constraints make the actual statements fail — no seams in the service.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func seedBot(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000") + fmt.Sprintf("%d", time.Now().UnixNano()%1000)
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"pcpay_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func seedPayment(t *testing.T, pool *pg.Pool, botID int64, amount float64) *string {
	t.Helper()
	p, err := CreatePendingPayment(context.Background(), pool, botID, PaymentSpec{
		Provider:        "manual",
		ProviderOrderID: strPtr("po_" + fmt.Sprintf("%d", time.Now().UnixNano())),
		AmountMinor:     1999,
		Currency:        "USD",
		Credits:         amount,
	})
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return &p.Code
}

func strPtr(s string) *string { return &s }

// grantCount returns how many grant_payment ledger rows exist for a payment
// code and their total amount (read-only query; tests may read the ledger).
func grantCount(t *testing.T, pool *pg.Pool, code string) (int, float64) {
	t.Helper()
	var n int
	var sum float64
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount), 0)
		 FROM tb_transactions WHERE ref_type = 'payment' AND ref_id = $1`, code).
		Scan(&n, &sum)
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n, sum
}

func balanceOf(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	b, err := credits.Balance(context.Background(), pool, botID)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b
}

func paymentStatus(t *testing.T, pool *pg.Pool, code string) (string, *time.Time) {
	t.Helper()
	var status string
	var paidAt *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT status, paid_at FROM tb_payments WHERE code = $1`, code).Scan(&status, &paidAt)
	if err != nil {
		t.Fatalf("load payment status: %v", err)
	}
	return status, paidAt
}

// withConstraint adds a CHECK constraint and drops it on cleanup (A1 pattern).
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

// -- 1. pending order creation --

func TestCreatePendingPayment(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 0)

	p, err := CreatePendingPayment(context.Background(), pool, botID, PaymentSpec{
		Provider:        "manual",
		ProviderOrderID: strPtr("po_create_1"),
		AmountMinor:     1999,
		Currency:        "USD",
		Credits:         66,
	})
	if err != nil {
		t.Fatalf("create pending payment: %v", err)
	}
	if p.Status != "pending" {
		t.Fatalf("status = %s, want pending", p.Status)
	}
	if len(p.Code) != 12 || !isHex(p.Code) {
		t.Fatalf("code = %s, want 12 hex chars", p.Code)
	}
	// DB round-trip matches the returned fact.
	status, paidAt := paymentStatus(t, pool, p.Code)
	if status != "pending" || paidAt != nil {
		t.Fatalf("db row: status=%s paid_at=%v", status, paidAt)
	}

	// amount/credits must be positive; provider/currency shape enforced.
	cases := []PaymentSpec{
		{Provider: "manual", AmountMinor: 0, Currency: "USD", Credits: 66},
		{Provider: "manual", AmountMinor: -1, Currency: "USD", Credits: 66},
		{Provider: "manual", AmountMinor: 1999, Currency: "USD", Credits: 0},
		{Provider: "manual", AmountMinor: 1999, Currency: "USD", Credits: -5},
		{Provider: "MANUAL!", AmountMinor: 1999, Currency: "USD", Credits: 66},
		{Provider: "manual", AmountMinor: 1999, Currency: "usd", Credits: 66},
		{Provider: "manual", AmountMinor: 1999, Currency: "USDT", Credits: 66},
		{Provider: "manual", ProviderOrderID: strPtr(""), AmountMinor: 1999, Currency: "USD", Credits: 66},
	}
	for i, spec := range cases {
		if _, err := CreatePendingPayment(context.Background(), pool, botID, spec); err == nil {
			t.Fatalf("case %d: invalid spec accepted", i)
		}
	}
}

// strings.AnyByteNotHex does not exist; local helper below.

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// -- 2. paid -> Credits grant --

func TestCompletePaymentGrantsCredits(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 0)
	code := seedPayment(t, pool, botID, 66)

	p, transitioned, err := CompletePayment(context.Background(), pool, *code)
	if err != nil {
		t.Fatalf("complete payment: %v", err)
	}
	if !transitioned {
		t.Fatalf("first complete should transition")
	}
	if p.Status != "paid" || p.PaidAt == nil {
		t.Fatalf("p.status=%s paid_at=%v", p.Status, p.PaidAt)
	}

	// Ledger: exactly one grant_payment with ref payment/<code>.
	n, sum := grantCount(t, pool, *code)
	if n != 1 || sum != 66 {
		t.Fatalf("grants: n=%d sum=%v, want 1/66", n, sum)
	}
	if got := balanceOf(t, pool, botID); got != 66 {
		t.Fatalf("balance = %v, want 66", got)
	}
}

// -- 3. duplicate paid idempotent (2x and 10x) --

func TestCompletePaymentDuplicateIdempotent(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 10)
	code := seedPayment(t, pool, botID, 66)

	// 2 confirmations
	for i := 0; i < 2; i++ {
		p, tr, err := CompletePayment(context.Background(), pool, *code)
		if err != nil {
			t.Fatalf("confirm #%d: %v", i+1, err)
		}
		if p.Status != "paid" {
			t.Fatalf("confirm #%d: status=%s", i+1, p.Status)
		}
		if i == 0 && !tr {
			t.Fatal("first confirm must transition")
		}
		if i > 0 && tr {
			t.Fatal("second confirm must be a no-op transition")
		}
	}

	// 10 more
	for i := 0; i < 10; i++ {
		if _, _, err := CompletePayment(context.Background(), pool, *code); err != nil {
			t.Fatalf("repeat confirm #%d: %v", i+1, err)
		}
	}

	if n, sum := grantCount(t, pool, *code); n != 1 || sum != 66 {
		t.Fatalf("after 12 confirms: grants n=%d sum=%v, want 1/66", n, sum)
	}
	if got := balanceOf(t, pool, botID); got != 76 { // 10 + 66
		t.Fatalf("balance = %v, want 76", got)
	}
	// paid_at unchanged by duplicates: re-read and compare second precision.
	_, paidAt := paymentStatus(t, pool, *code)
	if paidAt == nil {
		t.Fatal("paid_at missing")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, _, err := CompletePayment(context.Background(), pool, *code); err != nil {
		t.Fatalf("late duplicate: %v", err)
	}
	_, paidAt2 := paymentStatus(t, pool, *code)
	if !paidAt2.Equal(*paidAt) {
		t.Fatalf("paid_at changed on duplicate: %v -> %v", paidAt, paidAt2)
	}
}

// -- 4. concurrent duplicate paid idempotent --

func TestCompletePaymentConcurrentIdempotent(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 0)
	code := seedPayment(t, pool, botID, 66)

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	transitions := make([]bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, tr, err := CompletePayment(context.Background(), pool, *code)
			errs[i] = err
			if err == nil && p != nil && p.Status != "paid" {
				errs[i] = fmt.Errorf("worker %d: final status %s", i, p.Status)
			}
			transitions[i] = tr
		}(i)
	}
	wg.Wait()

	transitioned := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if transitions[i] {
			transitioned++
		}
	}
	if transitioned != 1 {
		t.Fatalf("exactly one worker must perform the paid transition, got %d", transitioned)
	}

	if n, sum := grantCount(t, pool, *code); n != 1 || sum != 66 {
		t.Fatalf("after 8 concurrent confirms: grants n=%d sum=%v, want 1/66", n, sum)
	}
	if got := balanceOf(t, pool, botID); got != 66 {
		t.Fatalf("balance = %v, want 66", got)
	}
}

// -- 5. failed/cancelled cannot grant --

func TestFailedCancelledCannotGrant(t *testing.T) {
	pool := testPool(t)

	for _, terminal := range []string{"failed", "cancelled"} {
		botID := seedBot(t, pool, 0)
		code := seedPayment(t, pool, botID, 66)

		var ok bool
		var err error
		if terminal == "failed" {
			ok, err = FailPayment(context.Background(), pool, *code)
		} else {
			ok, err = CancelPayment(context.Background(), pool, *code)
		}
		if err != nil || !ok {
			t.Fatalf("%s: mark err=%v ok=%v", terminal, err, ok)
		}

		if _, _, err := CompletePayment(context.Background(), pool, *code); err == nil {
			t.Fatalf("%s: complete must be rejected", terminal)
		} else if ae, valid := errors.IsAppError(err); !valid || ae.HTTPCode != 409 {
			t.Fatalf("%s: want 409 INVALID_PAYMENT_STATE, got %v", terminal, err)
		}

		if n, _ := grantCount(t, pool, *code); n != 0 {
			t.Fatalf("%s: %d grants leaked", terminal, n)
		}
		if got := balanceOf(t, pool, botID); got != 0 {
			t.Fatalf("%s: balance = %v, want 0", terminal, got)
		}
	}
}

// -- 6. credits mutation failure -> payment stays pending --

func TestCreditsFailureKeepsPaymentPending(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 0)
	code := seedPayment(t, pool, botID, 66)

	// Make the credits INSERT fail — scoped to THIS bot only: any other
	// bot's grant_payment of any amount stays untouched (parallel-package
	// isolation is structural, not based on magic amounts).
	withConstraint(t, pool, "tb_transactions", "pc_pay_grant_chk",
		fmt.Sprintf("bot_id <> %d OR type <> 'grant_payment' OR amount <> %g", botID, 66.0))

	// Isolation proof: while the constraint is mounted, ANOTHER bot
	// receiving a normal grant_payment of the same +66 must succeed.
	otherBot := seedBot(t, pool, 0)
	otherCode := seedPayment(t, pool, otherBot, 66)
	if _, tr, err := CompletePayment(context.Background(), pool, *otherCode); err != nil || !tr {
		t.Fatalf("other bot's +66 grant_payment must succeed during mounted constraint: tr=%v err=%v", tr, err)
	}
	if got := balanceOf(t, pool, otherBot); got != 66 {
		t.Fatalf("other bot balance = %v, want 66 (constraint must not leak)", got)
	}

	if _, _, err := CompletePayment(context.Background(), pool, *code); err == nil {
		t.Fatal("complete must fail when the ledger insert fails")
	}

	status, paidAt := paymentStatus(t, pool, *code)
	if status != "pending" || paidAt != nil {
		t.Fatalf("payment mutated despite grant failure: status=%s paid_at=%v", status, paidAt)
	}
	if n, _ := grantCount(t, pool, *code); n != 0 {
		t.Fatalf("%d grant rows committed despite failure", n)
	}
	if got := balanceOf(t, pool, botID); got != 0 {
		t.Fatalf("balance = %v, want 0", got)
	}

	// After removing the failure the same payment completes exactly once.
	_, err := pool.Exec(context.Background(), `ALTER TABLE tb_transactions DROP CONSTRAINT pc_pay_grant_chk`)
	if err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	if _, tr, err := CompletePayment(context.Background(), pool, *code); err != nil || !tr {
		t.Fatalf("retry after fix: transitioned=%v err=%v", tr, err)
	}
	if n, sum := grantCount(t, pool, *code); n != 1 || sum != 66 {
		t.Fatalf("retry grants: n=%d sum=%v", n, sum)
	}
}

// -- 7. payment update failure -> credit grant not committed --

func TestPaymentUpdateFailureRollsBackGrant(t *testing.T) {
	pool := testPool(t)
	botID := seedBot(t, pool, 0)
	code := seedPayment(t, pool, botID, 66)

	// Make the final UPDATE tb_payments fail after the grant statements ran.
	withConstraint(t, pool, "tb_payments", "pc_pay_mark_paid_chk",
		fmt.Sprintf("code <> '%s' OR status <> 'paid'", *code))

	if _, _, err := CompletePayment(context.Background(), pool, *code); err == nil {
		t.Fatal("complete must fail when the payment update fails")
	}

	if n, _ := grantCount(t, pool, *code); n != 0 {
		t.Fatalf("grant committed although payment update failed: %d rows", n)
	}
	if got := balanceOf(t, pool, botID); got != 0 {
		t.Fatalf("balance = %v, want 0 — grant must roll back with the payment", got)
	}
	status, _ := paymentStatus(t, pool, *code)
	if status != "pending" {
		t.Fatalf("status = %s, want pending", status)
	}
}

// -- 8. foreign/nonexistent bot --

func TestCreatePendingPaymentForeignBot(t *testing.T) {
	pool := testPool(t)

	// Nonexistent bot: rejected by the service bot check.
	if _, err := CreatePendingPayment(context.Background(), pool, 999999999,
		PaymentSpec{Provider: "manual", AmountMinor: 1999, Currency: "USD", Credits: 66}); err == nil {
		t.Fatal("nonexistent bot accepted")
	} else if ae, valid := errors.IsAppError(err); !valid || ae.HTTPCode != 404 {
		t.Fatalf("want 404 BOT_NOT_FOUND, got %v", err)
	}

	// FK is the backstop even if the service check were bypassed.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id = 999999999`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fk rows leaked: n=%d err=%v", n, err)
	}
}

// -- 9. migration chain: fresh DB reaches tb_payments from zero --

func TestMigrationChainFreshDB(t *testing.T) {
	// Source guard half: migrations run in filename order and 002 only
	// depends on 001's tb_bots.
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) < 2 {
		t.Fatalf("migration files: %v (%v)", files, err)
	}
	sort.Strings(files)
	if !strings.HasSuffix(files[0], "001_schema.sql") || !strings.HasSuffix(files[1], "002_payments.sql") {
		t.Fatalf("unexpected migration order: %v", files)
	}

	// Live half (requires KF_TEST_DATABASE_URL): tb_payments exists with
	// the expected contract columns and constraints.
	pool := testPool(t)
	var ok bool
	if err := pool.QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables WHERE table_name = 'tb_payments')`).
		Scan(&ok); err != nil || !ok {
		t.Fatalf("tb_payments missing: %v ok=%v", err, ok)
	}
	for _, chk := range []string{
		"chk_payments_amount_positive",
		"chk_payments_credits_positive",
		"chk_payments_status",
		"uk_payments_provider_order",
	} {
		var exists bool
		if err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, chk).
			Scan(&exists); err != nil || !exists {
			t.Fatalf("constraint %s missing: %v", chk, err)
		}
	}

	// DB-level invariants: non-positive amounts/credits rejected, status
	// domain enforced, duplicate (provider, provider_order_id) rejected.
	botID := seedBot(t, pool, 0)
	mustFail := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err == nil {
			t.Fatalf("DB accepted invalid row: %s", sql)
		}
	}
	mustFail(`INSERT INTO tb_payments (code, bot_id, provider, amount_minor, currency, credits)
	          VALUES ('deadbeef0001', $1, 'manual', 0, 'USD', 66)`, botID)
	mustFail(`INSERT INTO tb_payments (code, bot_id, provider, amount_minor, currency, credits)
	          VALUES ('deadbeef0002', $1, 'manual', 1999, 'USD', 0)`, botID)
	mustFail(`INSERT INTO tb_payments (code, bot_id, provider, amount_minor, currency, credits, status)
	          VALUES ('deadbeef0003', $1, 'manual', 1999, 'USD', 66, 'refunded')`, botID)
	mustFail(`INSERT INTO tb_payments (code, bot_id, provider, provider_order_id, amount_minor, currency, credits)
	          VALUES ('deadbeef0004', $1, 'manual', NULL, 1999, 'USD', 66)`)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tb_payments (code, bot_id, provider, provider_order_id, amount_minor, currency, credits)
		 VALUES ('deadbeef0005', $1, 'manual', 'dup_po_1', 1999, 'USD', 66)`, botID); err != nil {
		t.Fatalf("seed dup row 1: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM tb_payments WHERE code LIKE 'deadbeef%'`)
	})
	mustFail(`INSERT INTO tb_payments (code, bot_id, provider, provider_order_id, amount_minor, currency, credits)
	          VALUES ('deadbeef0006', $1, 'manual', 'dup_po_1', 1999, 'USD', 66)`, botID)
	// NULL provider_order_id rows are exempt from the uniqueness rule.
	for _, c := range []string{"deadbeef0007", "deadbeef0008"} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO tb_payments (code, bot_id, provider, provider_order_id, amount_minor, currency, credits)
			 VALUES ($1, $2, 'manual', NULL, 1999, 'USD', 66)`, c, botID); err != nil {
			t.Fatalf("null provider_order_id row rejected: %v", err)
		}
	}
}
