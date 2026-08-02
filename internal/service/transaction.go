package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// Credit amounts — credit costs.
const (
	AmountTask = 1.0  // earn_task reward
	AmountPush = -1.0 // spend_push cost
	AmountGet  = -1.0 // spend_get cost
)

// Record records a credit transaction and updates the bot's balance.

// Transaction nesting pattern (critical):
// If called within an existing transaction (tx != nil), it does NOT start a new one.
// If called without a transaction (tx == nil), it starts its own BEGIN/COMMIT.

//

//	$startedTransaction = !$db->inTransaction();
//	if ($startedTransaction) { $db->beginTransaction(); }
//	...
//	if ($startedTransaction) { $db->commit(); }

// The lock uses SELECT ... FOR UPDATE on the bot row, .
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

	// SELECT balance FROM tb_bots WHERE id = ? FOR UPDATE
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

	// UPDATE tb_bots SET balance = ? WHERE id = ?
	_, err = useTx.Exec(ctx,
		`UPDATE tb_bots SET balance = $1, updated_at = NOW() WHERE id = $2`, newBalance, botID)
	if err != nil {
		return 0, fmt.Errorf("update balance: %w", err)
	}

	// INSERT INTO tb_transactions
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

// GetBalance returns the current balance without modifying it.
func GetBalance(ctx context.Context, pool *pg.Pool, botID int64) float64 {
	balance, err := repository.FindBalanceByBotID(ctx, pool, botID)
	if err != nil {
		return 0.0
	}
	return balance
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
