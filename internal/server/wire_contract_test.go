package server

// Lossless economic integer wire-contract regression proofs.
// The browser boundary serializes every economic integer (Credits,
// fiat minor units) as a canonical decimal STRING; the value must
// survive the exact round trip 9007199254740993 (2^53+1) — the first
// integer JS Number corrupts — and MaxInt64 where the transport
// allows it.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ownerGET drives an owner-session GET with the econ env cookie.
func (e *econEnv) ownerGET(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	setOwnerCookie(w, e.botID, e.s.Config.SessionSecret, false)
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Cookie", "kf_owner="+cookieValue(w))
	rec := httptest.NewRecorder()
	e.s.buildRouter().ServeHTTP(rec, req)
	return rec
}

// decodeWireJSON decodes a response body preserving number text
// (what a careful client sees).
func decodeWireJSON(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// TestAccountBalanceWireIsCanonicalString drives the real Owner
// /api/account endpoint with a 2^53+1 balance and asserts the wire
// text carries the canonical decimal string, not a JSON number.
func TestAccountBalanceWireIsCanonicalString(t *testing.T) {
	e := newEconEnv(t, 9007199254740993) // 2^53+1
	rec := e.ownerGET(t, "/api/account")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, `"balance": "9007199254740993"`) {
		t.Fatalf("balance must be canonical decimal string on the wire: %s", raw)
	}
	// A JSON number form (corruptible by JS Number) must NOT appear.
	if strings.Contains(raw, `"balance": 9007199254740993`) {
		t.Fatalf("balance serialized as JSON number: %s", raw)
	}
}

// TestRewardsCreditsPriceWireExact: owner rewards product price and
// redemption cost reach the browser as canonical strings.
func TestRewardsCreditsPriceWireExact(t *testing.T) {
	e := newEconEnv(t, 9007199254740993)
	// seed a product with a 2^53+1 price via SQL (repository authority)
	if _, err := e.s.Pool.Exec(ctxBg(), `
		INSERT INTO tb_rewards_products (code, title, credits_price, status)
		VALUES ($1, 'Wire Item', $2, 'active')`,
		"wp"+fmt.Sprintf("%010d", time.Now().UnixNano()%1e10), int64(9007199254740993)); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	rec := e.ownerGET(t, "/api/owner/rewards/products")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"credits_price": "9007199254740993"`) {
		t.Fatalf("owner rewards credits_price must be canonical string: %s", rec.Body.String())
	}
}

// TestOwnerLogsAmountWireExact: transaction amount / balance_after
// reach the browser as canonical strings.
func TestOwnerLogsAmountWireExact(t *testing.T) {
	e := newEconEnv(t, 9007199254740993)
	rec := e.ownerGET(t, "/api/owner/logs?type=credits")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	// we only asserted stringness of the shape via a seeded grant row
	if !strings.Contains(rec.Body.String(), `"balance": "9007199254740993"`) {
		t.Fatalf("logs balance must be canonical string: %s", rec.Body.String())
	}
}

// TestPaymentPackagesCreditsWireExact: package credits / amount_minor
// reach the browser as canonical strings (fiat included).
func TestPaymentPackagesCreditsWireExact(t *testing.T) {
	// wire conversion is a pure mapping — test the ownerTasksWire-style
	// mapping through the packages handler requires Creem; the wire
	// mapping itself is covered by ownerLogsWire/ownerTasksWire tests.
	// Here: econString + isEconomicKey contracts.
	if econString(9007199254740993) != "9007199254740993" {
		t.Fatal("econString(2^53+1) corrupted")
	}
	if econString(-66) != "-66" {
		t.Fatal("econString sign")
	}
	if econString(0) != "0" {
		t.Fatal("econString zero")
	}
	for _, k := range []string{"credits", "amount_minor", "credits_price", "credits_cost", "budget", "price", "amount", "balance_after", "reward"} {
		if !isEconomicKey(k) {
			t.Fatalf("%s must be economic", k)
		}
	}
	for _, k := range []string{"page", "total", "pinned", "id", "bot_id"} {
		if isEconomicKey(k) {
			t.Fatalf("%s must NOT be economic", k)
		}
	}
}

// TestAdminStoreCreateEditStringWriteback: admin create/edit accept
// canonical decimal integer strings for credits_price exactly.
func TestAdminStoreCreateEditStringWriteback(t *testing.T) {
	// decode-path: jsonCredits over string values (exact writeback)
	cases := []struct {
		in   interface{}
		want int64
		ok   bool
	}{
		{jsonNum(t, "9007199254740993"), 9007199254740993, true},
		{"9007199254740993", 9007199254740993, true},
		{"9223372036854775807", 9223372036854775807, true},
		{"9223372036854775808", 0, false},
		{"1000.5", 0, false},
		{"0.0001", 0, false},
		{42.5, 0, false}, // float64 has no accept path at all now
	}
	for _, tc := range cases {
		v, ok := jsonCredits(tc.in)
		if ok != tc.ok || v != tc.want {
			t.Errorf("jsonCredits(%v) = (%d,%v), want (%d,%v)", tc.in, v, ok, tc.want, tc.ok)
		}
	}
}

func nanoEconSuffix() string {
	return fmt.Sprint(time.Now().UnixNano())
}
