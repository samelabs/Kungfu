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

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// Record records a credit transaction and updates the bot's balance — the
// single balance-mutation primitive.
//
// Transaction nesting: when called with an existing transaction (tx != nil)
// it joins it (caller owns commit/rollback); without one it starts, commits,
// and on error rolls back its own transaction. The bot row is locked with
// SELECT ... FOR UPDATE, then balance is updated and the ledger row inserted
// inside that same transaction. A negative resulting balance is rejected with
// the 402 INSUFFICIENT_CREDITS contract.
func Record(ctx context.Context, pool *pg.Pool, tx pgx.Tx, botID int64,
	txnType string, amount float64, refType, refID *string) (float64, error) {

	useTx, startedNew, err := pool.BeginOrUse(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}

	// only rollback if we started it
	defer func() {
		if startedNew {
			// Only reached if we return an error before commit
			_ = pg.RollbackOrSkip(ctx, useTx, true)
		}
	}()

	var currentBalance float64
	err = useTx.QueryRow(ctx,
		`SELECT balance FROM tb_bots WHERE id = $1 FOR UPDATE`, botID).Scan(&currentBalance)
	if err != nil {
		return 0, fmt.Errorf("bot not found")
	}

	newBalance := currentBalance + amount

	if newBalance < 0 {
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
	return balance, nil
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
