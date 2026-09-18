package credits

// Finite-number invariant tests: NaN/±Inf can never produce a balance
// mutation or a ledger row, and a non-finite persisted balance is an
// error, never a "valid" balance. Real PostgreSQL.

import (
	"context"
	"crypto/sha256"
	"math"
	"os"
	"strings"
	"testing"
	"time"

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

func finSeedBot(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"fin_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
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

// Record(NaN/±Inf) → zero mutation, zero ledger, error — application
// invariant rejection, not a DB error.
func TestRecordRejectsNonFiniteAmounts(t *testing.T) {
	pool := finTestPool(t)
	botID := finSeedBot(t, pool, 100)

	rt := "test"
	for name, amount := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		_, err := Record(context.Background(), pool, nil, botID, "grant_signup", amount, &rt, nil)
		if err == nil {
			t.Fatalf("%s amount accepted", name)
		}
	}

	var balance float64
	_ = pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id = $1`, botID).Scan(&balance)
	if balance != 100 {
		t.Fatalf("balance mutated by non-finite record: %v", balance)
	}
	if n := finLedgerCount(t, pool, botID); n != 0 {
		t.Fatalf("ledger rows after non-finite record: %d", n)
	}
}

// Record of a finite amount on a bot whose persisted balance is
// non-finite (corrupted row) → error, no mutation.
func TestRecordRejectsNonFinitePersistedBalance(t *testing.T) {
	pool := finTestPool(t)
	botID := finSeedBot(t, pool, 0)

	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE tb_bots SET balance = 'NaN'::numeric WHERE id = $1`, botID); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}

	rt := "test"
	if _, err := Record(ctx, pool, nil, botID, "grant_signup", 10, &rt, nil); err == nil {
		t.Fatal("record against NaN balance accepted")
	}
}

// Balance() reading a non-finite persisted balance → error, not a value.
func TestBalanceRejectsNonFinitePersisted(t *testing.T) {
	pool := finTestPool(t)
	botID := finSeedBot(t, pool, 0)

	ctx := context.Background()
	// numeric(20,4) rejects 'Infinity' outright (DB-domain defense);
	// 'NaN' persists — Balance must refuse to return it as a value.
	if _, err := pool.Exec(ctx,
		`UPDATE tb_bots SET balance = 'NaN'::numeric WHERE id = $1`, botID); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}
	if _, err := Balance(ctx, pool, botID); err == nil {
		t.Fatal("Balance(NaN) must be an error")
	}

	// finite balance still reads normally
	finBot := finSeedBot(t, pool, 42)
	if b, err := Balance(ctx, pool, finBot); err != nil || b != 42 {
		t.Fatalf("finite balance read: %v %v", b, err)
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
