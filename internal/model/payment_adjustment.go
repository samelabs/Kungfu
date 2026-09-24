package model

// PaymentAdjustmentFact is one validated, reconciled provider-side
// refund/dispute fact ready to persist into tb_payment_adjustments.
// Facts only: no credits delta, no balance impact, no status machine.
type PaymentAdjustmentFact struct {
	PaymentID              int64
	ProviderEventID        string
	Kind                   string // "refund" | "dispute"
	Provider               string
	ProviderObjectID       string
	ProviderTransactionID  string
	ProviderOrderID        string
	AmountMinor            int64
	Currency               string
	TransactionAmountMinor int64
	AmountPaidMinor        int64
	RefundedAmountMinor    *int64
	ObjectStatus           *string
	TransactionStatus      *string
	Reason                 *string
	ProviderCreatedAt      int64
}
