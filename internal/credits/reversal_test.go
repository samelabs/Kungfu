package credits

// A2 capability boundary guards and behavioral tests: the authoritative
// negative-balance primitive is restricted to internal/payment, and the
// ordinary debit / positive-from-negative invariants hold.

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

func revTestPool(t *testing.T) *pg.Pool {
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

var revSeq int64

func revUnique() string {
	return fmt.Sprintf("%d_%d", time.Now().UnixNano(), atomic.AddInt64(&revSeq, 1))
}

func revSeedBot(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	var botID int64
	suffix := revUnique()
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"revtest_"+suffix, s61SeedKeyHash("kf_live_rev_"+suffix), s61SeedLast4("kf_live_rev_"+suffix), balance).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=$1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, botID)
	})
	return botID
}

func revBalance(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func revLedgerCount(t *testing.T, pool *pg.Pool, botID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func refPtr(s string) *string { return &s }

// ordinary debit below zero → 402, no mutation
func TestRecordOrdinaryDebitStillRejectsNegative(t *testing.T) {
	pool := revTestPool(t)
	botID := revSeedBot(t, pool, 10)

	_, err := Record(context.Background(), pool, nil, botID, "spend_test", -20, refPtr("test"), refPtr("r1"))
	if err == nil {
		t.Fatal("10 - 20 must be rejected")
	}
	if !strings.Contains(err.Error(), "INSUFFICIENT_CREDITS") {
		t.Fatalf("err = %v", err)
	}
	if b := revBalance(t, pool, botID); b != 10 {
		t.Fatalf("balance = %v", b)
	}
	if n := revLedgerCount(t, pool, botID); n != 0 {
		t.Fatalf("ledger rows = %d", n)
	}
}

// authoritative reversal may go negative
func TestAuthoritativeReversalAllowsNegative(t *testing.T) {
	pool := revTestPool(t)
	botID := revSeedBot(t, pool, 10)

	newBal, err := RecordAuthoritativeReversal(context.Background(), pool, nil,
		botID, "reverse_test", -20, refPtr("test"), refPtr("r2"))
	if err != nil {
		t.Fatalf("10 - 20 authoritative must pass: %v", err)
	}
	if newBal != -10 {
		t.Fatalf("newBal = %v", newBal)
	}
	if b := revBalance(t, pool, botID); b != -10 {
		t.Fatalf("balance = %v", b)
	}
	var amount, balAfter float64
	if err := pool.QueryRow(context.Background(),
		`SELECT amount, balance_after FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&amount, &balAfter); err != nil {
		t.Fatal(err)
	}
	if amount != -20 || balAfter != -10 {
		t.Fatalf("ledger = %v/%v", amount, balAfter)
	}
}

// positive credits always land, even from a negative balance
func TestPositiveFromNegativePasses(t *testing.T) {
	pool := revTestPool(t)
	botID := revSeedBot(t, pool, -10)

	newBal, err := Record(context.Background(), pool, nil, botID, "earn_test", 4, refPtr("test"), refPtr("r3"))
	if err != nil {
		t.Fatalf("-10 + 4 must pass: %v", err)
	}
	if newBal != -6 {
		t.Fatalf("newBal = %v", newBal)
	}
	if b := revBalance(t, pool, botID); b != -6 {
		t.Fatalf("balance = %v", b)
	}
}

// ordinary debit from negative balance → still 402
func TestOrdinaryDebitFromNegativeRejected(t *testing.T) {
	pool := revTestPool(t)
	botID := revSeedBot(t, pool, -10)

	if _, err := Record(context.Background(), pool, nil, botID, "spend_test", -1, refPtr("test"), refPtr("r4")); err == nil {
		t.Fatal("-10 - 1 must be rejected")
	}
	if b := revBalance(t, pool, botID); b != -10 {
		t.Fatalf("balance = %v", b)
	}
}

// NaN/Inf and non-negative authoritative reversals are rejected
func TestAuthoritativeReversalInputGates(t *testing.T) {
	pool := revTestPool(t)
	botID := revSeedBot(t, pool, 100)

	for _, amount := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, 10} {
		if _, err := RecordAuthoritativeReversal(context.Background(), pool, nil,
			botID, "reverse_test", amount, refPtr("test"), refPtr("r5")); err == nil {
			t.Fatalf("amount %v must be rejected", amount)
		}
	}
	if b := revBalance(t, pool, botID); b != 100 {
		t.Fatalf("balance = %v", b)
	}
	if n := revLedgerCount(t, pool, botID); n != 0 {
		t.Fatalf("ledger rows = %d", n)
	}
}

// ledger read primitive: SUM by type/ref
func TestSumAmountByTypeRef(t *testing.T) {
	pool := revTestPool(t)
	botID := revSeedBot(t, pool, 0)

	_, _ = Record(context.Background(), pool, nil, botID, "reverse_test", -200, nil, nil)
	_ = pgPoolExec(t, pool, `INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id, created_at)
		VALUES ($1,'reverse_test',-300,0,'payment','pc1',NOW())`, botID)

	sum, err := SumAmountByTypeRef(context.Background(), pool, botID, "reverse_test", "payment", "pc1")
	if err != nil {
		t.Fatal(err)
	}
	if sum != -300 {
		t.Fatalf("sum = %v", sum)
	}
	other, _ := SumAmountByTypeRef(context.Background(), pool, botID, "reverse_test", "payment", "other")
	if other != 0 {
		t.Fatalf("other = %v", other)
	}
}

func pgPoolExec(t *testing.T, pool *pg.Pool, sql string, args ...interface{}) error {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}
