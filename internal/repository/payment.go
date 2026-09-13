package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// Payment persistence for tb_payments.
// Every method accepts a pg.Querier so it works with both *pgxpool.Pool and
// pgx.Tx; the paid transition (LockByCode / MarkPaid) is always called inside
// the payment service's transaction.

// PaymentSpec is the server-confirmed input for creating a pending payment.
type PaymentSpec struct {
	BotID           int64
	Provider        string
	ProviderOrderID *string
	AmountMinor     int64
	Currency        string
	Credits         float64
}

// CreatePendingPayment inserts a pending payment fact. Validation (positive
// amounts, provider/currency format, bot existence) belongs to the service
// layer; the DB CHECK constraints are the last line of defense.
func CreatePendingPayment(ctx context.Context, q pg.Querier, code string, spec PaymentSpec) (*model.Payment, error) {
	row := q.QueryRow(ctx, `
		INSERT INTO tb_payments (code, bot_id, provider, provider_order_id,
		                        amount_minor, currency, credits, status,
		                        created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', NOW(), NOW())
		RETURNING id, code, bot_id, provider, provider_order_id, amount_minor,
		          currency, credits, status, created_at, updated_at, paid_at`,
		code, spec.BotID, spec.Provider, spec.ProviderOrderID,
		spec.AmountMinor, spec.Currency, spec.Credits)

	return scanPayment(row)
}

// FindPaymentByCode returns a payment by its public code, or nil when absent.
func FindPaymentByCode(ctx context.Context, q pg.Querier, code string) (*model.Payment, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, provider, provider_order_id, amount_minor,
		       currency, credits, status, created_at, updated_at, paid_at
		FROM tb_payments
		WHERE code = $1`, code)
	p, err := scanPayment(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// LockPaymentByCode locks the payment row (SELECT ... FOR UPDATE) inside the
// caller's transaction — the serialization point of the paid transition.
// Returns nil (no error) when the code does not exist.
func LockPaymentByCode(ctx context.Context, tx pgx.Tx, code string) (*model.Payment, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, code, bot_id, provider, provider_order_id, amount_minor,
		       currency, credits, status, created_at, updated_at, paid_at
		FROM tb_payments
		WHERE code = $1
		FOR UPDATE`, code)
	p, err := scanPayment(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// MarkPaymentPaid flips pending -> paid and stamps paid_at, inside the
// caller's transaction. It never touches credits/balance/ledger.
func MarkPaymentPaid(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE tb_payments
		SET status = 'paid', paid_at = NOW(), updated_at = NOW()
		WHERE id = $1`, id)
	return err
}

// SetPaymentStatus records a terminal non-paid transition (failed/cancelled)
// from pending. Returns (false, nil) if the row was not in pending.
func SetPaymentStatus(ctx context.Context, q pg.Querier, code, status string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE tb_payments
		SET status = $2, updated_at = NOW()
		WHERE code = $1 AND status = 'pending'`, code, status)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// BindProviderOrder atomically associates a provider order with a payment,
// BEFORE any grant. Rules (row-locked):
//   - payment.provider must match the expected provider
//   - NULL provider_order_id  -> bind (first and only binding)
//   - same value              -> idempotent success
//   - different value         -> conflict (no grant)
//   - the (provider, provider_order_id) UNIQUE constraint additionally
//     guards against the same provider order funding two payments.
type BindProviderOrderResult int

const (
	BindProviderOrderBound BindProviderOrderResult = iota
	BindProviderOrderAlreadyBound
	BindProviderOrderConflict
)

// BindProviderOrderByCode locks the payment row and applies the binding
// rules above. Returns the outcome; error only for DB failures.
func BindProviderOrderByCode(ctx context.Context, tx pgx.Tx, code, provider, providerOrderID string) (BindProviderOrderResult, error) {
	var p model.Payment
	row := tx.QueryRow(ctx, `
		SELECT id, code, bot_id, provider, provider_order_id, amount_minor,
		       currency, credits, status, created_at, updated_at, paid_at
		FROM tb_payments
		WHERE code = $1
		FOR UPDATE`, code)
	if err := row.Scan(&p.ID, &p.Code, &p.BotID, &p.Provider, &p.ProviderOrderID,
		&p.AmountMinor, &p.Currency, &p.Credits, &p.Status,
		&p.CreatedAt, &p.UpdatedAt, &p.PaidAt); err != nil {
		if err == pgx.ErrNoRows {
			return BindProviderOrderConflict, fmt.Errorf("payment %s not found", code)
		}
		return BindProviderOrderConflict, err
	}
	if p.Provider != provider {
		return BindProviderOrderConflict, fmt.Errorf("payment provider %q, want %q", p.Provider, provider)
	}
	if p.ProviderOrderID != nil {
		if *p.ProviderOrderID == providerOrderID {
			return BindProviderOrderAlreadyBound, nil
		}
		return BindProviderOrderConflict, fmt.Errorf("payment %s already bound to provider order %s", code, *p.ProviderOrderID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tb_payments SET provider_order_id = $2, updated_at = NOW()
		WHERE id = $1`, p.ID, providerOrderID); err != nil {
		return BindProviderOrderConflict, err
	}
	return BindProviderOrderBound, nil
}

// ProviderOrderBelongsToAnotherPayment reports whether the (provider,
// provider_order_id) pair is already used by a DIFFERENT payment code.
// Called inside the same transaction before binding.
func ProviderOrderBelongsToAnotherPayment(ctx context.Context, tx pgx.Tx, provider, providerOrderID, exceptCode string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_payments
		WHERE provider = $1 AND provider_order_id = $2 AND code <> $3`,
		provider, providerOrderID, exceptCode).Scan(&n)
	return n > 0, err
}

// FindPaymentByCodeForBot returns the payment with the given code ONLY
// when it belongs to botID — ownership scoped inside the query, never
// post-filtered in Go. Returns nil when absent or owned by another bot.
func FindPaymentByCodeForBot(ctx context.Context, q pg.Querier, botID int64, code string) (*model.Payment, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, bot_id, provider, provider_order_id, amount_minor,
		       currency, credits, status, created_at, updated_at, paid_at
		FROM tb_payments
		WHERE code = $1 AND bot_id = $2`, code, botID)
	p, err := scanPayment(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// PaymentCodeExists reports whether a code is already used (for unique-code
// generation, mirroring the kungfu/task pattern).
func PaymentCodeExists(ctx context.Context, q pg.Querier, code string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tb_payments WHERE code = $1)`, code).Scan(&exists)
	return exists, err
}

func scanPayment(row pgx.Row) (*model.Payment, error) {
	var p model.Payment
	err := row.Scan(&p.ID, &p.Code, &p.BotID, &p.Provider, &p.ProviderOrderID,
		&p.AmountMinor, &p.Currency, &p.Credits, &p.Status,
		&p.CreatedAt, &p.UpdatedAt, &p.PaidAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}
