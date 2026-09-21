package payment

// Creem refund/dispute adjustment facts: minimal event structures,
// reconciliation against the paid payment snapshot, and durable
// idempotent persistence into tb_payment_adjustments together with the
// cumulative authoritative Credits reversal. tb_payments.status stays
// "paid" — refund/dispute remains an independent fact bound to
// payment_id, but its persisted cumulative refunded_amount authorizes
// the reverse_payment ledger (which may drive the balance negative).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"strings"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"

	"github.com/jackc/pgx/v5"
)

// -- minimal Creem fact structures --

// CreemRefundObject is the refund.created payload — ONLY the fields the
// official webhook schema guarantees. The embedded transaction block is
// an IDENTIFIER, never an economic authority: cumulative refunded_amount
// and amount_paid come from the authoritative TransactionEntity fetched
// via GET /v1/transactions.
type CreemRefundObject struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	RefundAmount int64  `json:"refund_amount"`
	Reason       string `json:"reason"`
	// TransactionBlock carries only the provider transaction ID (the
	// schema guarantees id; other members are not relied upon).
	TransactionBlock *struct {
		ID string `json:"id"`
	} `json:"transaction"`
}

// CreemDisputeObject is the dispute.created payload — only guaranteed
// fields. The dispute amount semantics (tax-inclusive chargeback amount)
// are display facts; the reversal authority is the authoritative
// transaction's refunded_amount (Creem records chargebacks there with
// transaction.status="chargeback").
type CreemDisputeObject struct {
	ID     string `json:"id"`
	Amount int64  `json:"amount"`
	// TransactionBlock: provider transaction ID only.
	TransactionBlock *struct {
		ID string `json:"id"`
	} `json:"transaction"`
}

// PaymentAdjustmentFact is the validated, reconciled fact ready to persist.
type PaymentAdjustmentFact = model.PaymentAdjustmentFact

// -- reconciliation --

// reconcileAuthoritativeTransaction verifies the authoritative
// TransactionEntity (GET /v1/transactions) against the paid payment
// snapshot: the provider order binding and the order-level
// amount/currency must match; amount_paid must be present and positive
// (it may include tax, so equality with payment.amount_minor is NOT
// enforced); refunded_amount must be present for a reversal basis.
func reconcileAuthoritativeTransaction(txn *CreemTransactionEntity, p *model.Payment) error {
	if txn == nil {
		return fmt.Errorf("authoritative transaction missing")
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
	if txn.AmountPaid == nil || *txn.AmountPaid <= 0 {
		return fmt.Errorf("transaction.amount_paid missing or not positive")
	}
	return nil
}

// requireRefundedBasis is the refund-specific gate: the cumulative
// refunded amount must be present. Disputes do not require it — a
// dispute with no refund movement is stored as a durable fact with
// zero reversal contribution.
func requireRefundedBasis(txn *CreemTransactionEntity) error {
	if txn.RefundedAmount == nil {
		return fmt.Errorf("transaction.refunded_amount missing")
	}
	return nil
}

// buildRefundFact validates a refund.created webhook object RECONCILED
// with the authoritative transaction. The webhook contributes the refund
// identity and its nominal refund_amount; every economic field
// (amount_paid, cumulative refunded_amount) comes from the authoritative
// TransactionEntity. refund_amount is cross-checked against the
// authoritative cumulative refunded amount.
func buildRefundFact(ev *CreemWebhookEvent, obj *CreemRefundObject, txn *CreemTransactionEntity, p *model.Payment) (*PaymentAdjustmentFact, error) {
	if err := reconcileAuthoritativeTransaction(txn, p); err != nil {
		return nil, err
	}
	if err := requireRefundedBasis(txn); err != nil {
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
	if *txn.RefundedAmount < obj.RefundAmount {
		return nil, fmt.Errorf("authoritative refunded_amount %d < refund_amount %d",
			*txn.RefundedAmount, obj.RefundAmount)
	}
	if *txn.RefundedAmount > *txn.AmountPaid {
		return nil, fmt.Errorf("authoritative refunded_amount %d exceeds amount_paid %d",
			*txn.RefundedAmount, *txn.AmountPaid)
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
		ProviderTransactionID:  txn.ID,
		ProviderOrderID:        txn.Order,
		AmountMinor:            obj.RefundAmount,
		Currency:               txn.Currency,
		TransactionAmountMinor: txn.Amount,
		AmountPaidMinor:        *txn.AmountPaid,
		RefundedAmountMinor:    txn.RefundedAmount,
		ObjectStatus:           &status,
		TransactionStatus:      &txn.Status,
		Reason:                 reason,
		ProviderCreatedAt:      pc,
	}, nil
}

// buildDisputeFact validates a dispute.created webhook object reconciled
// with the authoritative transaction. Dispute semantics (single rule):
// Creem records the clawback on the transaction itself — the dispute
// event is the TRIGGER, the authoritative transaction's cumulative
// refunded_amount (with status "chargeback") is the REVERSAL AUTHORITY.
// If the authoritative transaction shows no refund movement yet, the
// dispute is stored as a durable fact with zero reversal contribution —
// a later refund event (or the chargeback settling into refunded_amount)
// advances the reversal through the same cumulative path.
func buildDisputeFact(ev *CreemWebhookEvent, obj *CreemDisputeObject, txn *CreemTransactionEntity, p *model.Payment) (*PaymentAdjustmentFact, error) {
	if err := reconcileAuthoritativeTransaction(txn, p); err != nil {
		return nil, err
	}
	if obj.ID == "" {
		return nil, fmt.Errorf("dispute.id empty")
	}
	if obj.Amount <= 0 {
		return nil, fmt.Errorf("dispute.amount %d not positive", obj.Amount)
	}
	if txn.Status == "" {
		return nil, fmt.Errorf("authoritative transaction.status empty")
	}
	if txn.RefundedAmount == nil {
		// No refund movement on the authoritative transaction: durable
		// provider fact, zero reversal contribution. A later event whose
		// authoritative basis shows refund movement advances the
		// cumulative reversal through the same single path.
		zero := int64(0)
		txn.RefundedAmount = &zero
	}
	if *txn.RefundedAmount < 0 || *txn.RefundedAmount > *txn.AmountPaid {
		return nil, fmt.Errorf("authoritative refunded_amount %d outside [0, %d]",
			*txn.RefundedAmount, *txn.AmountPaid)
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
		ProviderTransactionID:  txn.ID,
		ProviderOrderID:        txn.Order,
		AmountMinor:            obj.Amount,
		Currency:               txn.Currency,
		TransactionAmountMinor: txn.Amount,
		AmountPaidMinor:        *txn.AmountPaid,
		RefundedAmountMinor:    txn.RefundedAmount,
		ObjectStatus:           nilStatus,
		TransactionStatus:      &txn.Status,
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

// reversalTarget computes the cumulative whole-credit reversal target
// for original credits C, cumulative refunded minor R, and paid minor P:
//
//	target_reversed = floor(C * R / P)        (partial refund)
//	R == P          -> target_reversed = C    (full refund: exact)
//
// Pure 128-bit integer arithmetic — never float64, never a lossy
// uint64→int64 intermediate cast. C*R is computed as a full (hi, lo)
// 128-bit product via bits.Mul64 and divided by P as a 128-bit
// dividend via bits.Div64, so correctness does not depend on the
// product fitting in 64 bits. Cumulative FLOOR prevents fractional
// credits AND prevents over-clawback before a full refund (monotone
// non-decreasing in R, never exceeding C).
func reversalTarget(credits int64, refundedMinor, paidMinor int64) (int64, error) {
	if paidMinor <= 0 {
		return 0, fmt.Errorf("reversal: amount_paid %d not positive", paidMinor)
	}
	if refundedMinor < 0 || refundedMinor > paidMinor {
		return 0, fmt.Errorf("reversal: refunded %d outside [0, %d]", refundedMinor, paidMinor)
	}
	if credits <= 0 {
		return 0, fmt.Errorf("reversal: original credits %d not positive", credits)
	}
	if refundedMinor == paidMinor {
		return credits, nil // full refund: exactly the original grant
	}
	// Full 128-bit product C*R (both non-negative here), divided as a
	// 128-bit dividend by the 64-bit denominator.
	hi, lo := bits.Mul64(uint64(credits), uint64(refundedMinor))
	quo, _ := bits.Div64(hi, lo, uint64(paidMinor)) // full 128÷64; remainder discarded = floor
	// quotient must fit int64: its unsigned 64-bit form must not exceed
	// MaxInt64. (C ≤ MaxInt64 and R < P bound the mathematical quotient,
	// but verify, never assume.)
	if quo > uint64(math.MaxInt64) {
		return 0, fmt.Errorf("reversal: proportional quotient overflows int64 (credits=%d refunded=%d paid=%d)", credits, refundedMinor, paidMinor)
	}
	return int64(quo), nil
}

// adjustmentBasis is the canonical provider-transaction basis persisted
// with a payment's adjustment facts.
type adjustmentBasis struct {
	providerTransactionID string
	amountPaid            int64
}

// resolveAdjustmentFacts validates the persisted facts of one payment in
// a single deterministic pass:
//
//   - 0 rows                       → incoming establishes the basis
//   - rows, all one basis          → incoming must match that canonical basis
//   - rows with >1 distinct basis  → reconciliation failure, fail closed
//     (historical anomaly: never auto-merged, never "latest/max/first")
//
// On success it returns the canonical basis and MAX(refunded_amount_minor)
// computed over that SAME verified set — the denominator and the refund
// authority can never come from different collections of rows.
func resolveAdjustmentFacts(ctx context.Context, tx pgx.Tx, paymentID int64, incoming adjustmentBasis) (maxRefunded int64, basis adjustmentBasis, found bool, err error) {
	rows, err := tx.Query(ctx, `
		SELECT provider_transaction_id, amount_paid_minor, COALESCE(refunded_amount_minor, 0)
		FROM tb_payment_adjustments WHERE payment_id = $1`, paymentID)
	if err != nil {
		return 0, adjustmentBasis{}, false, err
	}
	defer rows.Close()

	maxRefunded = 0
	basis = adjustmentBasis{}
	found = false
	for rows.Next() {
		var txnID string
		var paid, refunded int64
		if err := rows.Scan(&txnID, &paid, &refunded); err != nil {
			return 0, adjustmentBasis{}, false, err
		}
		if !found {
			basis = adjustmentBasis{providerTransactionID: txnID, amountPaid: paid}
			found = true
		} else if basis.providerTransactionID != txnID || basis.amountPaid != paid {
			// Historical multi-basis anomaly: fail closed, no guesses.
			return 0, adjustmentBasis{}, false, fmt.Errorf(
				"reconciliation: persisted adjustment facts for payment %d span multiple provider transaction bases (%q/%d vs %q/%d)",
				paymentID, basis.providerTransactionID, basis.amountPaid, txnID, paid)
		}
		if refunded > maxRefunded {
			maxRefunded = refunded
		}
	}
	if err := rows.Err(); err != nil {
		return 0, adjustmentBasis{}, false, err
	}
	if !found {
		return 0, adjustmentBasis{}, false, nil
	}
	if basis.providerTransactionID != incoming.providerTransactionID {
		return 0, adjustmentBasis{}, false, fmt.Errorf(
			"reconciliation: incoming adjustment basis (%q) does not match canonical basis (%q) of persisted facts",
			incoming.providerTransactionID, basis.providerTransactionID)
	}
	if basis.amountPaid != incoming.amountPaid {
		return 0, adjustmentBasis{}, false, fmt.Errorf(
			"reconciliation: incoming adjustment amount_paid %d does not match canonical %d",
			incoming.amountPaid, basis.amountPaid)
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
//	target = floor(credits * MAX_refunded / amount_paid)   [R==P → C]
//	already_reversed = -SUM(reverse_payment ledger for this payment)
//	delta = target - already_reversed (whole credits)
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
	defer func() { _ = pg.Rollback(tx) }()

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

	inserted, err = repository.InsertPaymentAdjustment(ctx, tx, fact)
	if err != nil {
		return false, err
	}

	// Canonical basis validation + cumulative provider authority over the
	// SAME verified fact set (duplicates and A1-era facts included;
	// historical multi-basis fails closed with everything rolled back).
	maxRefunded, basis, found, err := resolveAdjustmentFacts(ctx, tx, fact.PaymentID, adjustmentBasis{
		providerTransactionID: fact.ProviderTransactionID,
		amountPaid:            fact.AmountPaidMinor,
	})
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
	// Integer cumulative FLOOR otherwise (see reversalTarget).
	target, err := reversalTarget(p.Credits, maxRefunded, basis.amountPaid)
	if err != nil {
		return false, err
	}

	// Already reversed, from the Credits ledger (never a direct
	// tb_transactions query from this domain).
	sum, err := credits.SumAmountByTypeRef(ctx, tx, p.BotID, "reverse_payment", "payment", p.Code)
	if err != nil {
		return false, fmt.Errorf("ledger read: %w", err)
	}
	if sum > 0 {
		return false, fmt.Errorf("reconciliation: reverse_payment ledger sum %d is positive", sum)
	}
	alreadyReversed := -sum
	if alreadyReversed < 0 || alreadyReversed > p.Credits {
		return false, fmt.Errorf("reconciliation: already_reversed %d outside [0, %d]", alreadyReversed, p.Credits)
	}

	if delta := target - alreadyReversed; delta > 0 {
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

// HandleCreemAdjustmentEvent is the webhook entry: parse the guaranteed
// webhook fields, resolve the payment, fetch the AUTHORITATIVE
// transaction via the official API, and only then reconcile, persist the
// durable fact, and advance the cumulative Credits reversal.
//
// Ambiguity discipline: a definitive 4xx from the provider lookup fails
// the reconciliation (Creem retries; nothing is mutated); a network/5xx
// ambiguity ALSO mutates nothing — Credits are never adjusted on
// ambiguous provider state.
func HandleCreemAdjustmentEvent(ctx context.Context, pool *pg.Pool, rt *CreemRuntime, ev *CreemWebhookEvent) error {
	switch ev.EventType {
	case "refund.created":
		var obj CreemRefundObject
		if err := json.Unmarshal(ev.Object, &obj); err != nil {
			return fmt.Errorf("refund object decode: %w", err)
		}
		if obj.TransactionBlock == nil || obj.TransactionBlock.ID == "" {
			return fmt.Errorf("refund transaction.id missing (guaranteed field absent)")
		}
		txn, p, err := authoritativePaymentTransaction(ctx, pool, rt, obj.TransactionBlock.ID)
		if err != nil {
			return err
		}
		fact, err := buildRefundFact(ev, &obj, txn, p)
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
		if obj.TransactionBlock == nil || obj.TransactionBlock.ID == "" {
			return fmt.Errorf("dispute transaction.id missing (guaranteed field absent)")
		}
		txn, p, err := authoritativePaymentTransaction(ctx, pool, rt, obj.TransactionBlock.ID)
		if err != nil {
			return err
		}
		fact, err := buildDisputeFact(ev, &obj, txn, p)
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

// authoritativePaymentTransaction fetches the provider's authoritative
// transaction and resolves the local paid payment through its order
// binding. The webhook's transaction block is used ONLY for the ID.
func authoritativePaymentTransaction(ctx context.Context, pool *pg.Pool, rt *CreemRuntime, transactionID string) (*CreemTransactionEntity, *model.Payment, error) {
	if rt == nil || rt.Client == nil {
		return nil, nil, errors.New(503, "PAYMENT_NOT_CONFIGURED", "Payment is not configured on this server")
	}
	txn, err := rt.Client.GetTransaction(ctx, transactionID)
	if err != nil {
		// Definitive (4xx) or ambiguous (network/5xx): either way NO
		// local mutation happens — fail the webhook; Creem retries.
		return nil, nil, fmt.Errorf("authoritative transaction lookup failed: %w", err)
	}
	p, err := findPaymentByProviderOrderID(ctx, pool, txn.Order)
	if err != nil {
		return nil, nil, err
	}
	return txn, p, nil
}

// findPaymentByProviderOrderID resolves the payment via the provider
// order binding (provider = creem, provider_order_id).
func findPaymentByProviderOrderID(ctx context.Context, pool *pg.Pool, orderID string) (*model.Payment, error) {
	if orderID == "" {
		return nil, errors.New(400, "RECONCILIATION_FAILED", "Transaction order missing")
	}
	p, err := repository.FindPaymentByProviderOrder(ctx, pool, "creem", orderID)
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
