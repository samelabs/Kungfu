package server

// Creem fixed-package HTTP endpoint tests: fake provider + real router +
// real PostgreSQL. Covers disabled config, auth gates, client-field
// tamper immunity, DTO hygiene, refund/dispute freeze, and e2e grant.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/config"
)

const cfpWebhookSecret = "whsec_http_cfp"

// newCfpFake builds a fake Creem implementing the official endpoints for
// two packages: prod_a (starter, 1000 minor) and prod_b (standard, 4000).
func newCfpFake(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/products/", func(w http.ResponseWriter, r *http.Request) {
		var prod string
		switch strings.TrimPrefix(r.URL.Path, "/v1/products/") {
		case "prod_a":
			prod = `{"id":"prod_a","billing_type":"onetime","status":"active","mode":"test","currency":"USD","price":1000}`
		case "prod_b":
			prod = `{"id":"prod_b","billing_type":"onetime","status":"active","mode":"test","currency":"USD","price":4000}`
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(prod))
	})
	mux.HandleFunc("/v1/checkouts", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ProductID string `json:"product_id"`
			RequestID string `json:"request_id"`
			Units     int64  `json:"units"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"id":"ch_%s","request_id":%q,"checkout_url":"https://checkout.fake.io/%s","status":"pending","mode":"test","units":%d,"product":%q}`,
			in.RequestID, in.RequestID, in.RequestID, in.Units, in.ProductID)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func cfpServer(t *testing.T, fakeURL string) *Server {
	t.Helper()
	s := storeTestServer(t)
	s.Config.CreemAPIKey = "k"
	s.Config.CreemWebhookSecret = cfpWebhookSecret
	s.Config.CreemMode = "test"
	s.Config.CreemSuccessURL = "https://kungfu.md/owner?payment=success"
	s.Config.CreemPackages = map[string]config.CreemPackage{
		"starter":  {Code: "starter", ProductID: "prod_a", Credits: 1000},
		"standard": {Code: "standard", ProductID: "prod_b", Credits: 5000},
	}
	s.creemBaseOverride = fakeURL
	return s
}

func cfpBot(t *testing.T, s *Server) int64 {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var botID int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', 0) RETURNING id`,
		"cfphttp_"+suffix, s61SeedKeyHash("kf_live_"+suffix), s61SeedLast4("kf_live_"+suffix)).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// FK-aware order: adjustment facts → payments → bot. Errors surfaced.
		ctx := context.Background()
		if _, err := s.Pool.Exec(ctx, `DELETE FROM tb_payment_adjustments WHERE payment_id IN (SELECT id FROM tb_payments WHERE bot_id = $1)`, botID); err != nil {
			t.Errorf("cleanup tb_payment_adjustments(bot=%d): %v", botID, err)
		}
		if _, err := s.Pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id = $1`, botID); err != nil {
			t.Errorf("cleanup tb_transactions(bot=%d): %v", botID, err)
		}
		if _, err := s.Pool.Exec(ctx, `DELETE FROM tb_payments WHERE bot_id = $1`, botID); err != nil {
			t.Errorf("cleanup tb_payments(bot=%d): %v", botID, err)
		}
		if _, err := s.Pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, botID); err != nil {
			t.Errorf("cleanup tb_bots(%d): %v", botID, err)
		}
	})
	return botID
}

func cfpSign(raw []byte) string {
	mac := hmac.New(sha256.New, []byte(cfpWebhookSecret))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// disabled → 503 PAYMENT_NOT_CONFIGURED
func TestCheckoutDisabled503(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID := cfpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(`{"package":"starter"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "PAYMENT_NOT_CONFIGURED") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// requires owner session
func TestCheckoutRequiresAuth(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(`{"package":"starter"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("unauthenticated checkout returned 200")
	}
}

// THE tamper proof: hostile client fields alongside a valid package code
// cannot alter any fact — the struct has no fields for them.
func TestCheckoutClientTamperImmunity(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	botID := cfpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	// every hostile field the work order names
	hostile := `{"package":"starter","units":999,"amount_minor":1,"credits":999999,"product_id":"evil","custom_price":1,"success_url":"https://evil.example/steal","metadata":{"credits":999999}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(hostile))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	// facts follow the SERVER package: prod_a, live price 1000, 1000 credits
	var out struct {
		Data struct {
			Payment struct {
				AmountMinor int64   `json:"amount_minor"`
				Credits     float64 `json:"credits"`
				Currency    string  `json:"currency"`
				Code        string  `json:"code"`
			} `json:"payment"`
			CheckoutURL string `json:"checkout_url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Data.Payment.AmountMinor != 1000 || out.Data.Payment.Credits != 1000 || out.Data.Payment.Currency != "USD" {
		t.Fatalf("tampered facts: %+v", out.Data.Payment)
	}
	if !strings.HasPrefix(out.Data.CheckoutURL, "https://checkout.fake.io/") {
		t.Fatalf("checkout_url = %s", out.Data.CheckoutURL)
	}

	// DB row carries the server snapshot, not client input
	var prodID string
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT provider_product_id FROM tb_payments WHERE code=$1`, out.Data.Payment.Code).Scan(&prodID); err != nil {
		t.Fatal(err)
	}
	if prodID != "prod_a" {
		t.Fatalf("provider_product_id = %s, client product_id leaked", prodID)
	}

	// DTO hygiene
	body := rec.Body.String()
	for _, banned := range []string{"bot_id", "api_key", "webhook"} {
		if strings.Contains(body, banned) {
			t.Fatalf("DTO leaks %q", banned)
		}
	}
}

// unknown package → 400 INVALID_PACKAGE
func TestCheckoutUnknownPackage(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	botID := cfpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(`{"package":"enterprise"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_PACKAGE") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// webhook signature gates over the real router
func TestWebhookBadSignature401(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()

	payload := []byte(`{"id":"e","eventType":"checkout.completed","object":{}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	req.Header.Set("creem-signature", strings.Repeat("ab", 32))
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}

	botID := cfpBot(t, s)
	var n int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1`, botID).Scan(&n)
	if n != 0 {
		t.Fatalf("mutation by bad signature: %d", n)
	}
}

// refund/dispute over the real router: full flow — paid payment first,
// then a signed refund.created → durable adjustment fact, ZERO Credits
// mutation; mismatched facts (unknown order) → 400 RECONCILIATION_FAILED.
func TestWebhookRefundDisputeFrozen(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	botID := cfpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	// paid payment via real checkout + webhook
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(`{"package":"starter"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Payment struct {
				Code string `json:"code"`
			} `json:"payment"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pc := out.Data.Payment.Code

	// complete the payment via signed webhook (the only grant path)
	completionPayload, _ := json.Marshal(map[string]interface{}{
		"id": fmt.Sprintf("evt_adj_complete_%d", time.Now().UnixNano()), "eventType": "checkout.completed", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{
			"id": "ch_" + pc, "request_id": pc, "status": "completed",
			"order_id": "ord_" + pc, "mode": "test",
			"metadata": map[string]string{"payment_code": pc, "bot_id": fmt.Sprintf("%d", botID), "source": "kungfu_owner"},
			"order": map[string]interface{}{
				"id": "ord_" + pc, "status": "paid", "product": "prod_a",
				"currency": "USD", "amount": 1000, "units": 1,
			},
		},
	})
	recC := httptest.NewRecorder()
	reqC := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(completionPayload))
	reqC.Header.Set("creem-signature", cfpSign(completionPayload))
	router.ServeHTTP(recC, reqC)
	if recC.Code != http.StatusOK {
		t.Fatalf("completion = %d %s", recC.Code, recC.Body.String())
	}

	var balBefore float64
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balBefore)
	if balBefore != 1000 {
		t.Fatalf("seed balance = %v, want 1000", balBefore)
	}

	// signed refund.created matching the payment snapshot (amount_paid
	// deliberately 1080 > 1000: tax must be accepted)
	refunded := 540
	payload, _ := json.Marshal(map[string]interface{}{
		"id": fmt.Sprintf("evt_http_refund_%d", time.Now().UnixNano()), "eventType": "refund.created", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{
			"id": fmt.Sprintf("ref_http_%d", time.Now().UnixNano()), "status": "succeeded",
			"refund_amount": 540, "refund_currency": "USD", "reason": "customer request",
			"transaction": map[string]interface{}{
				"id": fmt.Sprintf("txn_http_%d", time.Now().UnixNano()), "amount": 1000, "amount_paid": 1080, "currency": "USD",
				"status": "succeeded", "refunded_amount": refunded, "order": "ord_" + pc,
			},
		},
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	req2.Header.Set("creem-signature", cfpSign(payload))
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("refund status = %d body = %s", rec2.Code, rec2.Body.String())
	}

	// durable fact exists; credits untouched
	var adjN int
	_ = s.Pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id
		WHERE p.code=$1 AND a.kind='refund'`, pc).Scan(&adjN)
	if adjN != 1 {
		t.Fatalf("adjustment rows = %d, want 1", adjN)
	}
	// A2: the 540/1080 cumulative refund authorizes reversal of
	// round4(1000*540/1080)=500 — exactly one reverse_payment row.
	var balAfter float64
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&balAfter)
	if balAfter != 500 {
		t.Fatalf("balance = %v, want 500 (grant 1000 − reversal 500)", balAfter)
	}
	var txN int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&txN)
	if txN != 2 {
		t.Fatalf("tb_transactions rows = %d, want 2 (grant + reverse)", txN)
	}

	// missing signature on a refund event → 401, zero adjustment rows
	noSigPayload, _ := json.Marshal(map[string]interface{}{
		"id": fmt.Sprintf("evt_nosig_%d", time.Now().UnixNano()), "eventType": "refund.created", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{"id": "ref_nosig"},
	})
	recN := httptest.NewRecorder()
	reqN := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(noSigPayload))
	router.ServeHTTP(recN, reqN)
	if recN.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature = %d, want 401", recN.Code)
	}

	// invalid signature → 401, zero adjustment rows
	recI := httptest.NewRecorder()
	reqI := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(noSigPayload))
	reqI.Header.Set("creem-signature", strings.Repeat("ab", 32))
	router.ServeHTTP(recI, reqI)
	if recI.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature = %d, want 401", recI.Code)
	}
	var sigAdjN int
	_ = s.Pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id
		WHERE p.bot_id=$1`, botID).Scan(&sigAdjN)
	if sigAdjN != 1 {
		t.Fatalf("signature-gated events leaked adjustment rows: %d", sigAdjN)
	}

	// mismatched fact → 400, no new rows
	badPayload, _ := json.Marshal(map[string]interface{}{
		"id": fmt.Sprintf("evt_http_bad_%d", time.Now().UnixNano()), "eventType": "dispute.created", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{"id": "dis_bad"},
	})
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(badPayload))
	req3.Header.Set("creem-signature", cfpSign(badPayload))
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusBadRequest || !strings.Contains(rec3.Body.String(), "RECONCILIATION_FAILED") {
		t.Fatalf("mismatched fact = %d %s", rec3.Code, rec3.Body.String())
	}
	var adjN2 int
	_ = s.Pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM tb_payment_adjustments a JOIN tb_payments p ON p.id=a.payment_id
		WHERE p.code=$1`, pc).Scan(&adjN2)
	if adjN2 != 1 {
		t.Fatalf("mismatched fact leaked rows: %d", adjN2)
	}
}

// e2e: checkout → signed webhook → paid → balance, over the real router.
func TestCreemE2EPackageFlow(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	botID := cfpBot(t, s)
	cookie := storeOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(`{"package":"standard"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Payment struct {
				Code        string  `json:"code"`
				AmountMinor int64   `json:"amount_minor"`
				Credits     float64 `json:"credits"`
			} `json:"payment"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pc := out.Data.Payment
	if pc.AmountMinor != 4000 || pc.Credits != 5000 {
		t.Fatalf("standard facts = %+v", pc)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"id": "evt_e2e_cfp", "eventType": "checkout.completed", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{
			"id": "ch_" + pc.Code, "request_id": pc.Code, "status": "completed",
			"order_id": "ord_e2e_cfp", "mode": "test",
			"metadata": map[string]string{"payment_code": pc.Code, "bot_id": fmt.Sprintf("%d", botID), "source": "kungfu_owner"},
			"order": map[string]interface{}{
				"id": "ord_e2e_cfp", "status": "paid", "product": "prod_b",
				"currency": "USD", "amount": 4000, "units": 1,
			},
		},
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	req2.Header.Set("creem-signature", cfpSign(payload))
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("webhook = %d %s", rec2.Code, rec2.Body.String())
	}

	var bal float64
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&bal)
	if bal != 5000 {
		t.Fatalf("balance = %v, want 5000", bal)
	}

	// owner GET sees paid
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+pc.Code, nil)
	req3.AddCookie(cookie)
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK || !strings.Contains(rec3.Body.String(), `"paid"`) {
		t.Fatalf("payment get = %d %s", rec3.Code, rec3.Body.String())
	}
}

// owner GET ownership: cross-bot 404
func TestOwnerPaymentGetCrossBot404(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	botA, botB := cfpBot(t, s), cfpBot(t, s)
	cookieA, cookieB := storeOwnerCookie(t, s, botA), storeOwnerCookie(t, s, botB)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", bytes.NewBufferString(`{"package":"starter"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookieA)
	router.ServeHTTP(rec, req)
	var out struct {
		Data struct {
			Payment struct{ Code string } `json:"payment"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+out.Data.Payment.Code, nil)
	req2.AddCookie(cookieB)
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("cross-bot = %d, want 404", rec2.Code)
	}
}
