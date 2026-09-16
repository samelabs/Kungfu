package payment

// Creem refund/dispute adjustment facts: minimal event structures,
// reconciliation against the paid payment snapshot, and durable
// idempotent persistence into tb_payment_adjustments.
//
// FACTS ONLY this round: zero Credits mutation, zero balance change,
// no reversal execution. tb_payments.status stays "paid" — a refund or
// dispute is an independent follow-up fact bound to payment_id.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// -- minimal Creem fact structures --

// CreemTransactionFact is the shared transaction block inside refund and
// dispute objects. amount_paid is the actually-charged fiat (may include
// tax) and is deliberately NOT required to equal payment.amount_minor.
type CreemTransactionFact struct {
	ID             string `json:"id"`
	Amount         int64  `json:"amount"`
	AmountPaid     int64  `json:"amount_paid"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
	RefundedAmount *int64 `json:"refunded_amount"`
	Order          string `json:"order"`
}

// CreemRefundObject is the refund.created payload.
type CreemRefundObject struct {
	ID             string                `json:"id"`
	Status         string                `json:"status"`
	RefundAmount   int64                 `json:"refund_amount"`
	RefundCurrency string                `json:"refund_currency"`
	Reason         string                `json:"reason"`
	Transaction    *CreemTransactionFact `json:"transaction"`
}

// CreemDisputeObject is the dispute.created payload.
type CreemDisputeObject struct {
	ID          string                `json:"id"`
	Amount      int64                 `json:"amount"`
	Currency    string                `json:"currency"`
	Transaction *CreemTransactionFact `json:"transaction"`
}

// PaymentAdjustmentFact is the validated, reconciled fact ready to persist.
type PaymentAdjustmentFact = model.PaymentAdjustmentFact

// -- reconciliation --

// reconcileAdjustmentTransaction verifies the shared transaction fact
// against the paid payment snapshot: the provider order binding and the
// order-level amount/currency must match; amount_paid is only required
// positive (it may include tax, so equality with payment.amount_minor is
// NOT enforced).
func reconcileAdjustmentTransaction(txn *CreemTransactionFact, p *model.Payment) error {
	if txn == nil {
		return fmt.Errorf("transaction fact missing")
	}
	if txn.ID == "" {
		return fmt.Errorf("transaction.id empty")
	}
	if txn.Order == "" {
		return fmt.Errorf("transaction.order empty")
	}
	if p.ProviderOrderID == nil || *p.ProviderOrderID == "" {
		return fmt.Errorf("payment %s has no provider order binding", p.Code)
	}
	if txn.Order != *p.ProviderOrderID {
		return fmt.Errorf("transaction.order %q != payment provider order %q", txn.Order, *p.ProviderOrderID)
	}
	if txn.Amount != p.AmountMinor {
		return fmt.Errorf("transaction.amount %d != payment amount %d", txn.Amount, p.AmountMinor)
	}
	if txn.Currency != p.Currency {
		return fmt.Errorf("transaction.currency %q != payment currency %q", txn.Currency, p.Currency)
	}
	if txn.AmountPaid <= 0 {
		return fmt.Errorf("transaction.amount_paid %d not positive", txn.AmountPaid)
	}
	return nil
}

// buildRefundFact validates a refund.created object and reconciles it
// with the payment. Refund-specific rules: status must be "succeeded",
// the refund must not exceed what was actually paid, and the provider's
// cumulative refunded_amount must cover it without exceeding amount_paid.
func buildRefundFact(ev *CreemWebhookEvent, obj *CreemRefundObject, p *model.Payment) (*PaymentAdjustmentFact, error) {
	if err := reconcileAdjustmentTransaction(obj.Transaction, p); err != nil {
		return nil, err
	}
	if obj.ID == "" {
		return nil, fmt.Errorf("refund.id empty")
	}
	if obj.Status != "succeeded" {
		return nil, fmt.Errorf("refund.status %q, want succeeded", obj.Status)
	}
	if obj.RefundAmount <= 0 {
		return nil, fmt.Errorf("refund_amount %d not positive", obj.RefundAmount)
	}
	if obj.RefundCurrency != obj.Transaction.Currency {
		return nil, fmt.Errorf("refund_currency %q != transaction currency %q", obj.RefundCurrency, obj.Transaction.Currency)
	}
	if obj.RefundAmount > obj.Transaction.AmountPaid {
		return nil, fmt.Errorf("refund_amount %d exceeds amount_paid %d", obj.RefundAmount, obj.Transaction.AmountPaid)
	}
	if obj.Transaction.RefundedAmount == nil {
		return nil, fmt.Errorf("transaction.refunded_amount missing")
	}
	if *obj.Transaction.RefundedAmount < obj.RefundAmount {
		return nil, fmt.Errorf("refunded_amount %d < refund_amount %d", *obj.Transaction.RefundedAmount, obj.RefundAmount)
	}
	if *obj.Transaction.RefundedAmount > obj.Transaction.AmountPaid {
		return nil, fmt.Errorf("refunded_amount %d exceeds amount_paid %d", *obj.Transaction.RefundedAmount, obj.Transaction.AmountPaid)
	}
	pc, err := providerCreatedAt(ev)
	if err != nil {
		return nil, err
	}
	status := obj.Status
	reason := trimTo(obj.Reason, 64)
	return &PaymentAdjustmentFact{
		ProviderEventID:        ev.ID,
		Kind:                   "refund",
		Provider:               "creem",
		ProviderObjectID:       obj.ID,
		ProviderTransactionID:  obj.Transaction.ID,
		ProviderOrderID:        obj.Transaction.Order,
		AmountMinor:            obj.RefundAmount,
		Currency:               obj.RefundCurrency,
		TransactionAmountMinor: obj.Transaction.Amount,
		AmountPaidMinor:        obj.Transaction.AmountPaid,
		RefundedAmountMinor:    obj.Transaction.RefundedAmount,
		ObjectStatus:           &status,
		TransactionStatus:      &obj.Transaction.Status,
		Reason:                 reason,
		ProviderCreatedAt:      pc,
	}, nil
}

// buildDisputeFact validates a dispute.created object. The transaction
// status is only required non-empty and stored verbatim — no hardcoded
// chargeback vocabulary, provider representations differ.
func buildDisputeFact(ev *CreemWebhookEvent, obj *CreemDisputeObject, p *model.Payment) (*PaymentAdjustmentFact, error) {
	if err := reconcileAdjustmentTransaction(obj.Transaction, p); err != nil {
		return nil, err
	}
	if obj.ID == "" {
		return nil, fmt.Errorf("dispute.id empty")
	}
	if obj.Amount <= 0 {
		return nil, fmt.Errorf("dispute.amount %d not positive", obj.Amount)
	}
	if obj.Currency != obj.Transaction.Currency {
		return nil, fmt.Errorf("dispute.currency %q != transaction currency %q", obj.Currency, obj.Transaction.Currency)
	}
	if obj.Transaction.Status == "" {
		return nil, fmt.Errorf("dispute transaction.status empty")
	}
	pc, err := providerCreatedAt(ev)
	if err != nil {
		return nil, err
	}
	var nilStatus *string
	return &PaymentAdjustmentFact{
		ProviderEventID:        ev.ID,
		Kind:                   "dispute",
		Provider:               "creem",
		ProviderObjectID:       obj.ID,
		ProviderTransactionID:  obj.Transaction.ID,
		ProviderOrderID:        obj.Transaction.Order,
		AmountMinor:            obj.Amount,
		Currency:               obj.Currency,
		TransactionAmountMinor: obj.Transaction.Amount,
		AmountPaidMinor:        obj.Transaction.AmountPaid,
		RefundedAmountMinor:    obj.Transaction.RefundedAmount,
		ObjectStatus:           nilStatus,
		TransactionStatus:      &obj.Transaction.Status,
		Reason:                 nil,
		ProviderCreatedAt:      pc,
	}, nil
}

// providerCreatedAt returns the provider's own creation timestamp.
// A missing or non-positive created_at is a reconciliation failure —
// we never substitute local time for a provider fact.
func providerCreatedAt(ev *CreemWebhookEvent) (int64, error) {
	if ev.CreatedAt <= 0 {
		return 0, fmt.Errorf("event created_at %d not positive (provider fact missing)", ev.CreatedAt)
	}
	return ev.CreatedAt, nil
}

func trimTo(s string, n int) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) > n {
		s = s[:n]
	}
	return &s
}

// -- persistence (idempotent, row-locked) --

// RecordPaymentAdjustment persists one reconciled adjustment fact.
// Idempotency: the same (provider, provider_event_id) or the same
// (provider, kind, provider_object_id) already present → success with
// inserted=false and ZERO new rows. The payment row is locked FOR UPDATE
// while the fact lands, and tb_payments.status is never touched.
// Different refund/dispute objects leave independent facts.
func RecordPaymentAdjustment(ctx context.Context, pool *pg.Pool, fact *PaymentAdjustmentFact) (inserted bool, err error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin adjustment tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the payment row: serializes adjustment writes per payment.
	p, err := repository.LockPaymentByID(ctx, tx, fact.PaymentID)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, errors.New(404, "PAYMENT_NOT_FOUND", "Payment not found")
	}
	if p.Status != model.PaymentStatusPaid {
		return false, errors.New(409, "PAYMENT_NOT_PAID", "Adjustments apply only to paid payments")
	}

	inserted, err = repository.InsertPaymentAdjustment(ctx, tx, fact)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit adjustment tx: %w", err)
	}
	return inserted, nil
}

// HandleCreemAdjustmentEvent is the webhook entry: parse, reconcile
// against the payment found by provider order binding, persist. Zero
// Credits mutation — this round records facts only.
func HandleCreemAdjustmentEvent(ctx context.Context, pool *pg.Pool, ev *CreemWebhookEvent) error {
	switch ev.EventType {
	case "refund.created":
		var obj CreemRefundObject
		if err := json.Unmarshal(ev.Object, &obj); err != nil {
			return fmt.Errorf("refund object decode: %w", err)
		}
		p, err := findPaymentByProviderOrder(ctx, pool, obj.Transaction)
		if err != nil {
			return err
		}
		fact, err := buildRefundFact(ev, &obj, p)
		if err != nil {
			return err
		}
		fact.PaymentID = p.ID
		_, err = RecordPaymentAdjustment(ctx, pool, fact)
		return err
	case "dispute.created":
		var obj CreemDisputeObject
		if err := json.Unmarshal(ev.Object, &obj); err != nil {
			return fmt.Errorf("dispute object decode: %w", err)
		}
		p, err := findPaymentByProviderOrder(ctx, pool, obj.Transaction)
		if err != nil {
			return err
		}
		fact, err := buildDisputeFact(ev, &obj, p)
		if err != nil {
			return err
		}
		fact.PaymentID = p.ID
		_, err = RecordPaymentAdjustment(ctx, pool, fact)
		return err
	default:
		return fmt.Errorf("not an adjustment event: %s", ev.EventType)
	}
}

// findPaymentByProviderOrder resolves the payment via the provider order
// binding (provider = creem, provider_order_id = transaction.order).
func findPaymentByProviderOrder(ctx context.Context, pool *pg.Pool, txn *CreemTransactionFact) (*model.Payment, error) {
	if txn == nil || txn.Order == "" {
		return nil, errors.New(400, "RECONCILIATION_FAILED", "Transaction order missing")
	}
	p, err := repository.FindPaymentByProviderOrder(ctx, pool, "creem", txn.Order)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Could not load payment")
	}
	if p == nil {
		return nil, errors.New(404, "RECONCILIATION_FAILED", "No payment for this provider order")
	}
	if p.Status != model.PaymentStatusPaid {
		return nil, errors.New(409, "RECONCILIATION_FAILED", "Payment is not paid")
	}
	return p, nil
}
