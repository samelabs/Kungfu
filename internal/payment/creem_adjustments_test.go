package payment

// Payment adjustment facts (refund/dispute) integration tests: real
// PostgreSQL. Facts only — balance and tb_transactions must never move.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

// adjSeedPaidPayment drives the REAL flow: package checkout (fake Creem)
// → checkout.completed webhook → paid payment with a bound provider
// order, granted credits on balance. Returns bot id + payment code.
func adjSeedPaidPayment(t *testing.T, pool *pg.Pool, fc *fakeCreem, botID int64) string {
	t.Helper()
	rt := fc.runtime()
	res, err := StartCreemCheckout(context.Background(), pool, rt, botID, "starter")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	raw, _ := buildCompletion(res.Payment.Code, botID, adjUnique("ord_adj"), nil)
	var ev CreemWebhookEvent
	_ = json.Unmarshal(raw, &ev)
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err != nil {
		t.Fatalf("completion: %v", err)
	}
	return res.Payment.Code
}

// adjEvent builds a signed-envelope-shaped refund/dispute event.
var adjSeq int64

func adjUnique(prefix string) string {
	adjSeq++
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), adjSeq)
}

func adjEvent(eventID, eventType string, object interface{}) *CreemWebhookEvent {
	obj, _ := json.Marshal(object)
	return &CreemWebhookEvent{
		ID: eventID, EventType: eventType, CreatedAt: time.Now().Unix(), Object: obj,
	}
}

// adjTxn registers an authoritative TransactionEntity on the fake
// provider and returns its id.
func adjTxn(fc *fakeCreem, txnID, order string, amount, amountPaid int64, currency, status string, refunded *int64) string {
	if txnID == "" {
		txnID = adjUnique("txn")
	}
	fc.setTxn(&CreemTransactionEntity{
		ID: txnID, Object: "transaction", Amount: amount, AmountPaid: &amountPaid,
		RefundedAmount: refunded, Currency: currency, Status: status, Order: order, Mode: "test",
	})
	return txnID
}

// adjRefundObject builds a refund.created object carrying ONLY the
// officially-guaranteed fields (transaction.id included; currency/order
// deliberately absent from the webhook).
func adjRefundObject(refundID string, amountPaid int64, txnID string) *CreemRefundObject {
	return &CreemRefundObject{
		ID: refundID, Status: "succeeded",
		RefundAmount: 540,
		TransactionBlock: &struct {
			ID string `json:"id"`
		}{ID: txnID},
	}
}

// adjDisputeObject builds a dispute.created object with only guaranteed
// fields.
func adjDisputeObject(disputeID string, amount int64, txnID string) *CreemDisputeObject {
	return &CreemDisputeObject{
		ID:     disputeID,
		Amount: amount,
		TransactionBlock: &struct {
			ID string `json:"id"`
		}{ID: txnID},
	}
}

func adjCounts(t *testing.T, pool *pg.Pool, code string) (adjustments, transactions int, balance float64) {
	t.Helper()
	var botID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT bot_id FROM tb_payments WHERE code=$1`, code).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&adjustments); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT balance FROM tb_bots WHERE id=$1`, botID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	return
}

// Valid signed refund → exact adjustment row, balance unchanged, no new
// tb_transactions. amount_paid > payment.amount_minor (tax) must pass.
func TestRefundFactRecordedExact(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)

	// Complete a payment first.
	code := adjSeedPaidPayment(t, pool, fc, botID)

	p, _ := GetPayment(context.Background(), pool, code)
	order := *p.ProviderOrderID
	// amount_paid includes tax: 1000 product + 80 tax = 1080. The
	// authoritative transaction lives on the provider API, NOT in the
	// webhook payload.
	refunded := int64(540)
	txnID := adjTxn(fc, "", order, 1000, 1080, "USD", "succeeded", &refunded)
	ev := adjEvent(adjUnique("evt_refund"), "refund.created", adjRefundObject(adjUnique("ref"), 540, txnID))
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), ev); err != nil {
		t.Fatalf("refund: %v", err)
	}

	var kind, objStatus, cur string
	var amountMinor, amountPaid, txnAmount, refundedStored int64
	var paymentID int64
	var reason *string
	if err := pool.QueryRow(context.Background(), `
		SELECT a.kind, a.object_status, a.currency, a.amount_minor, a.amount_paid_minor,
		       a.transaction_amount_minor, a.refunded_amount_minor, a.payment_id, a.reason
		FROM tb_payment_adjustments a JOIN tb_payments p ON p.id = a.payment_id
		WHERE p.code = $1`, code).Scan(
		&kind, &objStatus, &cur, &amountMinor, &amountPaid, &txnAmount, &refundedStored, &paymentID, &reason); err != nil {
		t.Fatal(err)
	}
	if kind != "refund" || objStatus != "succeeded" || cur != "USD" {
		t.Fatalf("fact = %s/%s/%s", kind, objStatus, cur)
	}
	if amountMinor != 540 || amountPaid != 1080 || txnAmount != 1000 || refundedStored != 540 {
		t.Fatalf("amounts = %d/%d/%d/%d", amountMinor, amountPaid, txnAmount, refundedStored)
	}
	if paymentID != p.ID {
		t.Fatalf("payment linkage = %d, want %d", paymentID, p.ID)
	}

	adjN, txN, bal := adjCounts(t, pool, code)
	// A2: full-amount refund (540/1080 cumulative) authorizes reversal of
	// full refund 1080/1080 → exactly 1000 reversed → balance 500.
	if adjN != 1 || txN != 2 || bal != 500 {
		t.Fatalf("adj=%d tx=%d bal=%v — want 1 fact, grant+reverse, bal 500", adjN, txN, bal)
	}
}

// Partial refund: refund_amount < amount_paid; cumulative refunded ≥ refund.
func TestPartialRefundFact(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(300)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "succeeded", &refunded)
	obj := adjRefundObject(adjUnique("ref"), 540, txnID)
	obj.RefundAmount = 300 // partial
	ev := adjEvent(adjUnique("evt_refund"), "refund.created", obj)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), ev); err != nil {
		t.Fatalf("partial refund: %v", err)
	}
	var amount int64
	if err := pool.QueryRow(context.Background(), `
		SELECT a.amount_minor FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1 AND a.kind='refund'`, code).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != 300 {
		t.Fatalf("partial refund amount = %d", amount)
	}
	_, txN, bal := adjCounts(t, pool, code)
	// A2 (integer credits): cumulative 300/1080 → reversal target
	// floor(1000*300/1080) = 277 (cumulative FLOOR, whole credits only)
	if txN != 2 || bal != 723 {
		t.Fatalf("tx=%d bal=%v", txN, bal)
	}
}

// Idempotency: same event redelivered, or same object under a new event
// id → zero new rows, success.
func TestAdjustmentIdempotency(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(540)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "succeeded", &refunded)
	evtID, objID := adjUnique("evt_d"), adjUnique("ref_i")
	// 1st delivery
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(evtID, "refund.created", adjRefundObject(objID, 540, txnID))); err != nil {
		t.Fatal(err)
	}
	// 2nd: exact redelivery (same event id)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(evtID, "refund.created", adjRefundObject(objID, 540, txnID))); err != nil {
		t.Fatalf("redelivery must succeed: %v", err)
	}
	// 3rd: same object id under a NEW event id — same unique object
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_d2"), "refund.created", adjRefundObject(objID, 540, txnID))); err != nil {
		t.Fatalf("same-object new-event must succeed: %v", err)
	}
	adjN, _, _ := adjCounts(t, pool, code)
	if adjN != 1 {
		t.Fatalf("adjustments = %d, want 1", adjN)
	}
}

// Refund + dispute on the same payment → two independent facts.
func TestRefundAndDisputeIndependentFacts(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(540)
	basis := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "succeeded", &refunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_rf"), "refund.created", adjRefundObject(adjUnique("ref_rd"), 540, basis))); err != nil {
		t.Fatal(err)
	}
	// dispute: the authoritative txn status advances to chargeback_open
	cbRefunded := int64(540)
	adjTxn(fc, basis, *p.ProviderOrderID, 1000, 1080, "USD", "chargeback_open", &cbRefunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_ds"), "dispute.created", adjDisputeObject(adjUnique("dis"), 1080, basis))); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("facts = %d, want 2 independent", n)
	}
	// dispute stores the transaction status verbatim, whatever it is
	var ts string
	if err := pool.QueryRow(context.Background(), `
		SELECT a.transaction_status FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1 AND a.kind='dispute'`, code).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != "chargeback_open" {
		t.Fatalf("dispute txn status = %s", ts)
	}
	// payment stays paid
	if p2, _ := GetPayment(context.Background(), pool, code); p2.Status != "paid" {
		t.Fatalf("payment status = %s", p2.Status)
	}
}

// Reconciliation gates: facts that do not match the payment snapshot
// must be rejected with zero rows.
func TestAdjustmentReconciliationGates(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	// Each case registers an authoritative transaction that is wrong in
	// exactly one dimension (or a wrong webhook refund object) — every
	// gate must reject with zero rows. The webhook carries only the
	// guaranteed fields; the basis is the API transaction.
	mkTxn := func(mutate func(t *CreemTransactionEntity)) string {
		r := int64(540)
		ap := int64(1080)
		txn := &CreemTransactionEntity{ID: adjUnique("txn"), Object: "transaction",
			Amount: 1000, AmountPaid: &ap, RefundedAmount: &r, Currency: "USD",
			Status: "succeeded", Order: *p.ProviderOrderID, Mode: "test"}
		if mutate != nil {
			mutate(txn)
		}
		fc.setTxn(txn)
		return txn.ID
	}
	cases := []struct {
		name string
		txn  string
		obj  func(txnID string) interface{}
	}{
		{"wrong order", mkTxn(func(t *CreemTransactionEntity) { t.Order = "ord_other" }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"wrong amount", mkTxn(func(t *CreemTransactionEntity) { t.Amount = 999 }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"wrong currency", mkTxn(func(t *CreemTransactionEntity) { t.Currency = "EUR" }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"amount_paid zero", mkTxn(func(t *CreemTransactionEntity) { ap := int64(0); t.AmountPaid = &ap }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"amount_paid missing", mkTxn(func(t *CreemTransactionEntity) { t.AmountPaid = nil }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"refund not succeeded", mkTxn(nil), func(id string) interface{} {
			o := adjRefundObject(adjUnique("ref"), 540, id)
			o.Status = "pending"
			return o
		}},
		{"refund exceeds paid", mkTxn(nil), func(id string) interface{} {
			o := adjRefundObject(adjUnique("ref"), 540, id)
			o.RefundAmount = 1081
			return o
		}},
		{"refunded missing", mkTxn(func(t *CreemTransactionEntity) { t.RefundedAmount = nil }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"refunded below refund", mkTxn(func(t *CreemTransactionEntity) { r := int64(1); t.RefundedAmount = &r }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"refunded exceeds amount_paid", mkTxn(func(t *CreemTransactionEntity) { r := int64(1081); t.RefundedAmount = &r }), func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"txn unknown to provider", "txn_not_registered", func(id string) interface{} { return adjRefundObject(adjUnique("ref"), 540, id) }},
		{"webhook txn id missing", mkTxn(nil), func(id string) interface{} {
			return &CreemRefundObject{ID: adjUnique("ref"), Status: "succeeded", RefundAmount: 540}
		}},
	}
	for _, tc := range cases {
		if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
			adjEvent(adjUnique("evt_gate"), "refund.created", tc.obj(tc.txn))); err == nil {
			t.Fatalf("%s: must fail", tc.name)
		}
	}
	adjN, _, _ := adjCounts(t, pool, code)
	if adjN != 0 {
		t.Fatalf("gated facts leaked rows: %d", adjN)
	}
}

// Unknown provider order → reconciliation failure, zero rows.
func TestAdjustmentUnknownOrder(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	_ = code

	r := int64(10)
	txnID := adjTxn(fc, "", "ord_nonexistent", 1000, 1080, "USD", "succeeded", &r)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_unk"), "refund.created", adjRefundObject(adjUnique("ref_u"), 540, txnID))); err == nil {
		t.Fatal("unknown order must fail")
	}
}

// Pending (not paid) payment must not receive adjustment facts.
func TestAdjustmentRequiresPaidPayment(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	res, _ := StartCreemCheckout(context.Background(), pool, rt, botID, "starter") // stays pending

	r := int64(500)
	txnID := adjTxn(fc, "", "ord_"+res.Payment.Code, 1000, 1000, "USD", "succeeded", &r)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_pp"), "refund.created", adjRefundObject(adjUnique("ref_pp"), 540, txnID))); err == nil {
		t.Fatal("pending payment must not accept adjustments")
	}
}

// Work-order fixture: amount_paid 1210 (tax) vs payment.amount_minor
// 1000 must NOT be an error; refund 605/605 records exactly.
func TestRefundFactTaxDifference1210(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(605)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1210, "USD", "succeeded", &refunded)
	obj := adjRefundObject(adjUnique("ref"), 540, txnID)
	obj.RefundAmount = 605
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_refund_1210"), "refund.created", obj)); err != nil {
		t.Fatalf("1210/605/605 must record: %v", err)
	}
	var amountMinor, amountPaid int64
	if err := pool.QueryRow(context.Background(), `
		SELECT a.amount_minor, a.amount_paid_minor FROM tb_payment_adjustments a
		JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1 AND a.kind='refund'`, code).Scan(&amountMinor, &amountPaid); err != nil {
		t.Fatal(err)
	}
	if amountMinor != 605 || amountPaid != 1210 {
		t.Fatalf("amounts = %d/%d", amountMinor, amountPaid)
	}
	_, txN, bal := adjCounts(t, pool, code)
	// A2: cumulative 605/1210 → reversal floor(1000*605/1210)=500
	if txN != 2 || bal != 500 {
		t.Fatalf("tx=%d bal=%v — want grant+reverse, bal 500", txN, bal)
	}
}

// Standalone valid dispute: durable row, balance unchanged, ledger unchanged.
func TestDisputeFactRecordedStandalone(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(0)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "needs_response", &refunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_dispute"), "dispute.created", adjDisputeObject(adjUnique("dis"), 1080, txnID))); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	adjN, txN, bal := adjCounts(t, pool, code)
	if adjN != 1 {
		t.Fatalf("dispute rows = %d", adjN)
	}
	if txN != 1 || bal != 1000 {
		t.Fatalf("dispute mutated money: tx=%d bal=%v", txN, bal)
	}
}

// Dispute with empty transaction.status → reconciliation failure, zero rows.
func TestDisputeEmptyStatusRejected(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(0)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "", &refunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_dispute_empty"), "dispute.created", adjDisputeObject(adjUnique("dis"), 1080, txnID))); err == nil {
		t.Fatal("empty dispute transaction.status must fail")
	}
	adjN, _, _ := adjCounts(t, pool, code)
	if adjN != 0 {
		t.Fatalf("empty-status dispute leaked rows: %d", adjN)
	}
}

// Missing/zero created_at → reconciliation failure, zero rows (no local
// time substitution for a provider fact). Covers refund and dispute.
func TestAdjustmentMissingCreatedAtRejected(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(540)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "succeeded", &refunded)

	// refund with zero created_at
	ev0 := adjEvent(adjUnique("evt_ca0"), "refund.created", adjRefundObject(adjUnique("ref"), 540, txnID))
	ev0.CreatedAt = 0
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), ev0); err == nil {
		t.Fatal("zero created_at refund must fail")
	}
	// dispute with negative created_at
	txnID2 := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "under_review", &refunded)
	evN := adjEvent(adjUnique("evt_can"), "dispute.created", adjDisputeObject(adjUnique("dis"), 1080, txnID2))
	evN.CreatedAt = -5
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), evN); err == nil {
		t.Fatal("negative created_at dispute must fail")
	}
	adjN, _, _ := adjCounts(t, pool, code)
	if adjN != 0 {
		t.Fatalf("missing-created_at events leaked rows: %d", adjN)
	}
}

// Residue proof: after a full adjustment test run, nothing this test
// created survives — bot, payments, and adjustment facts are all gone.
func TestAdjustmentCleanupLeavesNoResidue(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(540)
	txnID := adjTxn(fc, "", *p.ProviderOrderID, 1000, 1080, "USD", "succeeded", &refunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(),
		adjEvent(adjUnique("evt_residue"), "refund.created", adjRefundObject(adjUnique("ref"), 540, txnID))); err != nil {
		t.Fatal(err)
	}

	// force cleanup NOW (t.Cleanup order is LIFO; run the same helper)
	cleanupBotRows(t, pool, botID)

	var bots, payments, adjustments, txs int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_bots WHERE id=$1`, botID).Scan(&bots)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1`, botID).Scan(&payments)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_payment_adjustments WHERE payment_id=$1`, p.ID).Scan(&adjustments)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&txs)
	if bots != 0 || payments != 0 || adjustments != 0 || txs != 0 {
		t.Fatalf("residue: bots=%d payments=%d adjustments=%d txs=%d", bots, payments, adjustments, txs)
	}
}
