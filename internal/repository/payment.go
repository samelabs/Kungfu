package repository

import (
	"context"

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
