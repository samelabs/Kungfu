package server

// Owner Store API integration tests (real PostgreSQL + real router).
// Covers: auth gate, active-only catalog, redeem + atomic spend, snapshot,
// request_key idempotency (serial + concurrent), conflicts, ownership
// scoping, and DTO field hygiene (no internal ids).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/store"
)

// -- helpers --

func storeTestServer(t *testing.T) *Server {
	t.Helper()
	s, _ := seededTestServer(t)
	return s
}

func storeOwnerCookie(t *testing.T, s *Server, botID int64) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	setOwnerCookie(w, botID, s.Config.SessionSecret, false)
	return parseSetCookie(t, w.Header().Get("Set-Cookie"))
}

func storeSeedProduct(t *testing.T, s *Server, price float64) string {
	t.Helper()
	p, err := store.CreateProduct(context.Background(), s.Pool, store.ProductInput{
		Title:        fmt.Sprintf("SE Product %d", time.Now().UnixNano()),
		CreditsPrice: price,
	})
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(),
			`DELETE FROM tb_redemptions WHERE product_id = (SELECT id FROM tb_store_products WHERE code = $1)`, p.Code)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_store_products WHERE code = $1`, p.Code)
	})
	return p.Code
}

func storeDo(t *testing.T, router http.Handler, cookie *http.Cookie, method, path string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var parsed map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func storeBotBalance(t *testing.T, s *Server, botID int64) float64 {
	t.Helper()
	var b float64
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id = $1`, botID).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func storeSpendCount(t *testing.T, s *Server, botID int64, refID string) (int, float64) {
	t.Helper()
	var n int
	var sum float64
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM tb_transactions
		 WHERE bot_id = $1 AND type = 'spend_redemption' AND ref_id = $2`, botID, refID).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

// -- 1. unauthenticated -> owner auth error --

func TestStoreProductsRequiresOwnerAuth(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	rec, body := storeDo(t, router, nil, http.MethodGet, "/api/owner/store/products", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated request returned 200: %v", body)
	}
	if body["success"] == true {
		t.Fatal("unauthenticated request must not succeed")
	}
}

// -- 2. products list active only --

func TestStoreProductsActiveOnly(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	_, cookie := seedStoreBot(t, s, 10)

	active := storeSeedProduct(t, s, 40)
	inactive := storeSeedProduct(t, s, 50)
	if ok, err := store.SetProductInactive(context.Background(), s.Pool, inactive); err != nil || !ok {
		t.Fatalf("deactivate: %v %v", ok, err)
	}

	rec, body := storeDo(t, router, cookie, http.MethodGet, "/api/owner/store/products", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %v", rec.Code, body)
	}
	data := body["data"].(map[string]interface{})
	items := data["products"].([]interface{})
	sawActive, sawInactive := false, false
	for _, it := range items {
		m := it.(map[string]interface{})
		if m["code"] == active {
			sawActive = true
		}
		if m["code"] == inactive {
			sawInactive = true
		}
		// DTO hygiene on every row.
		for _, banned := range []string{"id", "bot_id"} {
			if _, has := m[banned]; has {
				t.Fatalf("product DTO leaks %q", banned)
			}
		}
	}
	if !sawActive {
		t.Fatal("active product missing from catalog")
	}
	if sawInactive {
		t.Fatal("inactive product visible in catalog")
	}
}

// seedStoreBot creates an independent bot (balance-controlled) + cookie.
func seedStoreBot(t *testing.T, s *Server, balance float64) (int64, *http.Cookie) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var botID int64
	if err := s.Pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"sestore_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
	).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(),
			`DELETE FROM tb_redemptions WHERE bot_id = $1`, botID)
		_, _ = s.Pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID, storeOwnerCookie(t, s, botID)
}

// -- 3/4/5. redeem -> pending_review, balance, single ledger row --

func TestStoreRedeemCreatesPendingAndSpends(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID, cookie := seedStoreBot(t, s, 100)
	product := storeSeedProduct(t, s, 40)

	rec, body := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": product, "request_key": "se_redeem_1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %v", rec.Code, body)
	}
	data := body["data"].(map[string]interface{})
	r := data["redemption"].(map[string]interface{})
	if r["status"] != "pending_review" {
		t.Fatalf("status = %v", r["status"])
	}
	if created, ok := r["created"].(bool); !ok || !created {
		t.Fatalf("created = %v", r["created"])
	}
	if r["credits_cost"] != float64(40) {
		t.Fatalf("credits_cost = %v", r["credits_cost"])
	}
	if got := storeBotBalance(t, s, botID); got != 60 {
		t.Fatalf("balance = %v, want 60", got)
	}
	code := r["code"].(string)
	n, sum := storeSpendCount(t, s, botID, code)
	if n != 1 || sum != -40 {
		t.Fatalf("spend rows = %d/%v, want 1/-40", n, sum)
	}

	// 6. snapshot fields present
	if _, has := r["product_title"]; !has {
		t.Fatal("snapshot product_title missing")
	}
}

// -- 7. same key + same product -> replay, no second spend --

func TestStoreRedeemIdempotentReplay(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID, cookie := seedStoreBot(t, s, 100)
	product := storeSeedProduct(t, s, 40)

	_, b1 := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": product, "request_key": "se_replay_1"})
	code := b1["data"].(map[string]interface{})["redemption"].(map[string]interface{})["code"].(string)

	rec, b2 := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": product, "request_key": "se_replay_1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d", rec.Code)
	}
	r2 := b2["data"].(map[string]interface{})["redemption"].(map[string]interface{})
	if created, ok := r2["created"].(bool); !ok || created {
		t.Fatalf("replay created = %v", r2["created"])
	}
	if r2["code"] != code {
		t.Fatalf("replay returned different redemption: %v vs %v", r2["code"], code)
	}
	if got := storeBotBalance(t, s, botID); got != 60 {
		t.Fatalf("balance after replay = %v, want 60", got)
	}
	if n, _ := storeSpendCount(t, s, botID, code); n != 1 {
		t.Fatalf("spend rows after replay = %d", n)
	}
}

// -- 8. concurrent same key -> one spend --

func TestStoreRedeemConcurrentSameKey(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID, cookie := seedStoreBot(t, s, 100)
	product := storeSeedProduct(t, s, 40)

	const workers = 6
	var wg sync.WaitGroup
	codes := make([]string, workers)
	codesMu := sync.Mutex{}
	var distinct []string
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, body := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
				map[string]string{"product_code": product, "request_key": "se_conc_1"})
			if c, ok := body["data"].(map[string]interface{})["redemption"].(map[string]interface{})["code"].(string); ok {
				codesMu.Lock()
				codes[i] = c
				distinct = append(distinct, c)
				codesMu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, c := range distinct {
		seen[c] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent redeem produced %d distinct redemptions", len(seen))
	}
	if got := storeBotBalance(t, s, botID); got != 60 {
		t.Fatalf("balance after concurrent redeem = %v, want 60", got)
	}
	var n int
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1 AND type = 'spend_redemption'`, botID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("spend rows = %d, want 1", n)
	}
	_ = codes
}

// -- 9. same key different product -> 409 --

func TestStoreRedeemSameKeyDifferentProduct(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID, cookie := seedStoreBot(t, s, 100)
	p1 := storeSeedProduct(t, s, 40)
	p2 := storeSeedProduct(t, s, 50)

	storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": p1, "request_key": "se_conflict_1"})
	rec, body := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": p2, "request_key": "se_conflict_1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %v", rec.Code, body)
	}
	if got := storeBotBalance(t, s, botID); got != 60 {
		t.Fatalf("balance = %v, want 70 (no second debit)", got)
	}
}

// -- 10. insufficient credits -> no redemption, no debit --

func TestStoreRedeemInsufficient(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID, cookie := seedStoreBot(t, s, 5)
	product := storeSeedProduct(t, s, 40)

	rec, _ := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": product, "request_key": "se_insuf_1"})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	var n int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = 'se_insuf_1'`, botID).Scan(&n)
	if n != 0 {
		t.Fatalf("redemption leaked: %d", n)
	}
	if got := storeBotBalance(t, s, botID); got != 5 {
		t.Fatalf("balance = %v", got)
	}
}

// -- 11. inactive product -> no redemption, no debit --

func TestStoreRedeemInactive(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botID, cookie := seedStoreBot(t, s, 100)
	product := storeSeedProduct(t, s, 40)
	if ok, _ := store.SetProductInactive(context.Background(), s.Pool, product); !ok {
		t.Fatal("deactivate failed")
	}

	rec, _ := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": product, "request_key": "se_inactive_1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var n int
	_ = s.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_redemptions WHERE bot_id = $1 AND request_key = 'se_inactive_1'`, botID).Scan(&n)
	if n != 0 {
		t.Fatalf("redemption leaked: %d", n)
	}
	if got := storeBotBalance(t, s, botID); got != 100 {
		t.Fatalf("balance = %v", got)
	}
}

// -- 12/13/14. ownership-scoped get --

func TestStoreRedemptionGetOwnership(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	botA, cookieA := seedStoreBot(t, s, 100)
	_, cookieB := seedStoreBot(t, s, 100)
	product := storeSeedProduct(t, s, 40)

	_, b1 := storeDo(t, router, cookieA, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": product, "request_key": "se_get_1"})
	r := b1["data"].(map[string]interface{})["redemption"].(map[string]interface{})
	code := r["code"].(string)

	// own -> 200
	rec, body := storeDo(t, router, cookieA, http.MethodGet, "/api/owner/store/redemptions/"+code, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("own get status = %d", rec.Code)
	}
	got := body["data"].(map[string]interface{})["redemption"].(map[string]interface{})
	if got["code"] != code || got["status"] != "pending_review" {
		t.Fatalf("own get = %v", got)
	}
	// 15. DTO hygiene: no internal ids anywhere in the payload
	for _, banned := range []string{"id", "bot_id", "product_id"} {
		if _, has := got[banned]; has {
			t.Fatalf("redemption DTO leaks %q", banned)
		}
	}

	// other bot -> 404
	rec, _ = storeDo(t, router, cookieB, http.MethodGet, "/api/owner/store/redemptions/"+code, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-bot get status = %d, want 404", rec.Code)
	}

	// nonexistent -> 404
	rec, _ = storeDo(t, router, cookieA, http.MethodGet, "/api/owner/store/redemptions/nonexistent0", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing get status = %d, want 404", rec.Code)
	}
	_ = botA
}

// -- missing fields -> 400 --

func TestStoreRedeemMissingFields(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()
	_, cookie := seedStoreBot(t, s, 100)

	rec, _ := storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"request_key": "se_missing_1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing product_code: %d, want 400", rec.Code)
	}
	rec, _ = storeDo(t, router, cookie, http.MethodPost, "/api/owner/store/redemptions",
		map[string]string{"product_code": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing request_key: %d, want 400", rec.Code)
	}
}
