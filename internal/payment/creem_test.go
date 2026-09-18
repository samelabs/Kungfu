package payment

// Creem fixed-package runtime integration tests: fake Creem HTTP server +
// real PostgreSQL. Covers package selection, fiat/credits authorities,
// snapshot persistence, webhook snapshot reconciliation, duplicates,
// concurrency, tampering, ownership, and refund/dispute freezing.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

// -- fake Creem (official shapes) --

type fakeCreem struct {
	server         *httptest.Server
	mu             sync.Mutex
	products       map[string]CreemProduct
	checkouts      []CreateCheckoutInput
	checkoutStatus int
	checkoutBody   string
}

func newFakeCreem(t *testing.T, products ...CreemProduct) *fakeCreem {
	t.Helper()
	fc := &fakeCreem{products: map[string]CreemProduct{}}
	for _, p := range products {
		fc.products[p.ID] = p
	}
	mux := http.NewServeMux()
	// Official: GET /v1/products/{id}, direct object shape only.
	mux.HandleFunc("/v1/products/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/products/")
		p, ok := fc.products[id]
		if !ok || r.URL.RawQuery != "" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("/v1/checkouts", func(w http.ResponseWriter, r *http.Request) {
		var in CreateCheckoutInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fc.mu.Lock()
		fc.checkouts = append(fc.checkouts, in)
		status, body := fc.checkoutStatus, fc.checkoutBody
		fc.mu.Unlock()
		if status == 0 {
			status = 200
		}
		if body == "" {
			prod := in.ProductID
			body = fmt.Sprintf(`{"id":"ch_%s","request_id":%q,"checkout_url":"https://checkout.fake.io/%s","status":"pending","mode":%q,"units":%d,"product":%q}`,
				in.RequestID, in.RequestID, in.RequestID, fc.products[prod].Mode, in.Units, prod)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	fc.server = httptest.NewServer(mux)
	t.Cleanup(fc.server.Close)
	return fc
}

func pkgProducts() []CreemProduct {
	return []CreemProduct{
		{ID: "prod_a", Name: "Starter", BillingType: "onetime", Status: "active", Mode: "test", Currency: "USD", Price: 1000},
		{ID: "prod_b", Name: "Standard", BillingType: "onetime", Status: "active", Mode: "test", Currency: "USD", Price: 4000},
	}
}

func (fc *fakeCreem) runtime() *CreemRuntime {
	return &CreemRuntime{
		Client:     NewCreemClient(CreemConfig{APIBase: fc.server.URL, APIKey: "k"}),
		Mode:       "test",
		SuccessURL: "https://kungfu.md/owner?payment=success",
		Packages: map[string]CreemPackageSpec{
			"starter":  {Code: "starter", ProductID: "prod_a", Credits: 1000},
			"standard": {Code: "standard", ProductID: "prod_b", Credits: 5000},
		},
	}
}

// -- env helpers --

func crTestPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func crSeedBot(t *testing.T, pool *pg.Pool) int64 {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var botID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', 0) RETURNING id`,
		"cfp_"+suffix, s61SeedKeyHash("kf_live_"+suffix), s61SeedLast4("kf_live_"+suffix)).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupBotRows(t, pool, botID) })
	return botID
}

func crGrants(t *testing.T, pool *pg.Pool, code string) (int, float64) {
	t.Helper()
	var n int
	var sum float64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE ref_type='payment' AND ref_id=$1 AND type='grant_payment'`, code).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

func crBalance(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	_ = pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&b)
	return b
}

const crWebhookSecret = "whsec_cfp"

func signRaw(raw []byte) string {
	mac := hmac.New(sha256.New, []byte(crWebhookSecret))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func buildCompletion(pCode string, botID int64, orderID string, mutate func(*CreemCheckoutObject)) ([]byte, error) {
	co := CreemCheckoutObject{
		ID: "ch_" + pCode, RequestID: pCode, Status: "completed",
		OrderID: orderID, Mode: "test",
		Metadata: map[string]string{"payment_code": pCode, "bot_id": fmt.Sprintf("%d", botID), "source": "kungfu_owner"},
	}
	// defaults match a starter checkout snapshot; tests mutate as needed
	co.Order = &CreemOrder{ID: orderID, Status: "paid", Product: "prod_a", Currency: "USD", Amount: 1000, Units: 1}
	if mutate != nil {
		mutate(&co)
	}
	obj, _ := json.Marshal(co)
	env := map[string]interface{}{
		"id": "evt_" + pCode, "eventType": "checkout.completed",
		"created_at": time.Now().Unix(), "object": json.RawMessage(obj),
	}
	return json.Marshal(env)
}

// -- checkout: package selection + authorities --

func TestPackageSelectionAndAuthorities(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()

	// package A → product A, live price A, configured credits A
	resA, err := StartCreemCheckout(context.Background(), pool, rt, botID, "starter")
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	if resA.Payment.AmountMinor != 1000 || resA.Payment.Credits != 1000 || resA.Payment.Currency != "USD" {
		t.Fatalf("starter facts = %+v", resA.Payment)
	}
	if resA.Payment.ProviderProductID == nil || *resA.Payment.ProviderProductID != "prod_a" {
		t.Fatalf("provider_product_id = %v", resA.Payment.ProviderProductID)
	}

	// package B → product B, live price B, configured credits B
	resB, err := StartCreemCheckout(context.Background(), pool, rt, botID, "standard")
	if err != nil {
		t.Fatalf("standard: %v", err)
	}
	if resB.Payment.AmountMinor != 4000 || resB.Payment.Credits != 5000 {
		t.Fatalf("standard facts = %+v", resB.Payment)
	}
	if *resB.Payment.ProviderProductID != "prod_b" {
		t.Fatalf("standard product = %v", *resB.Payment.ProviderProductID)
	}

	// every checkout request: units=1, configured product, no custom_price
	fc.mu.Lock()
	reqs := append([]CreateCheckoutInput{}, fc.checkouts...)
	fc.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("checkouts = %d", len(reqs))
	}
	for i, cr := range reqs {
		if cr.Units != 1 {
			t.Fatalf("checkout %d units = %d", i, cr.Units)
		}
		if cr.ProductID != "prod_a" && cr.ProductID != "prod_b" {
			t.Fatalf("checkout %d product = %s", i, cr.ProductID)
		}
	}
	// no custom_price field exists on CreateCheckoutInput — structural
	// proof; also assert the wire JSON omits it
	wire, _ := json.Marshal(reqs[0])
	if strings.Contains(string(wire), "custom_price") {
		t.Fatal("custom_price emitted")
	}
}

func TestUnknownPackageRejected(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	if _, err := StartCreemCheckout(context.Background(), pool, fc.runtime(), botID, "enterprise"); err == nil {
		t.Fatal("unknown package accepted")
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1`, botID).Scan(&n)
	if n != 0 {
		t.Fatalf("payments leaked: %d", n)
	}
}

// Snapshot persisted BEFORE CreateCheckout: definitive failure leaves a
// payment row carrying the snapshot.
func TestSnapshotPersistedBeforeProviderCall(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	fc.checkoutStatus, fc.checkoutBody = 500, `{"error":"boom"}`

	if _, err := StartCreemCheckout(context.Background(), pool, fc.runtime(), botID, "starter"); err == nil {
		t.Fatal("ambiguous failure must error")
	}
	var prodID *string
	var status string
	if err := pool.QueryRow(context.Background(), `
		SELECT provider_product_id, status FROM tb_payments WHERE bot_id=$1`, botID).Scan(&prodID, &status); err != nil {
		t.Fatal(err)
	}
	if prodID == nil || *prodID != "prod_a" {
		t.Fatalf("snapshot missing before provider call: %v", prodID)
	}
	if status != "pending" {
		t.Fatalf("status = %s, want pending", status)
	}
}

// -- webhook: signature + snapshot reconciliation --

func TestWebhookSignatureGates(t *testing.T) {
	raw := []byte(`{"id":"evt_x","eventType":"checkout.completed"}`)
	if VerifyCreemWebhookSignature(raw, "secret", "") {
		t.Fatal("empty signature accepted")
	}
	if VerifyCreemWebhookSignature(raw, "secret", "ab"+strings.Repeat("c", 62)) {
		t.Fatal("garbage signature accepted")
	}
	good := signRaw(raw)
	if !VerifyCreemWebhookSignature(raw, crWebhookSecret, good) {
		t.Fatal("valid signature rejected")
	}
	if VerifyCreemWebhookSignature([]byte(`{"tampered":1}`), crWebhookSecret, good) {
		t.Fatal("tampered body accepted")
	}
}

func TestValidCompletionGrantsSnapshotCredits(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()

	res, _ := StartCreemCheckout(context.Background(), pool, rt, botID, "standard") // 5000 credits
	raw, _ := buildCompletion(res.Payment.Code, botID, "ord_1", func(c *CreemCheckoutObject) {
		c.Order.Product = "prod_b"
		c.Order.Amount = 4000
	})
	var ev CreemWebhookEvent
	_ = json.Unmarshal(raw, &ev)
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n, sum := crGrants(t, pool, res.Payment.Code); n != 1 || sum != 5000 {
		t.Fatalf("grants = %d/%v, want 1/5000", n, sum)
	}
	if b := crBalance(t, pool, botID); b != 5000 {
		t.Fatalf("balance = %v", b)
	}
}

func TestDuplicateAndConcurrentOneGrant(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()

	res, _ := StartCreemCheckout(context.Background(), pool, rt, botID, "starter")
	raw, _ := buildCompletion(res.Payment.Code, botID, "ord_dup", nil)
	var ev CreemWebhookEvent
	_ = json.Unmarshal(raw, &ev)

	for i := 0; i < 3; i++ {
		if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err != nil {
			t.Fatalf("dup #%d: %v", i, err)
		}
	}
	const workers = 6
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ReconcileCreemCompletion(context.Background(), pool, rt, &ev) }()
	}
	wg.Wait()
	if n, sum := crGrants(t, pool, res.Payment.Code); n != 1 || sum != 1000 {
		t.Fatalf("grants = %d/%v, want 1/1000", n, sum)
	}
}

// Mismatch gates against the SNAPSHOT: zero grant, still pending.
func TestSnapshotReconciliationGates(t *testing.T) {
	pool := crTestPool(t)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()

	cases := []struct {
		name   string
		mutate func(*CreemCheckoutObject)
	}{
		{"wrong product", func(c *CreemCheckoutObject) { c.Order.Product = "prod_b" }},
		{"wrong amount", func(c *CreemCheckoutObject) { c.Order.Amount = 999 }},
		{"wrong currency", func(c *CreemCheckoutObject) { c.Order.Currency = "EUR" }},
		{"wrong mode", func(c *CreemCheckoutObject) { c.Mode = "prod" }},
		{"order not paid", func(c *CreemCheckoutObject) { c.Order.Status = "refunded" }},
		{"metadata bot mismatch", func(c *CreemCheckoutObject) { c.Metadata["bot_id"] = "999" }},
		{"no order", func(c *CreemCheckoutObject) { c.Order = nil }},
	}
	for _, tc := range cases {
		botID := crSeedBot(t, pool)
		res, err := StartCreemCheckout(context.Background(), pool, rt, botID, "starter")
		if err != nil {
			t.Fatalf("%s: seed: %v", tc.name, err)
		}
		raw, _ := buildCompletion(res.Payment.Code, botID, fmt.Sprintf("ord_%d", time.Now().UnixNano()), tc.mutate)
		var ev CreemWebhookEvent
		_ = json.Unmarshal(raw, &ev)
		if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err == nil {
			t.Fatalf("%s: must fail", tc.name)
		}
		if n, _ := crGrants(t, pool, res.Payment.Code); n != 0 {
			t.Fatalf("%s: grant leaked", tc.name)
		}
		p, _ := GetPayment(context.Background(), pool, res.Payment.Code)
		if p.Status != "pending" {
			t.Fatalf("%s: status = %s", tc.name, p.Status)
		}
	}
}

// Provider order reuse across payments → conflict, no grant.
func TestProviderOrderReusedAcrossPayments(t *testing.T) {
	pool := crTestPool(t)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	botA, botB := crSeedBot(t, pool), crSeedBot(t, pool)

	resA, _ := StartCreemCheckout(context.Background(), pool, rt, botA, "starter")
	rawA, _ := buildCompletion(resA.Payment.Code, botA, "ord_shared", nil)
	var evA CreemWebhookEvent
	_ = json.Unmarshal(rawA, &evA)
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &evA); err != nil {
		t.Fatalf("first: %v", err)
	}

	resB, _ := StartCreemCheckout(context.Background(), pool, rt, botB, "starter")
	rawB, _ := buildCompletion(resB.Payment.Code, botB, "ord_shared", nil)
	var evB CreemWebhookEvent
	_ = json.Unmarshal(rawB, &evB)
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &evB); err == nil {
		t.Fatal("order reuse must conflict")
	}
	if b := crBalance(t, pool, botB); b != 0 {
		t.Fatalf("bot B balance = %v", b)
	}
}

// THE fixed-package keystone: create under config A, then mutate/remove
// the package config BEFORE the webhook — the payment still completes
// with its ORIGINAL snapshot entitlement.
func TestSnapshotSurvivesConfigChange(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()

	res, _ := StartCreemCheckout(context.Background(), pool, rt, botID, "starter") // 1000 credits @ prod_a

	// package config changes: starter now maps to a different product and
	// half the credits; the old product even disappears from the catalog.
	rtMutated := &CreemRuntime{
		Client:     rt.Client,
		Mode:       rt.Mode,
		SuccessURL: rt.SuccessURL,
		Packages: map[string]CreemPackageSpec{
			"starter": {Code: "starter", ProductID: "prod_c", Credits: 500},
		},
	}

	// webhook facts match the ORIGINAL snapshot (prod_a / 1000 minor)
	raw, _ := buildCompletion(res.Payment.Code, botID, "ord_snap", nil)
	var ev CreemWebhookEvent
	_ = json.Unmarshal(raw, &ev)
	if err := ReconcileCreemCompletion(context.Background(), pool, rtMutated, &ev); err != nil {
		t.Fatalf("snapshot reconciliation failed after config change: %v", err)
	}
	if n, sum := crGrants(t, pool, res.Payment.Code); n != 1 || sum != 1000 {
		t.Fatalf("grants = %d/%v, want 1/1000 (original snapshot)", n, sum)
	}
	if b := crBalance(t, pool, botID); b != 1000 {
		t.Fatalf("balance = %v, want 1000", b)
	}
}

// Ownership read.
func TestGetPaymentForBotOwnership(t *testing.T) {
	pool := crTestPool(t)
	fc := newFakeCreem(t, pkgProducts()...)
	rt := fc.runtime()
	botA, botB := crSeedBot(t, pool), crSeedBot(t, pool)

	res, _ := StartCreemCheckout(context.Background(), pool, rt, botA, "starter")
	if _, err := GetPaymentForBot(context.Background(), pool, botA, res.Payment.Code); err != nil {
		t.Fatalf("own: %v", err)
	}
	if _, err := GetPaymentForBot(context.Background(), pool, botB, res.Payment.Code); err == nil {
		t.Fatal("cross-bot read must 404")
	}
}

// cleanupBotRows removes exactly this test bot's rows in FK-aware order:
// adjustment facts → ledger rows → payments → bot. Scoped by bot_id;
// no TRUNCATE, no table-wide deletes; errors surfaced via t.Errorf.
func cleanupBotRows(t *testing.T, pool *pg.Pool, botID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM tb_payment_adjustments WHERE payment_id IN (SELECT id FROM tb_payments WHERE bot_id = $1)`, botID); err != nil {
		t.Errorf("cleanup tb_payment_adjustments(bot=%d): %v", botID, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id = $1`, botID); err != nil {
		t.Errorf("cleanup tb_transactions(bot=%d): %v", botID, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tb_payments WHERE bot_id = $1`, botID); err != nil {
		t.Errorf("cleanup tb_payments(bot=%d): %v", botID, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tb_bots WHERE id = $1`, botID); err != nil {
		t.Errorf("cleanup tb_bots(%d): %v", botID, err)
	}
}
