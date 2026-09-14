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
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1,$2,'x','active',0) RETURNING id`,
		"cfphttp_"+suffix, "kf_live_"+suffix).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_payments WHERE bot_id=$1`, botID)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, botID)
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

// refund/dispute → 200, zero mutation
func TestWebhookRefundDisputeFrozen(t *testing.T) {
	s := cfpServer(t, newCfpFake(t).URL)
	router := s.buildRouter()
	botID := cfpBot(t, s)

	for _, et := range []string{"refund.created", "dispute.created"} {
		payload, _ := json.Marshal(map[string]interface{}{
			"id": "evt_" + et, "eventType": et, "created_at": time.Now().Unix(),
			"object": map[string]interface{}{"id": "ord_1"},
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
		req.Header.Set("creem-signature", cfpSign(payload))
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", et, rec.Code)
		}
	}
	var txN int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&txN)
	if txN != 0 {
		t.Fatalf("refund/dispute mutated credits: %d", txN)
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
