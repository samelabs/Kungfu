package payment

// Dispute lifecycle authority regressions — written against the REAL
// official capability boundary, verified on docs.creem.io:
//
//   - dispute.created is "dispute opened", NOT a terminal outcome
//   - Creem has NO dispute resolution webhook and NO disputes API
//   - the ONLY machine-readable terminal economic fact is the
//     authoritative Transaction (GET /v1/transactions): a lost dispute
//     (chargeback) is recorded there as a refund movement
//     (status chargedBack / cumulative refunded_amount); a won dispute
//     NEVER produces a refund movement
//   - webhook success (200) implies NO future redelivery, so the ONLY
//     durable convergence path for an unresolved dispute is the
//     periodic re-read of the authoritative transaction
//
// These tests use exactly that: ONE dispute.created delivery, then the
// provider's transaction state advancing outside Kungfu (as the real
// provider does), observed only through ReconcileUnresolvedDisputes.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"kungfu.md/internal/pg"
)

// disputeWebhook delivers a single guaranteed-fields dispute.created.
func disputeWebhook(t *testing.T, pool *pg.Pool, rt *CreemRuntime, evtID, objID, txnID string) error {
	t.Helper()
	raw := `{"id":"` + evtID + `","eventType":"dispute.created","created_at":1758000000,
		"object":{"id":"` + objID + `","amount":1080,"transaction":{"id":"` + txnID + `"}}}`
	var ev CreemWebhookEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return HandleCreemAdjustmentEvent(context.Background(), pool, rt, &ev)
}

func reversalCount(t *testing.T, pool *pg.Pool, code string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDisputeOpenedZeroBasisThenWorkerLost: one dispute.created while
// the authoritative transaction shows no refund movement → durable
// fact, zero reversal. The provider later records the chargeback as a
// refund movement (transaction state advances OUTSIDE Kungfu — no
// second webhook exists). The worker's re-read converges to the full
// exactly-once reversal.
func TestDisputeOpenedZeroBasisThenWorkerLost(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	txnID := "txn_dwl"
	zero := int64(0)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)

	// The single webhook delivery (this is ALL Creem ever sends).
	if err := disputeWebhook(t, pool, rt, "evt_dw1", "dp_1", txnID); err != nil {
		t.Fatalf("opened dispute must record durably: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("opened dispute must not reverse: balance=%v", b)
	}

	// Provider records the chargeback as a full refund movement.
	full := int64(1080)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "chargedBack", &full)

	// Worker pass 1: converges to the full reversal.
	n, err := ReconcileUnresolvedDisputes(context.Background(), pool, rt)
	if err != nil || n != 1 {
		t.Fatalf("worker lost pass: n=%d err=%v", n, err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("lost dispute full reversal: balance=%v want 0", b)
	}
	if c := reversalCount(t, pool, code); c != 1 {
		t.Fatalf("reversal rows=%d want 1", c)
	}

	// Worker passes 2..3: nothing left unresolved; no double effect.
	for i := 0; i < 2; i++ {
		n, err := ReconcileUnresolvedDisputes(context.Background(), pool, rt)
		if err != nil || n != 0 {
			t.Fatalf("post-lost pass %d: n=%d err=%v", i, n, err)
		}
		if b := crBalance(t, pool, botID); b != 0 {
			t.Fatalf("post-lost balance=%v", b)
		}
		if c := reversalCount(t, pool, code); c != 1 {
			t.Fatalf("post-lost reversal rows=%d", c)
		}
	}
}

// TestDisputeOpenedZeroBasisThenWorkerWon: the dispute resolves in the
// merchant's favor — the authoritative transaction NEVER records a
// refund movement. The worker re-reads repeatedly; zero reversal ever.
func TestDisputeOpenedZeroBasisThenWorkerWon(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	txnID := "txn_dww"
	zero := int64(0)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	if err := disputeWebhook(t, pool, rt, "evt_dw2", "dp_2", txnID); err != nil {
		t.Fatalf("opened dispute must record durably: %v", err)
	}

	for i := 0; i < 3; i++ {
		n, err := ReconcileUnresolvedDisputes(context.Background(), pool, rt)
		if err != nil || n != 0 {
			t.Fatalf("won pass %d: n=%d err=%v", i, n, err)
		}
		if b := crBalance(t, pool, botID); b != 1000 {
			t.Fatalf("won dispute must never reverse: balance=%v", b)
		}
	}
	if c := reversalCount(t, pool, code); c != 0 {
		t.Fatalf("won dispute left reversal rows=%d", c)
	}
}

// TestDisputeWorkerCrashRecovery: the provider goes ambiguous BETWEEN
// worker passes (process-crash / provider-ambiguity equivalence: the
// durable zero-basis fact survives, nothing was mutated). When the
// provider becomes readable again and shows the chargeback movement,
// the next pass converges exactly-once.
func TestDisputeWorkerCrashRecovery(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	txnID := "txn_dwc"
	zero := int64(0)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	if err := disputeWebhook(t, pool, rt, "evt_dw3", "dp_3", txnID); err != nil {
		t.Fatal(err)
	}

	full := int64(1080)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "chargedBack", &full)

	// Provider ambiguous: every pass reports and mutates nothing.
	fc.setAmbiguous(txnID, true)
	for i := 0; i < 2; i++ {
		n, err := ReconcileUnresolvedDisputes(context.Background(), pool, rt)
		if err == nil {
			t.Fatalf("ambiguous pass %d must report", i)
		}
		if n != 0 {
			t.Fatalf("ambiguous pass %d advanced %d", i, n)
		}
		if b := crBalance(t, pool, botID); b != 1000 {
			t.Fatalf("ambiguous pass mutated balance=%v", b)
		}
	}

	// Provider readable again: converges exactly-once.
	fc.setAmbiguous(txnID, false)
	n, err := ReconcileUnresolvedDisputes(context.Background(), pool, rt)
	if err != nil || n != 1 {
		t.Fatalf("recovery pass: n=%d err=%v", n, err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("recovered reversal: balance=%v", b)
	}
	if c := reversalCount(t, pool, code); c != 1 {
		t.Fatalf("recovered rows=%d", c)
	}
}

// TestDisputeWorkerConcurrentWithWebhook: a late duplicate webhook
// delivery racing the worker's reconciliation must not double-reverse.
// Both funnel through RecordPaymentAdjustment's payment-row lock and
// the monotonic cumulative advance.
func TestDisputeWorkerConcurrentWithWebhook(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	txnID := "txn_dwx"
	zero := int64(0)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	if err := disputeWebhook(t, pool, rt, "evt_dw4", "dp_4", txnID); err != nil {
		t.Fatal(err)
	}

	full := int64(1080)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "chargedBack", &full)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = ReconcileUnresolvedDisputes(context.Background(), pool, rt)
			} else {
				_ = disputeWebhook(t, pool, rt, "evt_dw4_dup", "dp_4", txnID)
			}
		}(i)
	}
	wg.Wait()

	// Final convergence check via one more worker pass.
	n, err := ReconcileUnresolvedDisputes(context.Background(), pool, rt)
	if err != nil {
		t.Fatalf("final pass: %v", err)
	}
	_ = n
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("concurrent lost dispute: balance=%v want 0", b)
	}
	if c := reversalCount(t, pool, code); c != 1 {
		t.Fatalf("concurrent reversal rows=%d want 1", c)
	}
}
