package service

// Runtime invariant tests: invalid integer economic inputs are rejected
// by the application layer (never relying on a DB error happening to
// fire), and PostAPI responses are read bounded. Real PostgreSQL.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/payment"
	"kungfu.md/internal/store"
)

// ValidatePrice: non-positive → existing PRICE_INVALID rule.
// -- Payment / Store last-line input defense --

func TestPaymentSpecNonPositiveCreditsRejected(t *testing.T) {
	pool := a5TestPool(t)
	botID := a5TestBotWithBalance(t, pool, 100)
	for _, v := range []int64{0, -5} {
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

func TestStoreProductNonPositivePriceRejected(t *testing.T) {
	pool := a5TestPool(t)
	for _, v := range []int64{0, -3} {
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

	res := delivery.PostJSON(context.Background(), srv.URL, []byte(`{}`), nil, delivery.TestTaskErrorConfig())
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

	res := delivery.PostJSON(context.Background(), srv.URL, []byte(`{}`), nil, delivery.AgentSubmitErrorConfig())
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

	res := delivery.PostJSON(context.Background(), srv.URL, []byte(`{}`), nil, delivery.TestTaskErrorConfig())
	if !res.Success {
		t.Fatalf("delivery failed: %v", res.ErrorCode)
	}
	if res.ResponseBody != nil && len(*res.ResponseBody) > 65535 {
		t.Fatalf("body beyond transport cap: %d", len(*res.ResponseBody))
	}
}
