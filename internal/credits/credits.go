// Package credits owns the credit-ledger domain: balance mutations, ledger
// transactions, balance reads, the insufficient-credit invariant, and the
// DB transaction / row-lock mechanism behind them.
//
// Boundary rules:
//   - this package is the ONLY place that writes tb_bots.balance or inserts
//     into tb_transactions;
//   - consumers (task, kungfu, registration, future payment/store/storage)
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

// ErrNonFinite is returned when a credit amount or balance is NaN/±Inf —
// the final hard gate before any balance mutation or ledger insert.
var ErrNonFinite = fmt.Errorf("non-finite credit value")

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
	txnType string, amount float64, refType, refID *string) (float64, error) {

	// Ordinary debit gate: a negative amount may never make the balance
	// negative. Positive amounts always pass (debt offset).
	if amount < 0 {
		// checked inside record() against the locked current balance
		return record(ctx, pool, tx, botID, txnType, amount, refType, refID, false)
	}
	return record(ctx, pool, tx, botID, txnType, amount, refType, refID, true)
}

// RecordAuthoritativeReversal is the NARROW exceptional primitive backing
// Payment authoritative reversal: it must be a negative, finite amount and
// is allowed to drive the balance BELOW zero (debt), because a provider
// refund/dispute claws back credits the provider has already returned in
// fiat. Production callers are restricted to internal/payment (see the
// architecture guard test); no boolean capability switch is exposed to
// business domains.
func RecordAuthoritativeReversal(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	txnType string, amount float64, refType, refID *string) (float64, error) {

	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, ErrNonFinite
	}
	if amount >= 0 {
		return 0, fmt.Errorf("authoritative reversal requires a negative amount, got %v", amount)
	}
	return record(ctx, pool, tx, botID, txnType, amount, refType, refID, true)
}

// record is the single internal implementation shared by Record and
// RecordAuthoritativeReversal. allowNegative is never exposed outside this
// package: false = reject a resulting negative balance (ordinary debit
// contract), true = allow it (authoritative reversal only).
func record(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	txnType string, amount float64, refType, refID *string, allowNegative bool) (float64, error) {

	// Finite invariant — the final hard gate: NaN/±Inf never reaches a
	// balance UPDATE or a ledger INSERT.
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, ErrNonFinite
	}

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

	var currentBalance float64
	err = useTx.QueryRow(ctx,
		`SELECT balance FROM tb_bots WHERE id = $1 FOR UPDATE`, botID).Scan(&currentBalance)
	if err != nil {
		return 0, fmt.Errorf("bot not found")
	}
	if math.IsNaN(currentBalance) || math.IsInf(currentBalance, 0) {
		return 0, ErrNonFinite
	}

	newBalance := currentBalance + amount
	if math.IsNaN(newBalance) || math.IsInf(newBalance, 0) {
		return 0, ErrNonFinite
	}

	if newBalance < 0 && !allowNegative {
		return 0, errors.New(402, "INSUFFICIENT_CREDITS",
			fmt.Sprintf("Insufficient credits. Need %v, have %v", absFloat(amount), currentBalance))
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
func Balance(ctx context.Context, q pg.Querier, botID int64) (float64, error) {
	var balance float64
	err := q.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id = $1`, botID).Scan(&balance)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(balance) || math.IsInf(balance, 0) {
		return 0, ErrNonFinite
	}
	return balance, nil
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// SumAmountByTypeRef returns the SUM of transaction amounts for one bot
// with the given txn type and reference. This is the ONLY sanctioned way
// for other domains to read aggregate ledger facts about their own
// references — they never query tb_transactions directly. Works with a
// pool or a caller-owned transaction (querier), so payment reversal can
// read it INSIDE its locked transaction.
func SumAmountByTypeRef(ctx context.Context, q pg.Querier, botID int64,
	txnType string, refType, refID string) (float64, error) {

	var sum float64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM tb_transactions
		WHERE bot_id = $1 AND type = $2 AND ref_type = $3 AND ref_id = $4`,
		botID, txnType, refType, refID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum transactions: %w", err)
	}
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return 0, ErrNonFinite
	}
	return sum, nil
}
