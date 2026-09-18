package server

// R2.2: strict bounded inbound I/O proofs. Oversized bodies whose
// first cap bytes are VALID JSON must fail closed — proving the old
// silent-truncation-then-parse scenario is gone. Boundary proofs are
// structural (exact cap accepted, cap+1 rejected), not magic-payload.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/config"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/ratelimit"
)

// -- boundary: exact cap accepted, cap+1 rejected (structural) --

func TestR22BoundedReaderBoundary(t *testing.T) {
	for _, cap := range []int64{1 << 16, 1 << 20} {
		exact := strings.Repeat(" ", int(cap))
		req := httptest.NewRequest("POST", "/x", strings.NewReader(exact))
		data, err := readBoundedRequestBody(req, cap)
		if err != nil || int64(len(data)) != cap {
			t.Fatalf("exact cap %d: err=%v len=%d", cap, err, len(data))
		}
		over := exact + "x"
		req = httptest.NewRequest("POST", "/x", strings.NewReader(over))
		if _, err := readBoundedRequestBody(req, cap); err == nil {
			t.Fatalf("cap+1 (%d) must be rejected", cap)
		}
	}
}

// buildOversizedJSON returns a body whose FIRST cap bytes are a valid
// JSON object (padded with legal trailing whitespace INSIDE the JSON
// text), followed by at least one extra byte — the exact scenario
// silent truncation used to accept.
func buildOversizedJSON(cap int, prefix string) string {
	pad := int(cap) - len(prefix) - 2 // trailing whitespace inside cap
	if pad < 0 {
		panic("prefix larger than cap")
	}
	body := prefix + strings.Repeat("\n", pad) // still valid JSON at exactly cap
	return body + `{"overflow":true}`          // bytes beyond the cap
}

// -- A. Store Redeem oversized: balance/redemption/ledger untouched --

func TestR22StoreRedeemOversizedFailClosed(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	router := s.buildRouter()

	// owner bot with balance + an active product
	suffix := fmt.Sprint(time.Now().UnixNano())
	w := httptest.NewRecorder()
	_, botID := seedR22Bot(t, pool, suffix)
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	cookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	prodCode := seedR22Product(t, pool, "R22 Oversize "+suffix, 5)

	var balanceBefore float64
	_ = pool.QueryRow(ctxBG2(), "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&balanceBefore)

	// first 1 MiB is a VALID redeem JSON (whitespace-padded), then extra
	body := buildOversizedJSON(1<<20,
		fmt.Sprintf(`{"product_code":%q,"request_key":"rk-oversize-%s"}`, prodCode, suffix))

	req := httptest.NewRequest("POST", "/api/owner/store/redemptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_JSON") {
		t.Fatalf("oversized redeem = %d %s, want 400 INVALID_JSON", rec.Code, rec.Body.String())
	}

	// zero mutation
	var balanceAfter float64
	var redemptions, spends int
	_ = pool.QueryRow(ctxBG2(), "SELECT balance FROM tb_bots WHERE id=$1", botID).Scan(&balanceAfter)
	_ = pool.QueryRow(ctxBG2(),
		"SELECT COUNT(*) FROM tb_redemptions WHERE bot_id=$1 AND request_key=$2",
		botID, "rk-oversize-"+suffix).Scan(&redemptions)
	_ = pool.QueryRow(ctxBG2(),
		"SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='spend_redemption'",
		botID).Scan(&spends)
	if balanceAfter != balanceBefore {
		t.Fatalf("balance changed: %v -> %v", balanceBefore, balanceAfter)
	}
	if redemptions != 0 || spends != 0 {
		t.Fatalf("mutations: redemptions=%d spends=%d", redemptions, spends)
	}
}

// -- B. Owner Checkout oversized: zero provider calls, zero payments --

func TestR22CheckoutOversizedFailClosed(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	// a fake provider that FAILS the test if ever hit
	providerHits := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits++
		w.WriteHeader(200)
	}))
	defer fake.Close()

	s := &Server{
		Config: testConfig(),
		Pool:   pool,
		RateLimiter: ratelimit.NewLimiter(
			map[string]ratelimit.Config{}),
	}
	s.Config.CreemAPIKey = "k"
	s.Config.CreemWebhookSecret = "s"
	s.Config.CreemMode = "test"
	s.Config.CreemSuccessURL = "https://example.com/x"
	s.Config.CreemPackages = map[string]config.CreemPackage{
		"starter": {Code: "starter", ProductID: "prod_r22", Credits: 10},
	}
	s.creemBaseOverride = fake.URL
	router := s.buildRouter()

	suffix := fmt.Sprint(time.Now().UnixNano())
	_, botID := seedR22Bot(t, pool, suffix)
	w := httptest.NewRecorder()
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	cookie := parseSetCookie(t, w.Header().Get("Set-Cookie"))

	var paymentsBefore int
	_ = pool.QueryRow(ctxBG2(),
		"SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1", botID).Scan(&paymentsBefore)

	// first 64 KiB valid checkout JSON, then extra bytes
	body := buildOversizedJSON(1<<16, `{"package":"starter"}`)

	req := httptest.NewRequest("POST", "/api/owner/payments/checkout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_JSON") {
		t.Fatalf("oversized checkout = %d %s, want 400 INVALID_JSON", rec.Code, rec.Body.String())
	}
	if providerHits != 0 {
		t.Fatalf("provider was called %d times on an oversized request", providerHits)
	}
	var paymentsAfter int
	_ = pool.QueryRow(ctxBG2(),
		"SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1", botID).Scan(&paymentsAfter)
	if paymentsAfter != paymentsBefore {
		t.Fatalf("payment rows changed: %d -> %d", paymentsBefore, paymentsAfter)
	}
}

// -- C. Webhook oversized: INVALID_BODY BEFORE signature verification --

func TestR22WebhookOversizedFailsBeforeSignature(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)

	s := &Server{
		Config:      testConfig(),
		Pool:        pool,
		RateLimiter: ratelimit.NewLimiter(map[string]ratelimit.Config{}),
	}
	s.Config.CreemWebhookSecret = "whsec"
	router := s.buildRouter()

	// >1 MiB raw body with a VALID signature would still be rejected
	// on body size FIRST — so send it with NO signature header: if the
	// handler answered 401 INVALID_SIGNATURE it read the body fine and
	// skipped our gate; it must answer 400 INVALID_BODY instead.
	inner := buildOversizedJSON(1<<20, `{"id":"evt_1","event_type":"checkout.completed"}`)

	var paymentsBefore, adjustmentsBefore int
	_ = pool.QueryRow(ctxBG2(), "SELECT COUNT(*) FROM tb_payments").Scan(&paymentsBefore)
	_ = pool.QueryRow(ctxBG2(), "SELECT COUNT(*) FROM tb_payment_adjustments").Scan(&adjustmentsBefore)

	req := httptest.NewRequest("POST", "/api/webhooks/creem", strings.NewReader(inner))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "INVALID_BODY") {
		t.Fatalf("oversized webhook = %d %s, want 400 INVALID_BODY (before signature)",
			rec.Code, rec.Body.String())
	}
	var paymentsAfter, adjustmentsAfter int
	_ = pool.QueryRow(ctxBG2(), "SELECT COUNT(*) FROM tb_payments").Scan(&paymentsAfter)
	_ = pool.QueryRow(ctxBG2(), "SELECT COUNT(*) FROM tb_payment_adjustments").Scan(&adjustmentsAfter)
	if paymentsAfter != paymentsBefore || adjustmentsAfter != adjustmentsBefore {
		t.Fatal("webhook mutation happened on oversized body")
	}
}

// -- helpers --

func ctxBG2() context.Context { return context.Background() }

func seedR22Bot(t *testing.T, pool *pg.Pool, suffix string) (struct{}, int64) {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctxBG2(), `
		INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		VALUES ($1, $2, 'x', 'active', 20) RETURNING id`,
		"r22bot_"+suffix, "kf_live_"+suffix+strings.Repeat("a", 64-len(suffix))).Scan(&id); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctxBG2(), "DELETE FROM tb_transactions WHERE bot_id=$1", id)
		_, _ = pool.Exec(ctxBG2(), "DELETE FROM tb_redemptions WHERE bot_id=$1", id)
		_, _ = pool.Exec(ctxBG2(), "DELETE FROM tb_payments WHERE bot_id=$1", id)
		_, _ = pool.Exec(ctxBG2(), "DELETE FROM tb_bots WHERE id=$1", id)
	})
	return struct{}{}, id
}

func seedR22Product(t *testing.T, pool *pg.Pool, title string, price float64) string {
	t.Helper()
	var code string
	if err := pool.QueryRow(ctxBG2(), `
		INSERT INTO tb_store_products (code, title, credits_price, status)
		VALUES (substr(md5(random()::text),1,12), $1, $2, 'active') RETURNING code`,
		title, price).Scan(&code); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctxBG2(), "DELETE FROM tb_redemptions WHERE product_id=(SELECT id FROM tb_store_products WHERE code=$1)", code)
		_, _ = pool.Exec(ctxBG2(), "DELETE FROM tb_store_products WHERE code=$1", code)
	})
	return code
}
