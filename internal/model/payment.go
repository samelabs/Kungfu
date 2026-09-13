package model

import "time"

// Payment status values (state machine, see internal/payment).
const (
	PaymentStatusPending   = "pending"
	PaymentStatusPaid      = "paid"
	PaymentStatusFailed    = "failed"
	PaymentStatusCancelled = "cancelled"
)

// Payment is a single row of tb_payments.
// Monetary amount is stored as integer minor units (amount_minor, e.g. cents);
// credits uses NUMERIC(20,4) like the rest of the ledger.
type Payment struct {
	ID              int64      `db:"id" json:"id"`
	Code            string     `db:"code" json:"code"`
	BotID           int64      `db:"bot_id" json:"bot_id"`
	Provider        string     `db:"provider" json:"provider"`
	ProviderOrderID *string    `db:"provider_order_id" json:"provider_order_id,omitempty"`
	AmountMinor     int64      `db:"amount_minor" json:"amount_minor"`
	Currency        string     `db:"currency" json:"currency"`
	Credits         float64    `db:"credits" json:"credits"`
	Status          string     `db:"status" json:"status"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updated_at"`
	PaidAt          *time.Time `db:"paid_at" json:"paid_at,omitempty"`
}
