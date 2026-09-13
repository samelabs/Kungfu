package payment

// Creem runtime integration tests: fake Creem HTTP server + real
// PostgreSQL. Covers config gating, product validation, units math,
// checkout creation, webhook signature + full reconciliation gates,
// duplicate/concurrent grants, provider-order binding, and ownership.

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

// -- fake Creem server --

type fakeCreem struct {
	server *httptest.Server
	mu     sync.Mutex
	// configured product returned by GET /v1/products
	product CreemProduct
	// checkout creation behavior
	checkoutStatus int // HTTP status for POST /v1/checkouts
	checkoutBody   string
	checkouts      []CreateCheckoutInput // recorded requests
	productPathHit *bool                 // official path endpoint was used
}

func newFakeCreem(t *testing.T, product CreemProduct) *fakeCreem {
	t.Helper()
	fc := &fakeCreem{product: product, checkoutStatus: 200}
	mux := http.NewServeMux()
	// Official endpoint: GET /v1/products/{id}. The query-string form is
	// deliberately NOT implemented so a regression to the old endpoint
	// fails loudly (404).
	productPathHit := false
	mux.HandleFunc("/v1/products/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/products/")
		if id == "" || id != fc.product.ID || r.URL.RawQuery != "" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		productPathHit = true
		// Official direct object shape.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fc.product)
	})
	fc.productPathHit = &productPathHit
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
			// Official direct object shape with the full fact set.
			body = fmt.Sprintf(`{"id":"ch_test_%s","request_id":%q,"checkout_url":"https://checkout.fake.io/%s","status":"pending","mode":"test","units":%d,"product":%q}`,
				in.RequestID, in.RequestID, in.RequestID, in.Units, fc.product.ID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	fc.server = httptest.NewServer(mux)
	t.Cleanup(fc.server.Close)
	return fc
}

func (fc *fakeCreem) runtime() *CreemRuntime {
	return &CreemRuntime{
		Client:         NewCreemClient(CreemConfig{APIBase: fc.server.URL, APIKey: "test-key"}),
		ProductID:      fc.product.ID,
		CreditsPerUnit: 100,
		Mode:           "test",
		SuccessURL:     "https://kungfu.md/owner?payment=success",
	}
}

func goodProduct() CreemProduct {
	return CreemProduct{
		ID: "prod_test123", Name: "Kungfu Credits", BillingType: "onetime",
		Status: "active", Mode: "test", Currency: "USD", Price: 1000,
	}
}

// -- test env helpers --

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
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1, $2, 'x', 'active', 0) RETURNING id`,
		"crbot_"+suffix, "kf_live_"+suffix).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM tb_payments WHERE bot_id = $1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func crGrants(t *testing.T, pool *pg.Pool, code string) (int, float64) {
	t.Helper()
	var n int
	var sum float64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE ref_type = 'payment' AND ref_id = $1 AND type = 'grant_payment'`, code).
		Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

func crBalance(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id = $1`, botID).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// buildCompletion assembles a valid signed checkout.completed envelope.
func buildCompletion(t *testing.T, rt *CreemRuntime, pCode string, botID int64, orderID string, mutate func(*CreemCheckoutObject)) ([]byte, string) {
	t.Helper()
	co := CreemCheckoutObject{
		ID:        "ch_test_" + pCode,
		RequestID: pCode,
		Status:    "completed",
		OrderID:   orderID,
		Mode:      rt.Mode,
		Metadata: map[string]string{
			"payment_code": pCode,
			"bot_id":       fmt.Sprintf("%d", botID),
			"source":       "kungfu_owner",
		},
		Order: &CreemOrder{
			ID: orderID, Status: "paid", Product: rt.ProductID,
			Currency: "USD", Amount: 1000, Units: 1,
		},
	}
	if mutate != nil {
		mutate(&co)
	}
	obj, _ := json.Marshal(co)
	env := map[string]interface{}{
		"id": "evt_" + pCode, "eventType": "checkout.completed",
		"created_at": time.Now().Unix(), "object": json.RawMessage(obj),
	}
	raw, _ := json.Marshal(env)
	return raw, signRaw(raw)
}

var crWebhookSecret = "whsec_test_secret"

func signRaw(raw []byte) string {
	mac := hmac.New(sha256.New, []byte(crWebhookSecret))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// -- 3. GetProduct validation --

func TestCreemProductValidation(t *testing.T) {
	base := goodProduct()
	cases := []struct {
		name   string
		mutate func(*CreemProduct)
	}{
		{"wrong id", func(p *CreemProduct) { p.ID = "prod_other" }},
		{"not onetime", func(p *CreemProduct) { p.BillingType = "subscription" }},
		{"inactive", func(p *CreemProduct) { p.Status = "archived" }},
		{"wrong mode", func(p *CreemProduct) { p.Mode = "prod" }},
		{"zero price", func(p *CreemProduct) { p.Price = 0 }},
		{"bad currency", func(p *CreemProduct) { p.Currency = "usd" }},
	}
	for _, tc := range cases {
		prod := base
		tc.mutate(&prod)
		if err := validateCreemProduct(&prod, base.ID, "test"); err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
	}
	if err := validateCreemProduct(&base, base.ID, "test"); err != nil {
		t.Fatalf("valid product rejected: %v", err)
	}
}

// -- 5/6/7/8. units math + payment row + request_id + metadata --

func TestStartCheckoutUnitsAndPaymentFacts(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	res, err := StartCreemCheckout(context.Background(), pool, rt, botID, 3)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	p := res.Payment
	if p.AmountMinor != 3000 { // 1000 x 3
		t.Fatalf("amount_minor = %d, want 3000", p.AmountMinor)
	}
	if p.Credits != 300 { // 100 x 3
		t.Fatalf("credits = %v, want 300", p.Credits)
	}
	if p.Provider != "creem" || p.Currency != "USD" || p.Status != "pending" {
		t.Fatalf("payment facts: %+v", p)
	}

	fc.mu.Lock()
	reqs := append([]CreateCheckoutInput{}, fc.checkouts...)
	fc.mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("checkout requests = %d, want 1", len(reqs))
	}
	cr := reqs[0]
	if cr.RequestID != p.Code {
		t.Fatalf("request_id = %s, want payment code %s", cr.RequestID, p.Code)
	}
	if cr.ProductID != rt.ProductID {
		t.Fatalf("product_id = %s", cr.ProductID)
	}
	if cr.Units != 3 {
		t.Fatalf("units = %d", cr.Units)
	}
	if cr.Metadata["payment_code"] != p.Code || cr.Metadata["bot_id"] != fmt.Sprintf("%d", botID) {
		t.Fatalf("metadata = %v", cr.Metadata)
	}
	if !strings.HasPrefix(res.CheckoutURL, "https://checkout.fake.io/") {
		t.Fatalf("checkout_url = %s", res.CheckoutURL)
	}
}

// -- 4. invalid units / no invented cap / overflow --

func TestStartCheckoutInvalidUnits(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	// Only non-positive units are invalid — there is no Kungfu-side
	// purchase cap (provider limits are Creem's to enforce).
	for _, units := range []int64{0, -1} {
		if _, err := StartCreemCheckout(context.Background(), pool, rt, botID, units); err == nil {
			t.Fatalf("units %d accepted", units)
		}
	}

	// units above any old fixed cap must be accepted (here: 5000 units
	// of a 1000-minor product computes safely).
	res, err := StartCreemCheckout(context.Background(), pool, rt, botID, 5000)
	if err != nil {
		t.Fatalf("units 5000 must be accepted: %v", err)
	}
	if res.Payment.AmountMinor != 5000000 || res.Payment.Credits != 500000 {
		t.Fatalf("5000-unit facts = %d/%v", res.Payment.AmountMinor, res.Payment.Credits)
	}
}

func TestStartCheckoutOverflowRejected(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	// A product priced near int64 max makes moderate units overflow.
	big := goodProduct()
	big.Price = (1 << 62)
	fc := newFakeCreem(t, big)
	rt := fc.runtime()

	// amount overflow
	if _, err := StartCreemCheckout(context.Background(), pool, rt, botID, 8); err == nil {
		t.Fatal("amount overflow accepted")
	}

	// credits overflow: normal price, huge per-unit credits
	normal := newFakeCreem(t, goodProduct())
	rt2 := normal.runtime()
	rt2.CreditsPerUnit = (1 << 62)
	if _, err := StartCreemCheckout(context.Background(), pool, rt2, botID, 8); err == nil {
		t.Fatal("credits overflow accepted")
	}
}

// -- 9. definitive vs ambiguous provider failure --

func TestCheckoutProviderFailureStates(t *testing.T) {
	pool := crTestPool(t)

	// definitive 401 → payment failed
	{
		botID := crSeedBot(t, pool)
		fc := newFakeCreem(t, goodProduct())
		fc.checkoutStatus, fc.checkoutBody = 401, `{"error":"unauthorized"}`
		_, err := StartCreemCheckout(context.Background(), pool, fc.runtime(), botID, 1)
		if err == nil {
			t.Fatal("definitive failure must error")
		}
		var n int
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1 AND status='failed'`, botID).Scan(&n)
		if n != 1 {
			t.Fatalf("failed payments = %d, want 1", n)
		}
	}

	// ambiguous 500 → payment stays pending, message carries no retry
	// semantics, zero grant
	{
		botID := crSeedBot(t, pool)
		fc := newFakeCreem(t, goodProduct())
		fc.checkoutStatus, fc.checkoutBody = 500, `{"error":"boom"}`
		_, err := StartCreemCheckout(context.Background(), pool, fc.runtime(), botID, 1)
		if err == nil {
			t.Fatal("ambiguous failure must error")
		}
		msg := err.Error()
		for _, banned := range []string{"retried", "retry"} {
			if strings.Contains(strings.ToLower(msg), strings.ToLower(banned)) {
				t.Fatalf("ambiguous message implies retry: %s", msg)
			}
		}
		var n int
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1 AND status='pending'`, botID).Scan(&n)
		if n != 1 {
			t.Fatalf("pending payments = %d, want 1 (ambiguous stays pending)", n)
		}
		var txN int
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='grant_payment'`, botID).Scan(&txN)
		if txN != 0 {
			t.Fatalf("ambiguous failure granted: %d", txN)
		}
	}
}

// -- 11. webhook signature --

func TestWebhookSignatureVerification(t *testing.T) {
	raw := []byte(`{"eventType":"checkout.completed"}`)
	if VerifyCreemWebhookSignature(raw, "secret", "") {
		t.Fatal("empty signature accepted")
	}
	if VerifyCreemWebhookSignature(raw, "secret", "zz"+strings.Repeat("a", 62)) {
		t.Fatal("garbage signature accepted")
	}
	good := signRaw(raw)
	if !VerifyCreemWebhookSignature(raw, crWebhookSecret, good) {
		t.Fatal("valid signature rejected")
	}
	if VerifyCreemWebhookSignature([]byte(`{"tampered":true}`), crWebhookSecret, good) {
		t.Fatal("tampered body accepted")
	}
	if VerifyCreemWebhookSignature(raw, "other-secret", good) {
		t.Fatal("wrong secret accepted")
	}
}

// -- 14/15. valid completion → paid + exactly one grant --

func TestValidCompletionGrantsOnce(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	res, err := StartCreemCheckout(context.Background(), pool, rt, botID, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := buildCompletion(t, rt, res.Payment.Code, botID, "ord_1", nil)

	var ev CreemWebhookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	p, err := GetPayment(context.Background(), pool, res.Payment.Code)
	if err != nil || p.Status != "paid" {
		t.Fatalf("payment status = %s err = %v", p.Status, err)
	}
	if n, sum := crGrants(t, pool, res.Payment.Code); n != 1 || sum != 100 {
		t.Fatalf("grants = %d/%v, want 1/100", n, sum)
	}
	if b := crBalance(t, pool, botID); b != 100 {
		t.Fatalf("balance = %v", b)
	}
}

// -- 16/17. duplicate + concurrent duplicate webhook → one grant --

func TestDuplicateAndConcurrentCompletionOneGrant(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	res, _ := StartCreemCheckout(context.Background(), pool, rt, botID, 1)
	raw, _ := buildCompletion(t, rt, res.Payment.Code, botID, "ord_dup", nil)
	var ev CreemWebhookEvent
	_ = json.Unmarshal(raw, &ev)

	// serial duplicates
	for i := 0; i < 3; i++ {
		if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err != nil {
			t.Fatalf("duplicate #%d: %v", i, err)
		}
	}

	// concurrent duplicates
	const workers = 6
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ReconcileCreemCompletion(context.Background(), pool, rt, &ev)
		}()
	}
	wg.Wait()

	if n, sum := crGrants(t, pool, res.Payment.Code); n != 1 || sum != 100 {
		t.Fatalf("grants after duplicates = %d/%v, want 1/100", n, sum)
	}
	if b := crBalance(t, pool, botID); b != 100 {
		t.Fatalf("balance = %v, want 100", b)
	}
}

// -- 18-24. reconciliation gates: zero grant on every mismatch --

func TestReconciliationGatesZeroGrant(t *testing.T) {
	pool := crTestPool(t)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	cases := []struct {
		name   string
		mutate func(*CreemCheckoutObject)
	}{
		{"wrong product", func(c *CreemCheckoutObject) { c.Order.Product = "prod_other" }},
		{"wrong amount", func(c *CreemCheckoutObject) { c.Order.Amount = 999 }},
		{"wrong currency", func(c *CreemCheckoutObject) { c.Order.Currency = "EUR" }},
		{"wrong mode", func(c *CreemCheckoutObject) { c.Mode = "prod" }},
		{"order not paid", func(c *CreemCheckoutObject) { c.Order.Status = "refunded" }},
		{"checkout not completed", func(c *CreemCheckoutObject) { c.Status = "expired" }},
		{"metadata payment_code mismatch", func(c *CreemCheckoutObject) { c.Metadata["payment_code"] = "other" }},
		{"metadata bot_id mismatch", func(c *CreemCheckoutObject) { c.Metadata["bot_id"] = "999" }},
		{"no order", func(c *CreemCheckoutObject) { c.Order = nil }},
		{"empty order id", func(c *CreemCheckoutObject) { c.Order.ID = "" }},
	}
	for _, tc := range cases {
		botID := crSeedBot(t, pool)
		res, err := StartCreemCheckout(context.Background(), pool, rt, botID, 1)
		if err != nil {
			t.Fatalf("%s: seed checkout: %v", tc.name, err)
		}
		raw, _ := buildCompletion(t, rt, res.Payment.Code, botID, "ord_"+fmt.Sprintf("%d", time.Now().UnixNano()), tc.mutate)
		var ev CreemWebhookEvent
		_ = json.Unmarshal(raw, &ev)
		if err := ReconcileCreemCompletion(context.Background(), pool, rt, &ev); err == nil {
			t.Fatalf("%s: reconciliation must fail", tc.name)
		}
		if n, _ := crGrants(t, pool, res.Payment.Code); n != 0 {
			t.Fatalf("%s: grant leaked: %d", tc.name, n)
		}
		p, _ := GetPayment(context.Background(), pool, res.Payment.Code)
		if p.Status != "pending" {
			t.Fatalf("%s: status = %s, want pending", tc.name, p.Status)
		}
	}
}

// -- 25. provider order reuse across payments → conflict, no grant --

func TestProviderOrderReusedAcrossPayments(t *testing.T) {
	pool := crTestPool(t)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	botA := crSeedBot(t, pool)
	botB := crSeedBot(t, pool)

	resA, _ := StartCreemCheckout(context.Background(), pool, rt, botA, 1)
	resB, _ := StartCreemCheckout(context.Background(), pool, rt, botB, 1)

	// A completes with ord_shared and is granted.
	rawA, _ := buildCompletion(t, rt, resA.Payment.Code, botA, "ord_shared", nil)
	var evA CreemWebhookEvent
	_ = json.Unmarshal(rawA, &evA)
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &evA); err != nil {
		t.Fatalf("first completion: %v", err)
	}

	// B tries to complete with the SAME provider order: metadata matches B,
	// but the order is already bound to A's payment.
	rawB, _ := buildCompletion(t, rt, resB.Payment.Code, botB, "ord_shared", nil)
	var evB CreemWebhookEvent
	_ = json.Unmarshal(rawB, &evB)
	if err := ReconcileCreemCompletion(context.Background(), pool, rt, &evB); err == nil {
		t.Fatal("provider order reuse must conflict")
	}
	if n, _ := crGrants(t, pool, resB.Payment.Code); n != 0 {
		t.Fatalf("second payment granted: %d", n)
	}
	if b := crBalance(t, pool, botB); b != 0 {
		t.Fatalf("bot B balance = %v, want 0", b)
	}
}

// -- 27. GetPaymentForBot ownership --

func TestGetPaymentForBotOwnership(t *testing.T) {
	pool := crTestPool(t)
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	botA := crSeedBot(t, pool)
	botB := crSeedBot(t, pool)

	res, _ := StartCreemCheckout(context.Background(), pool, rt, botA, 1)

	if _, err := GetPaymentForBot(context.Background(), pool, botA, res.Payment.Code); err != nil {
		t.Fatalf("own payment: %v", err)
	}
	if _, err := GetPaymentForBot(context.Background(), pool, botB, res.Payment.Code); err == nil {
		t.Fatal("cross-bot payment read must 404")
	}
	if _, err := GetPaymentForBot(context.Background(), pool, botA, "nonexistent0"); err == nil {
		t.Fatal("missing payment must 404")
	}
}

// -- official-shape contract tests --

// TestGetProductUsesOfficialPathEndpoint: the client must call
// GET /v1/products/{id} (path form); the fake only implements that route
// and 404s anything else, so a passing fetch proves the path form.
// The direct object shape is the primary fixture.
func TestGetProductUsesOfficialPathEndpoint(t *testing.T) {
	fc := newFakeCreem(t, goodProduct())
	rt := fc.runtime()

	p, err := rt.Client.GetProduct(context.Background(), rt.ProductID)
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if p.ID != "prod_test123" || p.Price != 1000 {
		t.Fatalf("product = %+v", p)
	}
	if !*fc.productPathHit {
		t.Fatal("official path endpoint /v1/products/{id} was not used")
	}
}

// TestGetProductWrappedShapeStillParses: compatibility with the wrapped
// envelope remains, but the direct shape is the tested main path above.
func TestGetProductWrappedShapeStillParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"product":{"id":"prod_test123","billing_type":"onetime","status":"active","mode":"test","currency":"USD","price":1000}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewCreemClient(CreemConfig{APIBase: srv.URL, APIKey: "k"})
	p, err := c.GetProduct(context.Background(), "prod_test123")
	if err != nil || p.ID != "prod_test123" {
		t.Fatalf("wrapped shape: %v %+v", err, p)
	}
}

// TestCheckoutResponseOfficialContract: every official fact field is
// mandatory — wrong/missing values reject; the four official statuses
// pass; the retired "active" rejects.
func TestCheckoutResponseOfficialContract(t *testing.T) {
	pool := crTestPool(t)

	// happy path per status (direct object shape)
	for _, status := range []string{"pending", "processing", "completed", "expired"} {
		botID := crSeedBot(t, pool)
		fc := newFakeCreem(t, goodProduct())
		fc.checkoutBody = fmt.Sprintf(`{"id":"ch_x","request_id":"REPLACE","checkout_url":"https://checkout.fake.io/x","status":%q,"mode":"test","units":1,"product":"prod_test123"}`, status)
		fc.checkoutBody = strings.Replace(fc.checkoutBody, "REPLACE", "%s", 1)
		// rebuild per-payment: use a custom handler expectation via mutation below
		res := runCheckoutWithBody(t, pool, fc, botID, fc.checkoutBody)
		if res == nil {
			t.Fatalf("status %q must be accepted", status)
		}
	}

	// rejections (each mutates one official fact)
	rejects := []struct {
		name string
		body string
	}{
		{"retired status active", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"active","mode":"test","units":1,"product":"prod_test123"}`},
		{"wrong mode", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"prod","units":1,"product":"prod_test123"}`},
		{"missing mode", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","units":1,"product":"prod_test123"}`},
		{"wrong product", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1,"product":"prod_other"}`},
		{"missing product", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1}`},
		{"wrong units", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":2,"product":"prod_test123"}`},
		{"zero units", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":0,"product":"prod_test123"}`},
		{"missing units", `{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","product":"prod_test123"}`},
		{"wrong request_id", `{"id":"ch_x","request_id":"other","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1,"product":"prod_test123"}`},
		{"missing request_id", `{"id":"ch_x","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1,"product":"prod_test123"}`},
	}
	for _, tc := range rejects {
		botID := crSeedBot(t, pool)
		fc := newFakeCreem(t, goodProduct())
		if res := runCheckoutWithBody(t, pool, fc, botID, tc.body); res != nil {
			t.Fatalf("%s: must reject", tc.name)
		}
	}
}

// runCheckoutWithBody runs StartCreemCheckout with the fake returning the
// given body template (%s substituted with the actual payment code).
func runCheckoutWithBody(t *testing.T, pool *pg.Pool, fc *fakeCreem, botID int64, bodyTemplate string) *CheckoutResult {
	t.Helper()
	fc.mu.Lock()
	fc.checkoutStatus = 200
	fc.mu.Unlock()

	// wrap the fake's default body logic: intercept via a wrapped server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/products/") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fc.product)
			return
		}
		if r.URL.Path == "/v1/checkouts" {
			var in CreateCheckoutInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			fc.mu.Lock()
			fc.checkouts = append(fc.checkouts, in)
			fc.mu.Unlock()
			body := bodyTemplate
			if strings.Contains(body, "%s") {
				body = fmt.Sprintf(body, in.RequestID)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)

	rt := &CreemRuntime{
		Client:         NewCreemClient(CreemConfig{APIBase: srv.URL, APIKey: "k"}),
		ProductID:      fc.product.ID,
		CreditsPerUnit: 100,
		Mode:           "test",
		SuccessURL:     "https://kungfu.md/owner?payment=success",
	}
	res, err := StartCreemCheckout(context.Background(), pool, rt, botID, 1)
	if err != nil {
		return nil
	}
	return res
}

// TestCheckoutProductShapeVariants: the product identity normalizes from
// string / object / product_id forms.
func TestCheckoutProductShapeVariants(t *testing.T) {
	variants := []string{
		`{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1,"product":"prod_test123"}`,
		`{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1,"product":{"id":"prod_test123","name":"Kungfu Credits"}}`,
		`{"id":"ch_x","request_id":"%s","checkout_url":"https://c.io/x","status":"pending","mode":"test","units":1,"product_id":"prod_test123"}`,
	}
	for i, body := range variants {
		var co CreemCheckout
		filled := fmt.Sprintf(body, "code000000001")
		if err := json.Unmarshal([]byte(filled), &co); err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if co.ProductID != "prod_test123" {
			t.Fatalf("variant %d: ProductID = %q", i, co.ProductID)
		}
	}
}

// TestCompletedStatusOnCreateGrantsNothing: even when checkout creation
// returns status=completed, no grant happens on the HTTP path — only the
// webhook reconciles and grants.
func TestCompletedStatusOnCreateGrantsNothing(t *testing.T) {
	pool := crTestPool(t)
	botID := crSeedBot(t, pool)
	fc := newFakeCreem(t, goodProduct())

	res := runCheckoutWithBody(t, pool, fc, botID,
		`{"id":"ch_c","request_id":"%s","checkout_url":"https://c.io/x","status":"completed","mode":"test","units":1,"product":"prod_test123"}`)
	if res == nil {
		t.Fatal("completed-on-create should still return the checkout URL")
	}
	if n, _ := crGrants(t, pool, res.Payment.Code); n != 0 {
		t.Fatalf("grant on create-response: %d", n)
	}
	if b := crBalance(t, pool, botID); b != 0 {
		t.Fatalf("balance = %v, want 0", b)
	}
	p, _ := GetPayment(context.Background(), pool, res.Payment.Code)
	if p.Status != "pending" {
		t.Fatalf("status = %s, want pending until webhook", p.Status)
	}
}
