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
	"math"
	"strings"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"

	"github.com/jackc/pgx/v5"
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
	// refunded_amount gate: nil → durable fact, no reversal target
	// contribution; non-nil must be within [0, amount_paid].
	if obj.Transaction.RefundedAmount != nil {
		if *obj.Transaction.RefundedAmount < 0 || *obj.Transaction.RefundedAmount > obj.Transaction.AmountPaid {
			return nil, fmt.Errorf("dispute refunded_amount %d outside [0, %d]",
				*obj.Transaction.RefundedAmount, obj.Transaction.AmountPaid)
		}
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

// round4 mirrors the NUMERIC(20,4) ledger precision: values are rounded
// to 4 decimal places at the reversal computation boundary.
func round4(x float64) float64 {
	return math.Round(x*10000) / 10000
}

// adjustmentBasis is the canonical provider-transaction basis persisted
// with a payment's adjustment facts.
type adjustmentBasis struct {
	providerTransactionID string
	amountPaid            int64
}

// assertBasisConsistency enforces that every adjustment fact on one
// payment shares the same provider transaction basis: the ratio
// computation depends on a single denominator, so a differing basis is
// a reconciliation failure — never auto-chosen.
func assertBasisConsistency(ctx context.Context, tx pgx.Tx, paymentID int64, incoming adjustmentBasis) error {
	var existingTxnID string
	var existingPaid int64
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT provider_transaction_id, amount_paid_minor, COUNT(*)
		FROM tb_payment_adjustments WHERE payment_id = $1
		GROUP BY provider_transaction_id, amount_paid_minor`, paymentID).Scan(&existingTxnID, &existingPaid, &n); err != nil {
		if err == pgx.ErrNoRows {
			return nil // no prior facts: incoming establishes the basis
		}
		return err
	}
	if existingTxnID != incoming.providerTransactionID {
		return fmt.Errorf("adjustment basis conflict: existing provider_transaction_id %q, incoming %q", existingTxnID, incoming.providerTransactionID)
	}
	if existingPaid != incoming.amountPaid {
		return fmt.Errorf("adjustment basis conflict: existing amount_paid %d, incoming %d", existingPaid, incoming.amountPaid)
	}
	return nil
}

// maxRefundedAndBasis reads the cumulative provider refund authority:
// MAX(refunded_amount_minor) across the payment's persisted facts, plus
// the canonical basis (any row — consistency already asserted).
func maxRefundedAndBasis(ctx context.Context, tx pgx.Tx, paymentID int64) (maxRefunded int64, basis adjustmentBasis, found bool, err error) {
	rows, err := tx.Query(ctx, `
		SELECT COALESCE(refunded_amount_minor, 0), provider_transaction_id, amount_paid_minor
		FROM tb_payment_adjustments WHERE payment_id = $1`, paymentID)
	if err != nil {
		return 0, adjustmentBasis{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var r int64
		var txnID string
		var paid int64
		if err := rows.Scan(&r, &txnID, &paid); err != nil {
			return 0, adjustmentBasis{}, false, err
		}
		if r > maxRefunded {
			maxRefunded = r
		}
		basis = adjustmentBasis{providerTransactionID: txnID, amountPaid: paid}
		found = true
	}
	return maxRefunded, basis, found, nil
}

// RecordPaymentAdjustment persists one reconciled adjustment fact AND
// advances the payment's Credits reversal atomically, in ONE PostgreSQL
// transaction:
//
//	BEGIN
//	Lock payment FOR UPDATE
//	Confirm paid
//	Assert adjustment basis consistency
//	INSERT adjustment fact ON CONFLICT DO NOTHING
//	Read persisted facts: MAX(refunded_amount), canonical basis
//	target = round4(credits * MAX_refunded / amount_paid)   [R==P → C]
//	already_reversed = -SUM(reverse_payment ledger for this payment)
//	delta = round4(target - already_reversed)
//	if delta > 0 → credits.RecordAuthoritativeReversal(-delta)
//	COMMIT
//
// Duplicate events: inserted=false but the target/ledger computation
// STILL runs, so A1-era facts redelivered by Creem catch up (reversal
// executes once). Any failure rolls back fact + balance + ledger
// together. tb_payments.status is never touched.
func RecordPaymentAdjustment(ctx context.Context, pool *pg.Pool, fact *PaymentAdjustmentFact) (inserted bool, err error) {
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin adjustment tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the payment row: serializes adjustment + reversal per payment.
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

	incoming := adjustmentBasis{
		providerTransactionID: fact.ProviderTransactionID,
		amountPaid:            fact.AmountPaidMinor,
	}
	if err := assertBasisConsistency(ctx, tx, fact.PaymentID, incoming); err != nil {
		return false, fmt.Errorf("reconciliation: %w", err)
	}

	inserted, err = repository.InsertPaymentAdjustment(ctx, tx, fact)
	if err != nil {
		return false, err
	}

	// Cumulative provider authority from PERSISTED facts (duplicates and
	// A1-era facts included).
	maxRefunded, basis, found, err := maxRefundedAndBasis(ctx, tx, fact.PaymentID)
	if err != nil {
		return false, err
	}
	if !found {
		// No fact persisted (should not happen after an insert, but fail
		// closed rather than reverse on nothing).
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit adjustment tx: %w", err)
		}
		return inserted, nil
	}

	// Defensive invariants on the provider basis.
	if basis.amountPaid <= 0 {
		return false, fmt.Errorf("reconciliation: amount_paid %d not positive", basis.amountPaid)
	}
	if maxRefunded < 0 || maxRefunded > basis.amountPaid {
		return false, fmt.Errorf("reconciliation: refunded %d outside [0, %d]", maxRefunded, basis.amountPaid)
	}

	// Target cumulative reversal. R == P → exactly the original credits.
	target := p.Credits
	if maxRefunded != basis.amountPaid {
		target = round4(p.Credits * float64(maxRefunded) / float64(basis.amountPaid))
	}

	// Already reversed, from the Credits ledger (never a direct
	// tb_transactions query from this domain).
	sum, err := credits.SumAmountByTypeRef(ctx, tx, p.BotID, "reverse_payment", "payment", p.Code)
	if err != nil {
		return false, fmt.Errorf("ledger read: %w", err)
	}
	if sum > 0 {
		return false, fmt.Errorf("reconciliation: reverse_payment ledger sum %v is positive", sum)
	}
	alreadyReversed := -sum
	if alreadyReversed < 0 || alreadyReversed > p.Credits {
		return false, fmt.Errorf("reconciliation: already_reversed %v outside [0, %v]", alreadyReversed, p.Credits)
	}

	if delta := round4(target - alreadyReversed); delta > 0 {
		refType := "payment"
		if _, err := credits.RecordAuthoritativeReversal(ctx, pool, tx, p.BotID,
			"reverse_payment", -delta, &refType, &p.Code); err != nil {
			return false, fmt.Errorf("credits reversal: %w", err)
		}
	}
	// delta <= 0 → zero reversal, never a compensating positive delta.

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
