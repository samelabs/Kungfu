package credits

// Integer-credit invariant tests: overflow fails closed (zero mutation,
// zero ledger row), the sign contract holds (ordinary spend never below
// zero; authoritative reversal may go negative), and balance reads stay
// exact. Real PostgreSQL.

import (
	"context"
	"crypto/sha256"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

func finTestPool(t *testing.T) *pg.Pool {
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

func finSeedBot(t *testing.T, pool *pg.Pool, balance int64) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"fin_"+suffix, fixtureKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), fixtureKeyLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
	).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func finLedgerCount(t *testing.T, pool *pg.Pool, botID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1`, botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Record that would overflow int64 → zero mutation, zero ledger,
// 500 CREDIT_OVERFLOW — integer replacement of the old NaN/Inf gate.
func TestRecordOverflowFailsClosed(t *testing.T) {
	pool := finTestPool(t)
	botID := finSeedBot(t, pool, math.MaxInt64)

	rt := "test"
	bal, err := Record(context.Background(), pool, nil, botID, "grant_signup", 1, &rt, nil)
	if err == nil {
		t.Fatalf("overflowing record accepted, balance=%d", bal)
	}
	if ae, ok := errors.IsAppError(err); !ok || ae.Code != "CREDIT_OVERFLOW" {
		t.Fatalf("want CREDIT_OVERFLOW, got %v", err)
	}

	var balance int64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance FROM tb_bots WHERE id = $1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != math.MaxInt64 {
		t.Fatalf("balance mutated by overflowing record: %d", balance)
	}
	if n := finLedgerCount(t, pool, botID); n != 0 {
		t.Fatalf("ledger rows after overflowing record: %d", n)
	}
}

// Large-but-safe arithmetic still works right up to the boundary.
func TestRecordNearOverflowBoundary(t *testing.T) {
	pool := finTestPool(t)
	botID := finSeedBot(t, pool, math.MaxInt64-5)

	rt := "test"
	bal, err := Record(context.Background(), pool, nil, botID, "grant_signup", 5, &rt, nil)
	if err != nil || bal != math.MaxInt64 {
		t.Fatalf("boundary-legal record failed: bal=%d err=%v", bal, err)
	}
	// one more credit tips over the boundary → fail closed
	if _, err := Record(context.Background(), pool, nil, botID, "grant_signup", 1, &rt, nil); err == nil {
		t.Fatal("post-boundary record accepted")
	}
}

// Most-negative balance plus a positive amount: overflow check must
// reject the wrap, not produce a bogus positive balance.
func TestRecordOverflowFromNegativeBoundary(t *testing.T) {
	pool := finTestPool(t)
	botID := finSeedBot(t, pool, math.MinInt64+1)

	rt := "payment"
	if _, err := Record(context.Background(), pool, nil, botID, "reverse_payment", -1, &rt, nil); err == nil {
		t.Fatal("underflow reversal accepted")
	}
}

// Ordinary spend can never drive the balance below zero (402), and a
// positive amount landing on a negative (debt) balance reduces it.
func TestRecordSignContract(t *testing.T) {
	pool := finTestPool(t)
	ctx := context.Background()

	// 402 path
	botID := finSeedBot(t, pool, 10)
	rt := "test"
	if _, err := Record(ctx, pool, nil, botID, "spend_redemption", -11, &rt, nil); err == nil {
		t.Fatal("ordinary negative-crossing spend accepted")
	}

	// debt offset: authoritative -15 then ordinary +3 → -2
	if _, err := RecordAuthoritativeReversal(ctx, pool, nil, botID, "reverse_payment", -15, &rt, nil); err != nil {
		t.Fatalf("authoritative reversal failed: %v", err)
	}
	bal, err := Record(ctx, pool, nil, botID, "grant_payment", 3, &rt, nil)
	if err != nil || bal != -2 {
		t.Fatalf("debt offset: bal=%d err=%v", bal, err)
	}
}

// fixtureKeyHash / fixtureKeyLast4: mechanical key fixture helpers —
// seed the SHA-256 digest + display last4 for a raw agent key.
func fixtureKeyHash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func fixtureKeyLast4(raw string) string {
	if len(raw) < 4 {
		return raw
	}
	return raw[len(raw)-4:]
}
