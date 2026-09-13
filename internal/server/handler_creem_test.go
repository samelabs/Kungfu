package server

// Creem HTTP endpoint tests: fake Creem server injected via test server
// config, real router, real PostgreSQL. Covers auth gates, signature
// 401s, DTO hygiene, refund/dispute zero-mutation, and disabled config.

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
)

const crTestWebhookSecret = "whsec_http_test"

// creemTestServer builds a server with Creem fully configured against a
// fake provider.
func creemTestServer(t *testing.T, fakeURL string) *Server {
	t.Helper()
	s := storeTestServer(t)
	s.Config.CreemAPIKey = "test-api-key"
	s.Config.CreemWebhookSecret = crTestWebhookSecret
	s.Config.CreemProductID = "prod_test123"
	s.Config.CreemCreditsPerUnit = 100
	s.Config.CreemMode = "test"
	s.Config.CreemSuccessURL = "https://kungfu.md/owner?payment=success"
	// Point the client at the fake server by overriding the derived base.
	s.Config.CreemMode = "test"
	s.creemBaseOverride = fakeURL
	return s
}

func creemOwnerCookie(t *testing.T, s *Server, botID int64) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	return parseSetCookie(t, w.Header().Get("Set-Cookie"))
}

func creemSeedBot(t *testing.T, s *Server) int64 {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var botID int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		 VALUES ($1, $2, 'x', 'active', 0) RETURNING id`,
		"crhttp_"+suffix, "kf_live_"+suffix).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_payments WHERE bot_id=$1`, botID)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, botID)
	})
	return botID
}

func signCreem(raw []byte) string {
	mac := hmac.New(sha256.New, []byte(crTestWebhookSecret))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// -- 1. disabled config → PAYMENT_NOT_CONFIGURED --

func TestCheckoutDisabledReturns503(t *testing.T) {
	s := storeTestServer(t) // no Creem config
	router := s.buildRouter()
	botID := creemSeedBot(t, s)
	cookie := creemOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"units":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PAYMENT_NOT_CONFIGURED") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// -- checkout requires owner session --

func TestCheckoutRequiresOwnerAuth(t *testing.T) {
	s := creemTestServer(t, "http://127.0.0.1:1")
	router := s.buildRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout",
		bytes.NewBufferString(`{"units":1}`))
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("unauthenticated checkout returned 200")
	}
}

// -- successful checkout DTO hygiene --

func TestCheckoutResponseDTOHygiene(t *testing.T) {
	fake := newFakeCreemHTTP(t)
	s := creemTestServer(t, fake.URL)
	router := s.buildRouter()
	botID := creemSeedBot(t, s)
	cookie := creemOwnerCookie(t, s, botID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout",
		bytes.NewBufferString(`{"units":2}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, banned := range []string{"bot_id", "api_key", "webhook", "\"id\""} {
		if strings.Contains(body, banned) {
			t.Fatalf("DTO leaks %q: %s", banned, body)
		}
	}
	if !strings.Contains(body, "checkout_url") {
		t.Fatal("checkout_url missing")
	}
}

// -- webhook signature gates --

func TestWebhookMissingOrBadSignature401(t *testing.T) {
	fake := newFakeCreemHTTP(t)
	s := creemTestServer(t, fake.URL)
	router := s.buildRouter()

	payload := []byte(`{"id":"evt_x","eventType":"checkout.completed","object":{}}`)

	// missing signature
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig status = %d", rec.Code)
	}

	// bad signature
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	req2.Header.Set("creem-signature", strings.Repeat("ab", 32))
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig status = %d", rec2.Code)
	}

	// zero mutation on any bot seeded by this test (global counts can race
	// with parallel packages sharing the test database).
	botID := creemSeedBot(t, s)
	var n int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id = $1`, botID).Scan(&n)
	if n != 0 {
		t.Fatalf("payments mutated by unauthenticated webhook: %d", n)
	}
	var txN int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1`, botID).Scan(&txN)
	if txN != 0 {
		t.Fatalf("transactions mutated by unauthenticated webhook: %d", txN)
	}
}

// -- refund.created / dispute.created → 200, zero mutation --

func TestWebhookRefundDisputeZeroMutation(t *testing.T) {
	fake := newFakeCreemHTTP(t)
	s := creemTestServer(t, fake.URL)
	router := s.buildRouter()
	botID := creemSeedBot(t, s)

	for _, eventType := range []string{"refund.created", "dispute.created"} {
		payload, _ := json.Marshal(map[string]interface{}{
			"id": "evt_" + eventType, "eventType": eventType, "created_at": time.Now().Unix(),
			"object": map[string]interface{}{"id": "ord_1"},
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
		req.Header.Set("creem-signature", signCreem(payload))
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", eventType, rec.Code)
		}
	}

	// no transactions, no payments, no balance change
	var txN int
	_ = s.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1`, botID).Scan(&txN)
	if txN != 0 {
		t.Fatalf("refund/dispute mutated transactions: %d", txN)
	}
}

// -- unknown event types acknowledged-ignored --

func TestWebhookUnknownEventIgnored(t *testing.T) {
	fake := newFakeCreemHTTP(t)
	s := creemTestServer(t, fake.URL)
	router := s.buildRouter()

	payload, _ := json.Marshal(map[string]interface{}{
		"id": "evt_sub", "eventType": "subscription.created", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	req.Header.Set("creem-signature", signCreem(payload))
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// -- owner payment GET ownership --

func TestOwnerPaymentGetOwnership(t *testing.T) {
	fake := newFakeCreemHTTP(t)
	s := creemTestServer(t, fake.URL)
	router := s.buildRouter()
	botA := creemSeedBot(t, s)
	botB := creemSeedBot(t, s)
	cookieA := creemOwnerCookie(t, s, botA)
	cookieB := creemOwnerCookie(t, s, botB)

	// create a payment for A via checkout
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout",
		bytes.NewBufferString(`{"units":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookieA)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			Payment struct{ Code string } `json:"payment"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	code := created.Data.Payment.Code

	// own read
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+code, nil)
	req2.AddCookie(cookieA)
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("own payment get = %d", rec2.Code)
	}
	if strings.Contains(rec2.Body.String(), "bot_id") {
		t.Fatalf("payment DTO leaks bot_id: %s", rec2.Body.String())
	}

	// cross-bot read
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+code, nil)
	req3.AddCookie(cookieB)
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("cross-bot payment get = %d, want 404", rec3.Code)
	}
}

// -- e2e: checkout → webhook → paid, over the real router --

func TestCreemE2EWebhookGrants(t *testing.T) {
	fake := newFakeCreemHTTP(t)
	s := creemTestServer(t, fake.URL)
	router := s.buildRouter()
	botID := creemSeedBot(t, s)
	cookie := creemOwnerCookie(t, s, botID)

	// checkout
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/checkout",
		bytes.NewBufferString(`{"units":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			Payment struct {
				Code        string  `json:"code"`
				AmountMinor int64   `json:"amount_minor"`
				Currency    string  `json:"currency"`
				Credits     float64 `json:"credits"`
			} `json:"payment"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	pc := created.Data.Payment
	if pc.AmountMinor != 1000 || pc.Credits != 100 || pc.Currency != "USD" {
		t.Fatalf("payment facts = %+v", pc)
	}

	// signed completion webhook
	payload, _ := json.Marshal(map[string]interface{}{
		"id": "evt_e2e", "eventType": "checkout.completed", "created_at": time.Now().Unix(),
		"object": map[string]interface{}{
			"id": "ch_e2e", "request_id": pc.Code, "status": "completed",
			"order_id": "ord_e2e", "mode": "test",
			"metadata": map[string]string{"payment_code": pc.Code, "bot_id": fmt.Sprintf("%d", botID), "source": "kungfu_owner"},
			"order": map[string]interface{}{
				"id": "ord_e2e", "status": "paid", "product": "prod_test123",
				"currency": "USD", "amount": 1000, "units": 1,
			},
		},
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/creem", bytes.NewReader(payload))
	req2.Header.Set("creem-signature", signCreem(payload))
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("webhook = %d %s", rec2.Code, rec2.Body.String())
	}

	// payment status via owner API = paid
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+pc.Code, nil)
	req3.AddCookie(cookie)
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK || !strings.Contains(rec3.Body.String(), `"paid"`) {
		t.Fatalf("payment get = %d %s", rec3.Code, rec3.Body.String())
	}

	var bal float64
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&bal)
	if bal != 100 {
		t.Fatalf("balance = %v, want 100", bal)
	}
}

// newFakeCreemHTTP mirrors payment.goodProduct for the server tests.
func newFakeCreemHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/products", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"product": map[string]interface{}{
				"id": "prod_test123", "name": "Kungfu Credits", "billing_type": "onetime",
				"status": "active", "mode": "test", "currency": "USD", "price": 1000,
			},
		})
	})
	mux.HandleFunc("/v1/checkouts", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RequestID string `json:"request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"checkout": map[string]interface{}{
				"id": "ch_http_" + in.RequestID, "request_id": in.RequestID,
				"checkout_url": "https://checkout.fake.io/" + in.RequestID,
				"status":       "pending", "mode": "test",
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
