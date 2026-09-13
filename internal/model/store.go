package model

import "time"

// Store product status values.
const (
	ProductStatusActive   = "active"
	ProductStatusInactive = "inactive"
)

// Redemption status values (state machine, see internal/store).
const (
	RedemptionStatusPendingReview = "pending_review"
	RedemptionStatusApproved      = "approved"
	RedemptionStatusRejected      = "rejected"
	RedemptionStatusFulfilled     = "fulfilled"
	RedemptionStatusCancelled     = "cancelled"
)

// StoreProduct is a single row of tb_store_products (virtual goods only).
type StoreProduct struct {
	ID           int64     `db:"id" json:"id"`
	Code         string    `db:"code" json:"code"`
	Title        string    `db:"title" json:"title"`
	Description  *string   `db:"description" json:"description,omitempty"`
	CreditsPrice float64   `db:"credits_price" json:"credits_price"`
	Status       string    `db:"status" json:"status"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// Redemption is a single row of tb_redemptions. ProductTitle and
// CreditsCost are immutable snapshots taken at redemption time — later
// product price changes or deactivation never rewrite history.
type Redemption struct {
	ID              int64      `db:"id" json:"id"`
	Code            string     `db:"code" json:"code"`
	BotID           int64      `db:"bot_id" json:"bot_id"`
	ProductID       int64      `db:"product_id" json:"product_id"`
	ProductTitle    string     `db:"product_title" json:"product_title"`
	CreditsCost     float64    `db:"credits_cost" json:"credits_cost"`
	RequestKey      string     `db:"request_key" json:"request_key"`
	Status          string     `db:"status" json:"status"`
	ReviewNote      *string    `db:"review_note" json:"review_note,omitempty"`
	FulfillmentNote *string    `db:"fulfillment_note" json:"fulfillment_note,omitempty"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updated_at"`
	ReviewedAt      *time.Time `db:"reviewed_at" json:"reviewed_at,omitempty"`
	FulfilledAt     *time.Time `db:"fulfilled_at" json:"fulfilled_at,omitempty"`
	CancelledAt     *time.Time `db:"cancelled_at" json:"cancelled_at,omitempty"`
}
