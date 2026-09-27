// Package credits owns the credit-ledger domain: balance mutations, ledger
// transactions, balance reads, the insufficient-credit invariant, and the
// DB transaction / row-lock mechanism behind them.
//
// Credits are WHOLE INTEGER units (int64). There is no fractional credit
// anywhere in this package: NaN/Inf are structurally impossible, and the
// numeric hazards are overflow and division — both guarded explicitly.
//
// Boundary rules:
//   - this package is the ONLY place that writes tb_bots.balance or inserts
//     into tb_transactions;
//   - consumers (task, kungfu, registration, future payment/rewards/storage)
//     call credits.Record / credits.Balance and never touch balance SQL;
//   - this package must not import any business domain (task, kungfu,
//     registration, server) — dependency direction is one-way.
//
// Business pricing/reward policy (AmountTask, AmountPush, AmountGet) stays
// with the consuming domains, not here.
package credits

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// maxBalance is the largest representable balance. Balance storage is
// BIGINT; the guard below this constant keeps newBalance provably inside
// int64 so the ledger row can never be written with an overflowed value.
const maxBalance = math.MaxInt64

// errOverflow reports an integer Credits operation that would leave the
// int64 range. It fails closed BEFORE any mutation.
var errOverflow = fmt.Errorf("credit amount overflows int64")

// checkAdd reports whether a + b stays inside int64 without overflow.
func checkAdd(a, b int64) (int64, error) {
	s := a + b
	// Overflow changes the sign relationship between operands and result.
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, errOverflow
	}
	return s, nil
}

// Record records a credit transaction and updates the bot's balance — the
// single balance-mutation primitive for ordinary flows.
//
// Transaction nesting: when called with an existing transaction (tx != nil)
// it joins it (caller owns commit/rollback); without one it starts, commits,
// and on error rolls back its own transaction. The bot row is locked with
// SELECT ... FOR UPDATE, then balance is updated and the ledger row inserted
// inside that same transaction.
//
// Ordinary rules: any amount >= 0 is always accepted (a negative resulting
// balance from a POSITIVE amount is fine — positive credits reduce debt);
// an amount < 0 that would push the balance below zero is rejected with the
// 402 INSUFFICIENT_CREDITS contract. Authoritative negative-balance
// capability lives ONLY in RecordAuthoritativeReversal.
func Record(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	txnType string, amount int64, refType, refID *string) (int64, error) {

	// Ordinary debit gate: a negative amount may never make the balance
	// negative. Positive amounts always pass (debt offset).
	if amount < 0 {
		// checked inside record() against the locked current balance
		return record(ctx, pool, tx, botID, txnType, amount, refType, refID, false)
	}
	return record(ctx, pool, tx, botID, txnType, amount, refType, refID, true)
}

// RecordAuthoritativeReversal is the NARROW exceptional primitive backing
// Payment authoritative reversal: it must be a strictly negative amount and
// is allowed to drive the balance BELOW zero (debt), because a provider
// refund/dispute claws back credits the provider has already returned in
// fiat. Production callers are restricted to internal/payment (see the
// architecture guard test); no boolean capability switch is exposed to
// business domains.
func RecordAuthoritativeReversal(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	txnType string, amount int64, refType, refID *string) (int64, error) {

	if amount >= 0 {
		return 0, fmt.Errorf("authoritative reversal requires a negative amount, got %d", amount)
	}
	return record(ctx, pool, tx, botID, txnType, amount, refType, refID, true)
}

// record is the single internal implementation shared by Record and
// RecordAuthoritativeReversal. allowNegative is never exposed outside this
// package: false = reject a resulting negative balance (ordinary debit
// contract), true = allow it (authoritative reversal only).
func record(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	txnType string, amount int64, refType, refID *string, allowNegative bool) (int64, error) {

	useTx, startedNew, err := pool.BeginOrUse(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}

	// only rollback if we started it
	defer func() {
		if startedNew {
			// Only reached if we return an error before commit
			_ = pg.RollbackOrSkip(useTx, true)
		}
	}()

	var currentBalance int64
	err = useTx.QueryRow(ctx,
		`SELECT balance FROM tb_bots WHERE id = $1 FOR UPDATE`, botID).Scan(&currentBalance)
	if err != nil {
		return 0, fmt.Errorf("bot not found")
	}

	newBalance, err := checkAdd(currentBalance, amount)
	if err != nil {
		// Integer overflow guard — fails closed before any mutation.
		return 0, errors.New(500, "CREDIT_OVERFLOW",
			"Credit operation would overflow the ledger range")
	}

	if newBalance < 0 && !allowNegative {
		return 0, errors.New(402, "INSUFFICIENT_CREDITS",
			fmt.Sprintf("Insufficient credits. Need %d, have %d", absInt(amount), currentBalance))
	}

	_, err = useTx.Exec(ctx,
		`UPDATE tb_bots SET balance = $1, updated_at = NOW() WHERE id = $2`, newBalance, botID)
	if err != nil {
		return 0, fmt.Errorf("update balance: %w", err)
	}

	_, err = useTx.Exec(ctx, `
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		botID, txnType, amount, newBalance, refType, refID)
	if err != nil {
		return 0, fmt.Errorf("insert transaction: %w", err)
	}

	if startedNew {
		if err := useTx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit transaction: %w", err)
		}
	}

	return newBalance, nil
}

// Balance returns the current balance without modifying it. Errors propagate:
// a DB failure or a missing bot returns an error (never a fake 0). A genuine
// zero balance returns (0, nil).
func Balance(ctx context.Context, q pg.Querier, botID int64) (int64, error) {
	var balance int64
	err := q.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id = $1`, botID).Scan(&balance)
	if err != nil {
		return 0, err
	}
	return balance, nil
}

func absInt(i int64) int64 {
	if i < 0 {
		return -i
	}
	return i
}

// SumAmountByTypeRef returns the SUM of transaction amounts for one bot
// with the given txn type and reference. This is the ONLY sanctioned way
// for other domains to read aggregate ledger facts about their own
// references — they never query tb_transactions directly. Works with a
// pool or a caller-owned transaction (querier), so payment reversal can
// read it INSIDE its locked transaction. A SUM that leaves the int64
// range fails closed instead of wrapping.
func SumAmountByTypeRef(ctx context.Context, q pg.Querier, botID int64,
	txnType string, refType, refID string) (int64, error) {

	var sum int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM tb_transactions
		WHERE bot_id = $1 AND type = $2 AND ref_type = $3 AND ref_id = $4`,
		botID, txnType, refType, refID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum transactions: %w", err)
	}
	return sum, nil
}
