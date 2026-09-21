package payment

// Dispute entitlement reversal regressions — fixed business rule:
//
//   - Payment success grants fixed Credits C.
//   - Ordinary refund: cumulative target floor(C·refunded/paid).
//   - VERIFIED dispute.created: cumulative target = C (the entire
//     entitlement is permanently revoked at dispute time).
//   - delta = target - already_reversed; only delta > 0 writes.
//   - No dispute won/lost lifecycle, no background polling, no
//     provider-state re-grant: a later merchant-won or funds-recovery
//     fact can never produce a positive Credits compensation because
//     the cumulative target is capped at C on every path.
//
// All disputes go through the REAL webhook path: guaranteed-fields
// event → authoritative Transaction lookup → binding validation →
// RecordPaymentAdjustment (payment lock + idempotency + cumulative
// ledger authority).

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"kungfu.md/internal/credits"
	"kungfu.md/internal/pg"
)

// dispWebhook delivers one guaranteed-fields dispute.created.
func dispWebhook(t *testing.T, pool *pg.Pool, rt *CreemRuntime, evtID, objID, txnID string) error {
	t.Helper()
	raw := `{"id":"` + evtID + `","eventType":"dispute.created","created_at":1758000000,
		"object":{"id":"` + objID + `","amount":1080,"transaction":{"id":"` + txnID + `"}}}`
	var ev CreemWebhookEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return HandleCreemAdjustmentEvent(context.Background(), pool, rt, &ev)
}

func dispReversalCount(t *testing.T, pool *pg.Pool, code string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dispSeed: paid payment (grant 1000, amount_paid 1080), authoritative
// transaction registered with the given refunded basis.
func dispSeed(t *testing.T) (pool *pg.Pool, botID int64, fc *fakeCreem, rt *CreemRuntime, code string) {
	t.Helper()
	pool = crTestPool(t)
	botID = crSeedBot(t, pool)
	fc = newFakeCreem(t, pkgProducts()...)
	rt = fc.runtime()
	code = adjSeedPaidPayment(t, pool, fc, botID)
	return
}

//  1. paid payment + dispute.created + refunded_amount = 0
//     → immediate FULL entitlement reversal (not gated on chargedBack).
func TestDisputeEntitlementFullReversalAtZeroRefunded(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_e1", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)

	if err := dispWebhook(t, pool, rt, "evt_e1", "dp_e1", "txn_e1"); err != nil {
		t.Fatalf("dispute must reconcile: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("entitlement not fully revoked: balance=%v want 0", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("reversal rows=%d want 1", c)
	}
}

// 2. partial refund (300 reversed) → dispute → only the remaining 700.
func TestDisputeEntitlementAfterPartialRefund(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	order := *p.ProviderOrderID

	// ordinary partial refund: 324/1080 → floor(1000·324/1080) = 300
	r := int64(324)
	adjTxn(fc, "txn_e2", order, 1000, 1080, "USD", "succeeded", &r)
	raw := `{"id":"evt_e2r","eventType":"refund.created","created_at":1758000000,
		"object":{"id":"ref_e2","status":"succeeded","refund_amount":324,"transaction":{"id":"txn_e2"}}}`
	var rev CreemWebhookEvent
	_ = json.Unmarshal([]byte(raw), &rev)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &rev); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 700 {
		t.Fatalf("partial refund balance=%v want 700", b)
	}

	// dispute on the same payment → target C=1000, delta=700 only.
	adjTxn(fc, "txn_e2", order, 1000, 1080, "USD", "under_review", &r)
	if err := dispWebhook(t, pool, rt, "evt_e2d", "dp_e2", "txn_e2"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("post-dispute balance=%v want 0", b)
	}
	var sum int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -1000 {
		t.Fatalf("cumulative reversal=%v want exactly -1000", sum)
	}
}

// 3. duplicate dispute event → no duplicate reversal.
func TestDisputeEntitlementDuplicateOnce(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_e3", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)

	for i := 0; i < 3; i++ {
		if err := dispWebhook(t, pool, rt, "evt_e3", "dp_e3", "txn_e3"); err != nil {
			t.Fatal(err)
		}
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("duplicate dispute balance=%v", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("duplicate dispute rows=%d want 1", c)
	}
}

// 4. concurrent duplicate dispute processing → exactly-once.
func TestDisputeEntitlementConcurrentExactlyOnce(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_e4", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = dispWebhook(t, pool, rt, "evt_e4", "dp_e4", "txn_e4")
		}()
	}
	wg.Wait()
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("concurrent balance=%v want 0", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("concurrent reversal rows=%d want 1", c)
	}
}

//  5. provider lookup failure (5xx ambiguity) → zero mutation, error out
//     so the webhook can be redelivered.
func TestDisputeEntitlementProviderFailureZeroMutation(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_e5", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	fc.setAmbiguous("txn_e5", true)

	if err := dispWebhook(t, pool, rt, "evt_e5", "dp_e5", "txn_e5"); err == nil {
		t.Fatal("ambiguous lookup must fail the webhook")
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("ambiguous mutated balance=%v", b)
	}
	var facts int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id WHERE p.code=$1`, code).Scan(&facts)
	if facts != 0 {
		t.Fatalf("ambiguous persisted %d facts", facts)
	}

	// 6. provider recovers, event redelivered → exactly-once reversal.
	fc.setAmbiguous("txn_e5", false)
	if err := dispWebhook(t, pool, rt, "evt_e5", "dp_e5", "txn_e5"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("recovered balance=%v want 0", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("recovered rows=%d want 1", c)
	}
}

//  7. credits already partially spent → dispute reversal continues
//     through the same Credits authority, balance goes negative (debt).
func TestDisputeEntitlementNegativeBalanceAllowed(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_e7", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)

	// spend 800 of the 1000 granted credits
	if _, err := credits.Record(context.Background(), pool, nil, botID,
		"spend_test", -800, nil, nil); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 200 {
		t.Fatalf("post-spend balance=%v want 200", b)
	}

	if err := dispWebhook(t, pool, rt, "evt_e7", "dp_e7", "txn_e7"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != -800 {
		t.Fatalf("dispute debt balance=%v want -800", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("debt reversal rows=%d want 1", c)
	}
}

//  8. after a dispute, a later merchant-won / funds-recovery provider
//     fact must NOT produce positive compensation.
func TestDisputeEntitlementNoPositiveCompensation(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	order := *p.ProviderOrderID
	zero := int64(0)
	adjTxn(fc, "txn_e8", order, 1000, 1080, "USD", "paid", &zero)

	if err := dispWebhook(t, pool, rt, "evt_e8", "dp_e8", "txn_e8"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("dispute balance=%v want 0", b)
	}

	// provider later "wins" / funds recovered: refunded goes back to 0
	// and status returns succeeded. No path may add credits back —
	// redelivered disputes and any later facts on the same payment all
	// resolve to the same capped cumulative target C.
	adjTxn(fc, "txn_e8", order, 1000, 1080, "USD", "succeeded", &zero)
	if err := dispWebhook(t, pool, rt, "evt_e8b", "dp_e8b", "txn_e8"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("compensation produced balance=%v want 0", b)
	}
	var sum int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -1000 {
		t.Fatalf("cumulative=%v want -1000", sum)
	}
}

// 9. ordinary refund proportional behavior unchanged (regression).
func TestDisputeEntitlementOrdinaryRefundUnchanged(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	order := *p.ProviderOrderID

	r := int64(540)
	adjTxn(fc, "txn_e9", order, 1000, 1080, "USD", "succeeded", &r)
	raw := `{"id":"evt_e9","eventType":"refund.created","created_at":1758000000,
		"object":{"id":"ref_e9","status":"succeeded","refund_amount":540,"transaction":{"id":"txn_e9"}}}`
	var rev CreemWebhookEvent
	_ = json.Unmarshal([]byte(raw), &rev)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &rev); err != nil {
		t.Fatal(err)
	}
	// floor(1000·540/1080) = 500
	if b := crBalance(t, pool, botID); b != 500 {
		t.Fatalf("ordinary refund balance=%v want 500", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("ordinary refund rows=%d want 1", c)
	}
}

// ---- provider-fact truthfulness + persisted-fact-derived target ----

// dispFact reads the persisted adjustment row for a payment.
func dispFact(t *testing.T, pool *pg.Pool, code string) (kind string, refunded *int64) {
	t.Helper()
	var k string
	var r *int64
	if err := pool.QueryRow(context.Background(), `
		SELECT a.kind, a.refunded_amount_minor
		FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id
		WHERE p.code=$1 ORDER BY a.id DESC LIMIT 1`, code).Scan(&k, &r); err != nil {
		t.Fatal(err)
	}
	return k, r
}

// A. dispute + provider refunded = 0 → full entitlement reversal AND the
// persisted fact keeps the provider truth refunded_amount_minor = 0.
func TestDisputeFactTruthZeroRefundedPersisted(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_t1", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	if err := dispWebhook(t, pool, rt, "evt_t1", "dp_t1", "txn_t1"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("entitlement not revoked: %v", b)
	}
	k, r := dispFact(t, pool, code)
	if k != "dispute" || r == nil || *r != 0 {
		t.Fatalf("persisted fact = (%q, %v), want (dispute, 0)", k, r)
	}
}

// B. dispute + provider refunded = nil → full entitlement reversal AND
// persisted refunded_amount_minor IS NULL.
func TestDisputeFactTruthNilRefundedPersisted(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	adjTxn(fc, "txn_t2", *p.ProviderOrderID, 1000, 1080, "USD", "paid", nil)
	if err := dispWebhook(t, pool, rt, "evt_t2", "dp_t2", "txn_t2"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("entitlement not revoked: %v", b)
	}
	k, r := dispFact(t, pool, code)
	if k != "dispute" || r != nil {
		t.Fatalf("persisted fact = (%q, %v), want (dispute, NULL)", k, r)
	}
}

// C. dispute → later ordinary refund → cumulative reversal stays exactly
// C; no duplicate reversal, no compensation. The target derivation uses
// the DURABLE dispute fact, not the incoming event's kind.
func TestDisputeThenOrdinaryRefundStaysAtC(t *testing.T) {
	pool, botID, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	order := *p.ProviderOrderID
	zero := int64(0)
	adjTxn(fc, "txn_t3", order, 1000, 1080, "USD", "paid", &zero)
	if err := dispWebhook(t, pool, rt, "evt_t3d", "dp_t3", "txn_t3"); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("post-dispute balance=%v", b)
	}

	// Later full refund on the same payment: durable dispute fact exists,
	// so the cumulative target must remain C — exactly one -1000 total,
	// no second reversal row, no compensation.
	full := int64(1080)
	adjTxn(fc, "txn_t3", order, 1000, 1080, "USD", "refunded", &full)
	raw := `{"id":"evt_t3r","eventType":"refund.created","created_at":1758000100,
		"object":{"id":"ref_t3","status":"succeeded","refund_amount":1080,"transaction":{"id":"txn_t3"}}}`
	var rev CreemWebhookEvent
	_ = json.Unmarshal([]byte(raw), &rev)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &rev); err != nil {
		t.Fatal(err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("post-refund balance=%v want 0 (no compensation)", b)
	}
	var sum int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&sum)
	if sum != -1000 {
		t.Fatalf("cumulative=%v want exactly -1000", sum)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("reversal rows=%d want 1", c)
	}
	// And the provider refund fact itself stays truthful (1080 persisted).
	k, r := dispFact(t, pool, code)
	if k != "refund" || r == nil || *r != 1080 {
		t.Fatalf("refund fact = (%q, %v), want (refund, 1080)", k, r)
	}
}

// D. durable dispute fact exists but the economic reversal was never
// applied (e.g. crash between INSERT and ledger write is impossible in
// one tx — but an A1-era fact predating this policy, or a fact written
// with zero reversal under the old code, is exactly this state).
// A subsequent valid event must converge to target C.
func TestDisputeDurableFactConvergesLater(t *testing.T) {
	pool, _, fc, rt, code := dispSeed(t)
	p, _ := GetPayment(context.Background(), pool, code)
	zero := int64(0)
	adjTxn(fc, "txn_t4", *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)

	// Seed the durable dispute fact WITHOUT running the reversal path
	// (simulates a pre-policy fact row / partial-history state).
	var payID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM tb_payments WHERE code=$1`, code).Scan(&payID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO tb_payment_adjustments (
			payment_id, provider, provider_event_id, provider_object_id, kind,
			provider_transaction_id, provider_order_id,
			amount_minor, currency, transaction_amount_minor,
			amount_paid_minor, refunded_amount_minor,
			transaction_status, provider_created_at
		) VALUES ($1,'creem','evt_t4','dp_t4','dispute','txn_t4',$2,1080,'USD',1000,1080,0,'paid',1758000000)`,
		payID, p.ProviderOrderID); err != nil {
		t.Fatal(err)
	}
	botID := p.BotID
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("seed state: balance=%v want 1000 (fact present, no reversal)", b)
	}

	// Subsequent valid event (the dispute object redelivered): the
	// durable dispute fact already exists, so the derived target is C
	// and the pending reversal converges — regardless of the fact's own
	// zero refunded basis.
	if err := dispWebhook(t, pool, rt, "evt_t4c", "dp_t4", "txn_t4"); err != nil {
		t.Fatalf("same-object redelivery must reconcile: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("converged balance=%v want 0", b)
	}
	if c := dispReversalCount(t, pool, code); c != 1 {
		t.Fatalf("reversal rows=%d want 1", c)
	}
}
