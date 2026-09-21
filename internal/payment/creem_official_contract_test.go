package payment

// Official Creem provider-contract wire tests. Every assertion here is
// against the CURRENT documented official contract (docs.creem.io):
//   - GET /v1/products/{id}          — x-api-key, ProductEntity direct
//   - POST /v1/checkouts             — x-api-key, CheckoutEntity direct,
//     request_id = Kungfu payment code
//   - GET /v1/transactions?transaction_id={id} — x-api-key,
//     TransactionEntity direct
// Webhook fixtures below carry only fields the official schema
// guarantees; economic authority for refund/dispute comes exclusively
// from the Transaction retrieval API.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
)

// Product retrieval: exact official wire — method, path, no query
// params, x-api-key header, direct object shape (no wrapper).
func TestOfficialProductRetrievalExactWire(t *testing.T) {
	fc := newFakeCreem(t, pkgProducts()...)
	client := &CreemClient{baseURL: fc.server.URL, apiKey: "sk_test_probe", http: &http.Client{}}
	var recReq *http.Request
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		recReq = r
		return http.DefaultTransport.RoundTrip(r)
	})
	client.http = &http.Client{Transport: rt}

	p, err := client.GetProduct(context.Background(), "prod_a")
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if p.ID != "prod_a" || p.Price != 1000 || p.Currency != "USD" {
		t.Fatalf("product = %+v", p)
	}
	if got := recReq; got == nil {
		t.Fatal("no request recorded")
	} else {
		if got.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", got.Method)
		}
		if got.URL.Path != "/v1/products/prod_a" {
			t.Fatalf("path = %s", got.URL.Path)
		}
		if got.URL.RawQuery != "" {
			t.Fatalf("query = %q, want empty", got.URL.RawQuery)
		}
		if got.Header.Get("x-api-key") != "sk_test_probe" {
			t.Fatalf("auth header missing/wrong: %q", got.Header.Get("x-api-key"))
		}
	}
	// 404 on unknown product must be a definitive error.
	if _, err := client.GetProduct(context.Background(), "prod_nope"); err == nil {
		t.Fatal("unknown product must error")
	}
}

// Checkout creation: request_id must be the Kungfu payment code; exact
// official wire — POST /v1/checkouts, x-api-key, JSON body.
func TestOfficialCheckoutRequestIdExactWire(t *testing.T) {
	fc := newFakeCreem(t, pkgProducts()...)
	client := &CreemClient{baseURL: fc.server.URL, apiKey: "sk_test_probe", http: &http.Client{}}

	var recReq *http.Request
	var recBody []byte
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		recReq = r
		b, _ := io.ReadAll(r.Body)
		recBody = b
		r.Body = io.NopCloser(strings.NewReader(string(recBody)))
		return http.DefaultTransport.RoundTrip(r)
	})
	client.http = &http.Client{Transport: rt}

	co, err := client.CreateCheckout(context.Background(), CreateCheckoutInput{
		ProductID: "prod_a", RequestID: "paycode_xyz", Units: 1, SuccessURL: "https://kungfu.md/s",
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if co.RequestID != "paycode_xyz" {
		t.Fatalf("checkout request_id = %q", co.RequestID)
	}
	if recReq == nil {
		t.Fatal("no request recorded")
	}
	if recReq.Method != http.MethodPost || recReq.URL.Path != "/v1/checkouts" {
		t.Fatalf("wire = %s %s", recReq.Method, recReq.URL.Path)
	}
	if recReq.Header.Get("x-api-key") != "sk_test_probe" {
		t.Fatal("x-api-key missing")
	}
	var sent map[string]interface{}
	if err := json.Unmarshal(recBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["request_id"] != "paycode_xyz" || sent["product_id"] != "prod_a" {
		t.Fatalf("body = %v", sent)
	}
}

// Transaction retrieval: exact official wire — GET with the single
// transaction_id query parameter, x-api-key, TransactionEntity direct.
func TestOfficialTransactionRetrievalExactWire(t *testing.T) {
	fc := newFakeCreem(t, pkgProducts()...)
	refunded := int64(540)
	fc.setTxn(&CreemTransactionEntity{
		ID: "txn_1", Object: "transaction", Amount: 1000, AmountPaid: int64Ptr(1080),
		RefundedAmount: &refunded, Currency: "USD", Status: "succeeded", Order: "ord_1", Mode: "test",
	})
	client := &CreemClient{baseURL: fc.server.URL, apiKey: "sk_test_probe", http: &http.Client{}}

	var recReq *http.Request
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		recReq = r
		return http.DefaultTransport.RoundTrip(r)
	})
	client.http = &http.Client{Transport: rt}

	txn, err := client.GetTransaction(context.Background(), "txn_1")
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if txn.ID != "txn_1" || *txn.AmountPaid != 1080 || *txn.RefundedAmount != 540 || txn.Order != "ord_1" {
		t.Fatalf("txn = %+v", txn)
	}
	if recReq.Method != http.MethodGet || recReq.URL.Path != "/v1/transactions" {
		t.Fatalf("wire = %s %s", recReq.Method, recReq.URL.Path)
	}
	if q := recReq.URL.Query().Get("transaction_id"); q != "txn_1" {
		t.Fatalf("transaction_id = %q", q)
	}
	if recReq.Header.Get("x-api-key") != "sk_test_probe" {
		t.Fatal("x-api-key missing")
	}
	// unknown id → definitive 404 error
	if _, err := client.GetTransaction(context.Background(), "txn_missing"); err == nil {
		t.Fatal("unknown txn must error")
	}
}

// checkout.completed official fixture: the object carries the order id
// and payment/checkout facts; Payment Core reconcile must accept it.
func TestOfficialCheckoutCompletedFixture(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	res, err := StartCreemCheckout(context.Background(), pool, rt, botID, "starter")
	if err != nil {
		t.Fatal(err)
	}
	// official-shaped completion event (checkout object with embedded
	// order fact — the documented checkout.completed shape)
	raw := `{"id":"evt_official_cc","eventType":"checkout.completed","created_at":1758000000,
		"object":{"id":"ch_official_1","object_type":"checkout","status":"completed",
		"request_id":"` + res.Payment.Code + `","mode":"test",
		"metadata":{"payment_code":"` + res.Payment.Code + `","bot_id":"` + fmtInt(botID) + `","source":"kungfu_owner"},
		"order":{"id":"ord_official_1","status":"paid","product":"prod_a","currency":"USD","amount":1000,"units":1}}}`
	var ev CreemWebhookEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err != nil {
		t.Fatalf("official checkout.completed must reconcile: %v", err)
	}
	p := mustGetPayment(t, pool, res.Payment.Code)
	if p.Status != "paid" {
		t.Fatalf("status = %s", p.Status)
	}
	if p.ProviderOrderID == nil || *p.ProviderOrderID != "ord_official_1" {
		t.Fatalf("order binding = %v", p.ProviderOrderID)
	}
}

// refund.created official fixture: guaranteed fields ONLY — no order,
// no currency, no refunded_amount in the webhook; economic facts come
// from the authoritative transaction API.
func TestOfficialRefundCreatedFixture(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(540)
	adjTxn(fc, "txn_official_r", *p.ProviderOrderID, 1000, 1080, "USD", "succeeded", &refunded)
	raw := `{"id":"evt_official_rf","eventType":"refund.created","created_at":1758000000,
		"object":{"id":"re_1","status":"succeeded","refund_amount":540,
		"transaction":{"id":"txn_official_r"}}}`
	var ev CreemWebhookEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), &ev); err != nil {
		t.Fatalf("official refund.created must reconcile: %v", err)
	}
	var refundedStored, amountPaid int64
	if err := pool.QueryRow(context.Background(),
		`SELECT a.refunded_amount_minor, a.amount_paid_minor FROM tb_payment_adjustments a
		 JOIN tb_payments pp ON pp.id=a.payment_id WHERE pp.code=$1 AND a.kind='refund'`, code).
		Scan(&refundedStored, &amountPaid); err != nil {
		t.Fatal(err)
	}
	if refundedStored != 540 || amountPaid != 1080 {
		t.Fatalf("basis = %d/%d — must come from authoritative txn", refundedStored, amountPaid)
	}
}

// dispute.created official fixture: guaranteed fields only; the
// authoritative transaction carries the reversal basis.
func TestOfficialDisputeCreatedFixture(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	refunded := int64(1080)
	adjTxn(fc, "txn_official_d", *p.ProviderOrderID, 1000, 1080, "USD", "chargeback", &refunded)
	raw := `{"id":"evt_official_ds","eventType":"dispute.created","created_at":1758000000,
		"object":{"id":"dp_1","amount":1080,"transaction":{"id":"txn_official_d"}}}`
	var ev CreemWebhookEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), &ev); err != nil {
		t.Fatalf("official dispute.created must reconcile: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("balance = %v, want 0 (full chargeback reversal)", b)
	}
}

// Missing/partial provider facts fail closed: no authoritative txn on
// the provider → zero rows, zero economic effect, error returned.
func TestOfficialMissingProviderFactsFailClosed(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	code := adjSeedPaidPayment(t, pool, fc, botID)

	// webhook references a transaction the provider does not know
	raw := `{"id":"evt_official_miss","eventType":"refund.created","created_at":1758000000,
		"object":{"id":"re_2","status":"succeeded","refund_amount":540,
		"transaction":{"id":"txn_not_on_provider"}}}`
	var ev CreemWebhookEvent
	_ = json.Unmarshal([]byte(raw), &ev)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, fc.runtime(), &ev); err == nil {
		t.Fatal("missing authoritative txn must fail closed")
	}
	adjN, txN, bal := adjCounts(t, pool, code)
	if adjN != 0 || txN != 1 || bal != 1000 {
		t.Fatalf("adj=%d tx=%d bal=%v — fail-closed must leave zero effect", adjN, txN, bal)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func int64Ptr(v int64) *int64 { return &v }

func fmtInt(v int64) string { return fmt.Sprintf("%d", v) }

var _ = pg.Pool{} // keep pg import for future fixtures

// Dispute authority regressions against the REAL official capability
// boundary (verified against docs.creem.io): Creem has NO dispute
// resolution webhook and NO disputes API. The single machine-readable
// terminal economic authority is the authoritative Transaction
// (GET /v1/transactions): a chargeback is recorded there as a refund
// movement (status chargedBack, cumulative refunded_amount). A dispute
// that never produces a refund movement (won / no clawback) must never
// move Credits.

// TestOfficialDisputePendingThenLost: dispute.created arrives while the
// authoritative transaction shows NO refund movement (dispute opened,
// outcome pending) → durable fact, zero reversal. The same dispute is
// redelivered AFTER the provider recorded the chargeback as a refund
// (authoritative status chargedBack, full cumulative refunded_amount) →
// exactly-once full reversal through the same single cumulative path.
func TestOfficialDisputePendingThenLost(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	txnID := "txn_dpl"
	// Phase 1 — pending: no refund movement on the authoritative fact.
	zero := int64(0)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	rawPending := `{"id":"evt_dp1","eventType":"dispute.created","created_at":1758000000,
		"object":{"id":"dp_pending","amount":1080,"transaction":{"id":"` + txnID + `"}}}`
	var ev1 CreemWebhookEvent
	_ = json.Unmarshal([]byte(rawPending), &ev1)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &ev1); err != nil {
		t.Fatalf("pending dispute must record durably: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("pending dispute must not reverse: balance=%v", b)
	}

	// Phase 2 — lost: the provider recorded the chargeback as a full
	// refund on the authoritative transaction (the ONLY terminal proof
	// Creem exposes). Same dispute id redelivered (real retry semantics).
	full := int64(1080)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "chargedBack", &full)
	rawLost := `{"id":"evt_dp2","eventType":"dispute.created","created_at":1758000100,
		"object":{"id":"dp_pending","amount":1080,"transaction":{"id":"` + txnID + `"}}}`
	var ev2 CreemWebhookEvent
	_ = json.Unmarshal([]byte(rawLost), &ev2)
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &ev2); err != nil {
		t.Fatalf("lost dispute must reconcile: %v", err)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("lost dispute full reversal: balance=%v, want 0", b)
	}
	var revCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&revCount)
	if revCount != 1 {
		t.Fatalf("reversal rows = %d, want exactly 1", revCount)
	}
	// Third delivery (duplicate of the lost fact) → still one reversal.
	if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &ev2); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&revCount)
	if revCount != 1 {
		t.Fatalf("duplicate lost dispute reversed again: rows=%d", revCount)
	}
}

// TestOfficialDisputePendingThenWon: the dispute is resolved in the
// merchant's favor — the authoritative transaction NEVER records any
// refund movement. Redelivery keeps producing durable facts with zero
// Credits reversal; no permanent reversal is ever created.
func TestOfficialDisputePendingThenWon(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	code := adjSeedPaidPayment(t, pool, fc, botID)
	p, _ := GetPayment(context.Background(), pool, code)

	txnID := "txn_dpw"
	zero := int64(0)
	adjTxn(fc, txnID, *p.ProviderOrderID, 1000, 1080, "USD", "paid", &zero)
	raw := `{"id":"evt_dw","eventType":"dispute.created","created_at":1758000000,
		"object":{"id":"dp_won","amount":1080,"transaction":{"id":"` + txnID + `"}}}`
	var ev CreemWebhookEvent
	_ = json.Unmarshal([]byte(raw), &ev)
	for i := 0; i < 2; i++ {
		if err := HandleCreemAdjustmentEvent(context.Background(), pool, rt, &ev); err != nil {
			t.Fatalf("won dispute delivery %d must record durably: %v", i, err)
		}
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("won dispute must never reverse: balance=%v", b)
	}
	var revCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE ref_id=$1 AND type='reverse_payment'`, code).Scan(&revCount)
	if revCount != 0 {
		t.Fatalf("won dispute left a reversal: rows=%d", revCount)
	}
}
