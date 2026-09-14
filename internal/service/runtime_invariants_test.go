package service

// Runtime invariant tests: non-finite economic inputs are rejected by the
// application layer (never relying on a DB error happening to fire), and
// PostAPI responses are read bounded. Real PostgreSQL.

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/payment"
	"kungfu.md/internal/store"
)

// -- Task inputs: NaN/Inf price/budget rejected, no task, no ledger --

func TestCreateTaskNonFiniteRejected(t *testing.T) {
	pool := a5TestPool(t)

	cases := []struct {
		name   string
		price  float64
		budget float64
	}{
		{"price NaN", math.NaN(), 1500},
		{"budget NaN", 1, math.NaN()},
		{"price +Inf", math.Inf(1), 1500},
		{"budget -Inf", 1, math.Inf(-1)},
	}
	for _, tc := range cases {
		botID := a5TestBotWithBalance(t, pool, 5000)
		_, err := CreateTask(context.Background(), pool, botID, &OwnerTaskConfig{}, &CreateTaskInput{
			Title: "Finite Test", Requirements: "req",
			PostAPI: "https://example.com/h", Price: tc.price, Budget: tc.budget,
		})
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		var n int
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_tasks WHERE bot_id = $1`, botID).Scan(&n)
		if n != 0 {
			t.Fatalf("%s: task row leaked: %d", tc.name, n)
		}
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_transactions WHERE bot_id = $1`, botID).Scan(&n)
		if n != 0 {
			t.Fatalf("%s: ledger leaked: %d", tc.name, n)
		}
		a5CleanupBot(t, pool, botID)
	}
}

// AddTaskBudget NaN → rejected, balance unchanged, budget unchanged.
func TestAddTaskBudgetNonFiniteRejected(t *testing.T) {
	pool := a5TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 5000)

	res, err := CreateTask(context.Background(), pool, botID, &OwnerTaskConfig{}, &CreateTaskInput{
		Title: "AddBudget Finite", Requirements: "req",
		PostAPI: "https://example.com/h", Price: 1, Budget: 1200,
	})
	if err != nil {
		t.Fatal(err)
	}
	code := res["task"].(map[string]interface{})["code"].(string)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM tb_task_logs WHERE task_code=$1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE target_type='task' AND target_id=$1`, code)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_tasks WHERE code=$1`, code)
	})

	var budgetBefore float64
	_ = pool.QueryRow(context.Background(),
		`SELECT budget::float8 FROM tb_tasks WHERE code=$1`, code).Scan(&budgetBefore)

	if _, err := AddTaskBudget(context.Background(), pool, botID, code, math.NaN()); err == nil {
		t.Fatal("NaN budget amount accepted")
	}

	var budgetAfter float64
	_ = pool.QueryRow(context.Background(),
		`SELECT budget::float8 FROM tb_tasks WHERE code=$1`, code).Scan(&budgetAfter)
	if budgetAfter != budgetBefore {
		t.Fatalf("budget changed on NaN add: %v -> %v", budgetBefore, budgetAfter)
	}
	var bal float64
	_ = pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id=$1`, botID).Scan(&bal)
	if bal != 3800 { // 5000 - 1200 locked at create
		t.Fatalf("balance changed on NaN add: %v", bal)
	}
}

// fundable() gate: non-finite never fundable (unit-level proof).
func TestFundableNonFiniteFalse(t *testing.T) {
	for _, tc := range [][2]float64{
		{math.NaN(), 1}, {1500, math.NaN()},
		{math.Inf(1), 1}, {1500, math.Inf(1)},
		{math.Inf(-1), 1}, {1500, math.Inf(-1)},
	} {
		if fundable(tc[0], tc[1]) {
			t.Fatalf("fundable(%v, %v) must be false", tc[0], tc[1])
		}
	}
	if !fundable(1500, 1) {
		t.Fatal("finite fundable pair rejected")
	}
}

// ValidatePrice: non-finite → existing PRICE_INVALID rule.
func TestValidatePriceNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		rule := ValidatePrice(v)
		want := RaiseRule("PRICE_INVALID")
		// The same existing rule (HTTP code + external code) fires for
		// non-finite as for zero/negative prices.
		if rule == nil || rule.Rule.HTTPCode != want.Rule.HTTPCode || rule.Rule.Code != want.Rule.Code {
			t.Fatalf("ValidatePrice(%v) = %v, want the existing PRICE_INVALID contract", v, rule)
		}
	}
}

// -- Payment / Store last-line input defense --

func TestPaymentSpecNonFiniteCreditsRejected(t *testing.T) {
	pool := a5TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 100)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := payment.CreatePendingPayment(context.Background(), pool, botID, payment.PaymentSpec{
			Provider: "manual", AmountMinor: 1000, Currency: "USD", Credits: v,
		})
		if err == nil {
			t.Fatalf("Credits %v accepted", v)
		}
	}
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_payments WHERE bot_id=$1`, botID).Scan(&n)
	if n != 0 {
		t.Fatalf("payment rows leaked: %d", n)
	}
}

func TestStoreProductNonFinitePriceRejected(t *testing.T) {
	pool := a5TestPool(t)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := store.CreateProduct(context.Background(), pool, store.ProductInput{
			Title: "Finite Product", CreditsPrice: v,
		})
		if err == nil {
			t.Fatalf("CreditsPrice %v accepted", v)
		}
	}
	// Only rows this test could have created (unique title prefix).
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_store_products WHERE title LIKE 'Finite Product%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("product rows leaked: %d", n)
	}
}

// -- PostAPI bounded read --

// A 100MB-class 2xx response: delivery succeeds, the captured body is
// bounded by the transport cap, and the reader never buffers the stream.
func TestPostJSONBoundedReadLarge2xx(t *testing.T) {
	chunk := strings.Repeat("x", 64*1024)
	var bytesServed int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ~100MB in 64KB chunks — never materialized server-side either.
		for i := 0; i < 1600; i++ {
			_, _ = w.Write([]byte(chunk))
			bytesServed += int64(len(chunk))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	res := delivery.PostJSON(srv.URL, []byte(`{}`), delivery.TestTaskErrorConfig())
	if !res.Success {
		t.Fatalf("large 2xx must still be a delivery success: %v", res.ErrorCode)
	}
	if res.ResponseBody == nil {
		t.Fatal("response body missing")
	}
	// Transport cap: 65535 bytes, never ~100MB.
	if len(*res.ResponseBody) > 65535 {
		t.Fatalf("captured body = %d bytes, exceeds transport cap", len(*res.ResponseBody))
	}
	// Existing display caps still apply downstream (e.g. TestTask 16000).
	trunc := testTruncateResponse(*res.ResponseBody)
	if len(trunc) > 16000+len("... [truncated]") {
		t.Fatalf("TestTask display cap broken: %d", len(trunc))
	}
	_ = bytesServed
}

// A large non-2xx response: existing rejection semantics + bounded body.
func TestPostJSONBoundedReadLargeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		for i := 0; i < 2000; i++ {
			_, _ = w.Write([]byte(strings.Repeat("y", 64*1024)))
		}
	}))
	t.Cleanup(srv.Close)

	res := delivery.PostJSON(srv.URL, []byte(`{}`), delivery.AgentSubmitErrorConfig())
	if res.Success {
		t.Fatal("non-2xx must still be rejected")
	}
	if res.ResponseBody != nil && len(*res.ResponseBody) > 65535 {
		t.Fatalf("captured error body = %d bytes, exceeds transport cap", len(*res.ResponseBody))
	}
	if res.ErrorCode != "POSTAPI_REJECTED" {
		t.Fatalf("error code = %s", res.ErrorCode)
	}
}

// Integration smoke over a real socket: an oversized streamed response
// is handled without error and the captured body stays within the
// transport cap. This is NOT a drain proof — the deterministic
// byte-count proof lives in internal/delivery/http_post_test.go
// (counting RoundTripper body). Server-side byte counting over
// localhost is timing-dependent and proves nothing here.
func TestPostJSONHugeStreamSmoke(t *testing.T) {
	const totalChunks = 4000 // 4000 x 64KB = ~256MB if fully read
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("z", 64*1024)
		for i := 0; i < totalChunks; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return // client stopped reading — expected under a bounded read
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	res := delivery.PostJSON(srv.URL, []byte(`{}`), delivery.TestTaskErrorConfig())
	if !res.Success {
		t.Fatalf("delivery failed: %v", res.ErrorCode)
	}
	if res.ResponseBody != nil && len(*res.ResponseBody) > 65535 {
		t.Fatalf("body beyond transport cap: %d", len(*res.ResponseBody))
	}
}
