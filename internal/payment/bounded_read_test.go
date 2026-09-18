package payment

// R2.2: strict provider-response bounded-read proofs.
// Oversize and mid-stream read failures are I/O ambiguity — never
// ErrCreemDefinitive, never partial-body authoritative facts; a
// checkout response read failure leaves the pending payment pending.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// r22Client builds a CreemClient pointed at the fake provider.
func r22Client(base string) *CreemClient {
	return NewCreemClient(CreemConfig{APIBase: base, APIKey: "test"})
}

// -- GetProduct oversized 2xx: fail closed, no definitive facts --

func TestR22GetProductOversizedResponse(t *testing.T) {
	// a 2xx body >1 MiB whose first 1 MiB looks like a plausible
	// product JSON prefix
	prefix := `{"id":"prod_r22","name":"`
	huge := prefix + strings.Repeat("x", 1<<20) + `"}`
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(huge))
	}))
	defer fake.Close()

	client := r22Client(fake.URL)
	start := time.Now()
	_, err := client.GetProduct(context.Background(), "prod_r22")
	if err == nil {
		t.Fatal("oversized GetProduct response must fail closed")
	}
	var def *ErrCreemDefinitive
	if errors.As(err, &def) {
		t.Fatalf("oversize must be I/O ambiguity, not ErrCreemDefinitive: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("not bounded")
	}
}

// -- readCreemResponse boundary: exact cap ok, cap+1 error --

func TestR22CreemResponseBoundary(t *testing.T) {
	cap := creemResponseCap
	respOK := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("a", cap)))}
	if b, err := readCreemResponse(respOK); err != nil || len(b) != cap {
		t.Fatalf("exact cap: err=%v len=%d", err, len(b))
	}
	respOver := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("a", cap+1)))}
	if _, err := readCreemResponse(respOver); err == nil {
		t.Fatal("cap+1 must error")
	}
}

// -- mid-stream read error: partial valid-looking JSON + non-EOF --
//
// faultingBody returns (valid-looking JSON prefix, then a hard error)
type faultingBody struct{ delivered bool }

func (f *faultingBody) Read(p []byte) (int, error) {
	if !f.delivered {
		f.delivered = true
		copy(p, `{"id":"prod_r22","name":"partial`)
		return 31, nil
	}
	return 0, fmt.Errorf("r22: connection reset mid-stream")
}
func (f *faultingBody) Close() error { return nil }

func TestR22CreemMidStreamReadError(t *testing.T) {
	resp := &http.Response{Body: &faultingBody{}}
	raw, err := readCreemResponse(resp)
	if err == nil {
		t.Fatal("mid-stream read error must surface")
	}
	if len(raw) != 0 {
		t.Fatalf("partial bytes leaked: %d", len(raw))
	}

	// through the production doRaw path: partial+error is an error,
	// never data the JSON parser could accept
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer fake.Close()
	// swap the transport for one that faults mid-body after headers
	client := r22Client(fake.URL)
	client.http.Transport = faultingTransport{}

	_, err = client.doRaw(context.Background(), http.MethodGet, "/v1/products/prod_r22", nil)
	if err == nil {
		t.Fatal("doRaw must fail on mid-stream fault")
	}
	var def *ErrCreemDefinitive
	if errors.As(err, &def) {
		t.Fatalf("mid-stream fault must be ambiguity, not definitive: %v", err)
	}
}

// faultingTransport serves a 2xx response whose body errors mid-stream
// after delivering a valid-looking JSON prefix.
type faultingTransport struct{}

func (faultingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       &faultingBody{},
	}, nil
}

// -- CreateCheckout oversized 2xx: pending payment stays pending --
// Real PostgreSQL: GetProduct fine, CreateCheckout returns >1 MiB —
// the created payment must remain pending (no fail, no provider
// order binding, no credit grant).

func TestR22CreateCheckoutOversizedKeepsPaymentPending(t *testing.T) {
	url := strings.TrimSpace(pgtestURL(t))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()

	var botID int64
	suffix := fmt.Sprint(time.Now().UnixNano())
	if err := pool.QueryRow(ctx, `
		INSERT INTO tb_bots (bot_name, api_key, password_hash, status, balance)
		VALUES ($1, $2, 'x', 'active', 0) RETURNING id`,
		"r22ck_"+suffix, "kf_live_"+suffix+strings.Repeat("a", 64-len(suffix))).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM tb_transactions WHERE bot_id=$1`, botID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_payments WHERE bot_id=$1`, botID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE id=$1`, botID)
	})

	// fake provider: GetProduct normal; CreateCheckout oversized 2xx
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/products/") {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"prod_r22","name":"Starter","billing_type":"onetime","status":"active","mode":"test","price":500,"currency":"USD"}`))
			return
		}
		// checkout creation: a 2xx body strictly larger than 1 MiB
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"ch_","checkout_url":"https://x/` + strings.Repeat("y", 1<<20) + `"}"`))
	}))
	defer fake.Close()

	rt := &CreemRuntime{
		Client:     r22Client(fake.URL),
		Mode:       "test",
		SuccessURL: "https://example.com/ok",
		Packages: map[string]CreemPackageSpec{
			"starter": {Code: "starter", ProductID: "prod_r22", Credits: 10},
		},
	}

	var balanceBefore float64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, botID).Scan(&balanceBefore)

	res, err := StartCreemCheckout(ctx, pool, rt, botID, "starter")
	if err == nil {
		t.Fatalf("oversized CreateCheckout must not succeed: %+v", res)
	}
	ae, ok := IsAppErrR22(err)
	if !ok || ae.HTTPCode != 502 {
		t.Fatalf("expected 502 provider-unavailable, got %v", err)
	}

	// the pending payment exists and STAYS pending
	var status string
	var providerOrder *string
	var grants int
	_ = pool.QueryRow(ctx,
		`SELECT status, provider_order_id FROM tb_payments WHERE bot_id=$1 ORDER BY id DESC LIMIT 1`,
		botID).Scan(&status, &providerOrder)
	if status != "pending" {
		t.Fatalf("payment status = %s, want pending", status)
	}
	if providerOrder != nil {
		t.Fatal("provider order was bound on an oversized checkout")
	}
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='grant_payment'`, botID).Scan(&grants)
	if grants != 0 {
		t.Fatalf("grant_payment rows = %d", grants)
	}
	var balanceAfter float64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, botID).Scan(&balanceAfter)
	if balanceAfter != balanceBefore {
		t.Fatal("balance changed")
	}
}

func pgtestURL(t *testing.T) string {
	t.Helper()
	return os.Getenv("KF_TEST_DATABASE_URL")
}

// IsAppErrR22 unwraps to the canonical AppError.
func IsAppErrR22(err error) (*apperrors.AppError, bool) {
	ae, ok := err.(*apperrors.AppError)
	return ae, ok
}
