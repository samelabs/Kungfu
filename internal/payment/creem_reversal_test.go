package payment

// A2: authoritative Credits reversal driven by adjustment facts. Real
// PostgreSQL. Each scenario follows the real flow: paid payment with a
// bound provider order, then adjustment events.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
)

// rev2 helpers: unique per-test txn basis id and payment lookup.
var rev2Seq int64

func rev2BasisID() string {
	rev2Seq++
	return fmt.Sprintf("txn_rev2_%d_%d", time.Now().UnixNano(), rev2Seq)
}

func rev2EventID() string {
	rev2Seq++
	return fmt.Sprintf("evt_rev2_%d_%d", time.Now().UnixNano(), rev2Seq)
}
func rev2RefundID() string {
	rev2Seq++
	return fmt.Sprintf("ref_rev2_%d_%d", time.Now().UnixNano(), rev2Seq)
}

func mustGetPayment(t *testing.T, pool *pg.Pool, code string) *model.Payment {
	t.Helper()
	p, err := GetPayment(context.Background(), pool, code)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// rev2SeedPaidPayment completes a starter checkout (1000 credits) and
// optionally spends credits so the balance can go negative.
func rev2SeedPaidPayment(t *testing.T, pool *pg.Pool, fc *fakeCreem, botID int64, spend float64) string {
	t.Helper()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	if spend > 0 {
		if _, err := credits.Record(context.Background(), pool, nil, botID,
			"spend_test", -spend, nil, nil); err != nil {
			t.Fatalf("seed spend: %v", err)
		}
	}
	return code
}

func rev2RefundEvent(t *testing.T, pool *pg.Pool, code, txnOrder string, amountPaid, refundAmount, refunded int64) *CreemWebhookEvent {
	return rev2RefundEventBasis(t, pool, code, txnOrder, amountPaid, refundAmount, refunded, rev2BasisID())
}

func rev2RefundEventBasis(t *testing.T, pool *pg.Pool, code, txnOrder string, amountPaid, refundAmount, refunded int64, basisID string) *CreemWebhookEvent {
	t.Helper()
	p := mustGetPayment(t, pool, code)
	r := refunded
	txn := &CreemTransactionFact{
		ID: basisID, Amount: p.AmountMinor, AmountPaid: amountPaid, Currency: p.Currency,
		Status: "succeeded", RefundedAmount: &r, Order: txnOrder,
	}
	obj := adjRefundObject(rev2RefundID(), txn)
	obj.RefundAmount = refundAmount
	return adjEvent(rev2EventID(), "refund.created", obj)
}

func rev2DisputeEvent(t *testing.T, pool *pg.Pool, code, txnOrder string, amountPaid int64, refunded *int64) *CreemWebhookEvent {
	return rev2DisputeEventBasis(t, pool, code, txnOrder, amountPaid, refunded, rev2BasisID())
}

func rev2DisputeEventBasis(t *testing.T, pool *pg.Pool, code, txnOrder string, amountPaid int64, refunded *int64, basisID string) *CreemWebhookEvent {
	t.Helper()
	p := mustGetPayment(t, pool, code)
	return adjEvent(rev2EventID(), "dispute.created", &CreemDisputeObject{
		ID: rev2RefundID(), Amount: amountPaid, Currency: p.Currency,
		Transaction: &CreemTransactionFact{
			ID: basisID, Amount: p.AmountMinor, AmountPaid: amountPaid, Currency: p.Currency,
			Status: "under_review", RefundedAmount: refunded, Order: txnOrder,
		},
	})
}

// Full refund after the user spent most credits → negative balance.
func TestReversalFullRefundNegativeBalance(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 900) // balance 100

	refunded := int64(1080)
	ev := rev2RefundEvent(t, pool, code, *mustGetPayment(t, pool, code).ProviderOrderID, 1080, 1080, refunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, ev); err != nil {
		t.Fatalf("full refund: %v", err)
	}

	if b := crBalance(t, pool, botID); b != -900 {
		t.Fatalf("balance = %v, want -900", b)
	}
	var sum float64
	if err := pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_type='payment' AND ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != -1000 {
		t.Fatalf("reverse sum = %v, want -1000", sum)
	}
	if p := mustGetPayment(t, pool, code); p.Status != "paid" {
		t.Fatalf("status = %s", p.Status)
	}
}

// Tax difference: target uses amount_paid as denominator — 605/1210 of
// 1000 credits = exactly 500, never 605 or 605/1000.
func TestReversalPartialTaxRefund(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)

	refunded := int64(605)
	ev := rev2RefundEvent(t, pool, code, *mustGetPayment(t, pool, code).ProviderOrderID, 1210, 605, refunded)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, ev); err != nil {
		t.Fatalf("partial tax refund: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("balance = %v, want 500 (1000-500)", b)
	}
}

// Multiple partial refunds accumulate: R=242→200, R=605→+300, total 500.
func TestReversalMultiplePartial(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	r242 := int64(242)
	basis := rev2BasisID()
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1210, 242, r242, basis)); err != nil {
		t.Fatal(err)
	}
	r605 := int64(605)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1210, 605, r605, basis)); err != nil {
		t.Fatal(err)
	}
	var sum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -500 {
		t.Fatalf("cumulative = %v, want -500", sum)
	}
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("balance = %v", b)
	}
}

// Full after partial lands exactly -payment.credits total.
func TestReversalFullAfterPartial(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	basis := rev2BasisID()
	r605 := int64(605)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1210, 605, r605, basis)); err != nil {
		t.Fatal(err)
	}
	full := int64(1210)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1210, 1210, full, basis)); err != nil {
		t.Fatal(err)
	}
	var sum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -1000 {
		t.Fatalf("total = %v, want exactly -1000", sum)
	}
}

// Duplicate event → 1 fact, 1 economic effect.
func TestReversalDuplicateOneEffect(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	r605 := int64(605)
	ev := rev2RefundEvent(t, pool, code, order, 1210, 605, r605)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, ev); err != nil {
		t.Fatal(err)
	}
	// exact redelivery of the SAME event id
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, ev); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	var facts int
	var sum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&facts)
	_ = pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if facts != 1 || sum != -500 {
		t.Fatalf("facts=%d sum=%v", facts, sum)
	}
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("balance = %v", b)
	}
}

// Out of order: 605 first, then 242 → second is zero mutation.
func TestReversalOutOfOrderNoGiveBack(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	basis := rev2BasisID()
	r605 := int64(605)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1210, 605, r605, basis)); err != nil {
		t.Fatal(err)
	}
	r242 := int64(242)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1210, 242, r242, basis)); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("balance = %v, want 500 (no give-back)", b)
	}
}

// Dispute full chargeback, then a full refund on the same transaction →
// exactly one full reversal.
func TestReversalDisputeThenRefundSingleReversal(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	full := int64(1080)
	basis := rev2BasisID()
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2DisputeEventBasis(t, pool, code, order, 1080, &full, basis)); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("after dispute balance = %v, want 0", b)
	}
	// subsequent full refund event on the SAME transaction
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEventBasis(t, pool, code, order, 1080, 1080, full, basis)); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("second full event double-reversed: %v", b)
	}
	var sum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -1000 {
		t.Fatalf("sum = %v, want exactly -1000", sum)
	}
}

// Dispute with nil refunded_amount → durable fact, zero reversal.
func TestReversalDisputeNilRefundedZeroMutation(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2DisputeEvent(t, pool, code, order, 1080, nil)); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("balance = %v, want unchanged 1000", b)
	}
	var facts int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&facts)
	if facts != 1 {
		t.Fatalf("facts = %d, want 1 durable dispute", facts)
	}
}

// Inconsistent basis: different txn id or amount_paid → reject, fact
// rolled back, zero ledger/balance mutation.
func TestReversalInconsistentBasisRejected(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	order := *mustGetPayment(t, pool, code).ProviderOrderID

	r605 := int64(605)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEvent(t, pool, code, order, 1210, 605, r605)); err != nil {
		t.Fatal(err)
	}

	// different amount_paid basis
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		rev2RefundEvent(t, pool, code, order, 1300, 650, 650)); err == nil {
		t.Fatal("different amount_paid must be rejected")
	}
	// different provider transaction id
	p := mustGetPayment(t, pool, code)
	txn := &CreemTransactionFact{
		ID: adjUnique("txn_other"), Amount: p.AmountMinor, AmountPaid: 1210, Currency: p.Currency,
		Status: "succeeded", RefundedAmount: &r605, Order: order,
	}
	obj := adjRefundObject(adjUnique("ref"), txn)
	obj.RefundAmount = 605
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		adjEvent(adjUnique("evt"), "refund.created", obj)); err == nil {
		t.Fatal("different txn id must be rejected")
	}

	var facts int
	var sum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&facts)
	_ = pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if facts != 1 || sum != -500 {
		t.Fatalf("facts=%d sum=%v — conflict must leave one fact one effect", facts, sum)
	}
}

// A1 catch-up: fact persisted without reversal, then redelivery of the
// same object under a new event id → reversal executes once.
func TestReversalA1CatchUp(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	p := mustGetPayment(t, pool, code)
	order := *p.ProviderOrderID

	// Simulate an A1-era fact: insert directly, no reversal.
	r605 := int64(605)
	txnID := adjUnique("txn_a1")
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tb_payment_adjustments (payment_id, provider, provider_event_id, provider_object_id, kind,
			provider_transaction_id, provider_order_id, amount_minor, currency,
			transaction_amount_minor, amount_paid_minor, refunded_amount_minor,
			object_status, transaction_status, reason, provider_created_at)
		VALUES ($1,'creem',$2,$3,'refund',$4,$5,605,'USD',1000,1210,$6,'succeeded','succeeded',NULL,$7)`,
		p.ID, adjUnique("evt_a1"), adjUnique("ref_a1"), txnID, order, r605, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("pre-catchup balance = %v", b)
	}

	// redelivery: SAME object id, NEW event id, SAME txn basis
	txn := &CreemTransactionFact{
		ID: txnID, Amount: 1000, AmountPaid: 1210, Currency: "USD",
		Status: "succeeded", RefundedAmount: &r605, Order: order,
	}
	// look up the seeded object id
	var objID string
	_ = pool.QueryRow(context.Background(),
		`SELECT provider_object_id FROM tb_payment_adjustments WHERE payment_id=$1`, p.ID).Scan(&objID)
	obj := adjRefundObject(objID, txn)
	obj.RefundAmount = 605
	if err := HandleCreemAdjustmentEvent(context.Background(), pool,
		adjEvent(adjUnique("evt_catchup"), "refund.created", obj)); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("post-catchup balance = %v, want 500", b)
	}
	var facts int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&facts)
	if facts != 1 {
		t.Fatalf("facts = %d, want 1", facts)
	}
}

// Concurrency: two adjustments with R1 < R2 racing → final cumulative
// reversal == target(max(R1,R2)), never over-reversed.
func TestReversalConcurrentNoOverReverse(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
	p := mustGetPayment(t, pool, code)
	order := *p.ProviderOrderID

	r242, r605 := int64(242), int64(605)
	txnID := rev2BasisID()
	mk := func(refundAmt int64, objID string) *CreemWebhookEvent {
		r := refundAmt
		txn := &CreemTransactionFact{
			ID: txnID, Amount: p.AmountMinor, AmountPaid: 1210, Currency: p.Currency,
			Status: "succeeded", RefundedAmount: &r, Order: order,
		}
		obj := adjRefundObject(objID, txn)
		obj.RefundAmount = refundAmt
		return adjEvent(adjUnique("evt_cc"), "refund.created", obj)
	}
	ev1 := mk(r242, adjUnique("ref_cc1"))
	ev2 := mk(r605, adjUnique("ref_cc2"))

	errCh := make(chan error, 2)
	for _, ev := range []*CreemWebhookEvent{ev1, ev2} {
		go func(e *CreemWebhookEvent) {
			errCh <- HandleCreemAdjustmentEvent(context.Background(), pool, e)
		}(ev)
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent adjustment: %v", err)
		}
	}

	var sum float64
	_ = pool.QueryRow(context.Background(),
		`SELECT SUM(amount) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -500 {
		t.Fatalf("concurrent cumulative = %v, want -500 (max target)", sum)
	}
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("balance = %v, want 500", b)
	}
}

// seedHistoricalFacts inserts pre-A2 adjustment facts directly (no
// reversal), with precise scoped cleanup.
func seedHistoricalFacts(t *testing.T, pool *pg.Pool, p *model.Payment, basisID string, amountPaid int64, refunded int64, tag string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tb_payment_adjustments (payment_id, provider, provider_event_id, provider_object_id, kind,
			provider_transaction_id, provider_order_id, amount_minor, currency,
			transaction_amount_minor, amount_paid_minor, refunded_amount_minor,
			object_status, transaction_status, reason, provider_created_at)
		VALUES ($1,'creem',$2,$3,'refund',$4,$5,605,'USD',1000,$6,$7,'succeeded','succeeded',NULL,$8)`,
		p.ID, adjUnique("evt_hist"), adjUnique("ref_hist_"+tag), basisID,
		*p.ProviderOrderID, amountPaid, refunded, time.Now().Unix()); err != nil {
		t.Fatalf("seed historical fact: %v", err)
	}
}

// Case A: same amount_paid, DIFFERENT txn ids → historical multi-basis.
// Case B: same txn id, different amount_paid (1210/1300) → multi-basis.
// Both: a subsequent valid adjustment event must fail closed with the
// history untouched — no new fact, no reverse_payment, no balance move,
// payment still paid.
func TestReversalHistoricalMultiBasisFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seedFn func(t *testing.T, pool *pg.Pool, p *model.Payment)
	}{
		{"different txn ids", func(t *testing.T, pool *pg.Pool, p *model.Payment) {
			seedHistoricalFacts(t, pool, p, adjUnique("txn_A"), 1210, 605, "a")
			seedHistoricalFacts(t, pool, p, adjUnique("txn_B"), 1210, 605, "b")
		}},
		{"different amount_paid", func(t *testing.T, pool *pg.Pool, p *model.Payment) {
			basis := adjUnique("txn_same")
			seedHistoricalFacts(t, pool, p, basis, 1210, 605, "c")
			seedHistoricalFacts(t, pool, p, basis, 1300, 605, "d")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := crTestPool(t)
			botID := crSeedBot(t, pool)
			fc := newFakeCreem(t, pkgProducts()...)
			code := rev2SeedPaidPayment(t, pool, fc, botID, 0)
			p := mustGetPayment(t, pool, code)
			order := *p.ProviderOrderID

			tc.seedFn(t, pool, p)

			var factsBefore int
			_ = pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM tb_payment_adjustments WHERE payment_id=$1`, p.ID).Scan(&factsBefore)
			if factsBefore != 2 {
				t.Fatalf("seed produced %d facts", factsBefore)
			}

			// valid event matching ONE of the historical bases
			if err := HandleCreemAdjustmentEvent(context.Background(), pool,
				rev2RefundEvent(t, pool, code, order, 1210, 605, 605)); err == nil {
				t.Fatal("event on historically inconsistent basis must fail closed")
			}

			var factsAfter int
			var revCount int
			var bal float64
			_ = pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM tb_payment_adjustments WHERE payment_id=$1`, p.ID).Scan(&factsAfter)
			_ = pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&revCount)
			_ = pool.QueryRow(context.Background(),
				`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&bal)
			if factsAfter != 2 || revCount != 0 || bal != 1000 {
				t.Fatalf("facts=%d rev=%d bal=%v — must stay untouched", factsAfter, revCount, bal)
			}
			if p2 := mustGetPayment(t, pool, code); p2.Status != "paid" {
				t.Fatalf("status = %s", p2.Status)
			}
		})
	}
}
