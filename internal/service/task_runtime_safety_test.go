package service

// Task runtime safety regression tests:
//   D. MaxConns=1 pool — gate failure must release the tx BEFORE the log
//      write (no pool self-deadlock; deterministic, bounded context).
//   E. Kungfu API-key redaction at the task-log sink (delivery payload
//      stays raw; persisted copies are masked).
//   F. UTF-8-safe truncation/normalization, including real task-log
//      PostgreSQL persistence.

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"kungfu.md/internal/pg"
)

// -- D. MaxConns=1 deterministic proof --

// oneConnPool builds a pool with exactly one connection (test-only; the
// production pg.NewPool settings are untouched).
func oneConnPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(pgTestURL())
	pool, err := pg.NewPoolMaxConns(url, 1)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func pgTestURL() string {
	return os.Getenv("KF_TEST_DATABASE_URL")
}

func TestSubmitGateFailureNoPoolDeadlockMaxConns1(t *testing.T) {
	pool := oneConnPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	// unfundable-but-open task → !fundable gate fires while the tx holds
	// the pool's ONLY connection. Old order (log inside tx) deadlocked;
	// new order returns promptly with the existing error and persists
	// the failure log.
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 500000, 1200)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Submit(ctx, pool, code, agent, map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("unfundable task must be rejected")
	}
	if ctx.Err() != nil {
		t.Fatalf("gate failure hit context deadline (pool deadlock): %v", ctx.Err())
	}
	if *hits != 0 {
		t.Fatalf("POST hits = %d, want 0", *hits)
	}

	// failure log persisted
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_task_logs WHERE task_code=$1 AND action='kfcheck' AND success=false`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("gate failure log rows = %d, want 1", n)
	}
}

func TestTestTaskGateFailureNoPoolDeadlockMaxConns1(t *testing.T) {
	pool := oneConnPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, _, hits := newGateServer()
	t.Cleanup(srv.Close)

	// malformed postapi → ValidatePostapi gate fires with the tx open.
	code := tcSeedTask(t, pool, owner, "pending", "not-a-url", 5, 1500)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := TestTaskDeliver(ctx, pool, owner, code, map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("malformed postapi must be rejected")
	}
	if ctx.Err() != nil {
		t.Fatalf("gate failure hit context deadline (pool deadlock): %v", ctx.Err())
	}
	if *hits != 0 {
		t.Fatalf("POST hits = %d, want 0", *hits)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_task_logs WHERE task_code=$1 AND action='kfcheck' AND success=false`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("gate failure log rows = %d, want 1", n)
	}
}

// -- E. redaction --

func rawAPIKey() string {
	hex := make([]byte, 32)
	_, _ = rand.Read(hex)
	return "kf_live_" + fmt.Sprintf("%x", hex)
}

// Owner test payload carries a raw key: the PostAPI delivery receives the
// ORIGINAL payload; the persisted tb_task_logs copy is masked.
func TestTaskLogRedactionOwnerTestPayload(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)

	var delivered atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		delivered.Store(string(buf[:n]))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)
	key := rawAPIKey()

	if _, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{
		"content": "do work with " + key,
	}); err != nil {
		t.Fatalf("owner test: %v", err)
	}

	// delivery payload stays RAW
	dv, _ := delivered.Load().(string)
	if !strings.Contains(dv, key) {
		t.Fatal("PostAPI delivery payload lost the raw key — redaction must not touch delivery")
	}

	// persisted payload log is masked
	var payloadLog string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(payload_json::text, '') FROM tb_task_logs WHERE task_code=$1 AND action='post_succeeded' ORDER BY id DESC LIMIT 1`,
		code).Scan(&payloadLog); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payloadLog, key) {
		t.Fatal("raw API key persisted in tb_task_logs payload")
	}
	if !strings.Contains(payloadLog, "kf_live_****") {
		t.Fatal("masked key representation missing from payload log")
	}
}

// PostAPI response carries a raw key: persisted response_body is masked.
func TestTaskLogRedactionResponseBody(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)

	key := rawAPIKey()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"echo":"` + key + `"}`))
	}))
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)
	res, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("owner test: %v", err)
	}
	// API response preview still contains the raw key (delivery view
	// unchanged); the persisted copy must not.
	if !strings.Contains(res.Post["response_body"].(string), key) {
		t.Fatal("API response preview unexpectedly altered")
	}

	var bodyLog string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(response_body, '') FROM tb_task_logs WHERE task_code=$1 AND action='post_succeeded' ORDER BY id DESC LIMIT 1`,
		code).Scan(&bodyLog); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodyLog, key) {
		t.Fatal("raw API key persisted in tb_task_logs response_body")
	}
	if !strings.Contains(bodyLog, "kf_live_****") {
		t.Fatal("masked representation missing from response_body log")
	}
}

// -- F. UTF-8 safety --

// Multi-byte rune straddling each byte budget: the stored string stays
// valid UTF-8, and the marker policy per path is asserted explicitly.
func TestUTF8TruncationBoundaries(t *testing.T) {
	// budget → withMarker policy (the original contracts)
	policies := []struct {
		budget     int
		withMarker bool
	}{
		{4000, true},   // Submit DB response preview
		{16000, true},  // TestTask API response preview
		{65535, false}, // TestTask DB response_body
		{256, false},   // TestTask DB error_message
	}
	for _, tc := range policies {
		// ASCII fill to budget-2, then "世" (3 bytes) straddles the cut.
		in := strings.Repeat("a", tc.budget-2) + "世界"
		out := normalizeTruncateUTF8(in, tc.budget, tc.withMarker)
		if !utf8.ValidString(out) {
			t.Fatalf("budget %d: invalid UTF-8 after truncation", tc.budget)
		}
		if tc.withMarker && !strings.HasSuffix(out, "... [truncated]") {
			t.Fatalf("budget %d: marker required but missing", tc.budget)
		}
		if !tc.withMarker && strings.Contains(out, "[truncated]") {
			t.Fatalf("budget %d: marker forbidden but present", tc.budget)
		}
		if !tc.withMarker && len(out) > tc.budget {
			t.Fatalf("budget %d: marker-free output exceeds budget: %d", tc.budget, len(out))
		}
	}
	// payload wrapper preview carries no marker (wrapper's _truncated
	// expresses truncation instead)
	preview := normalizeTruncateUTF8(strings.Repeat("世", 40000), testDBPayloadJSONMax-120, false)
	if strings.Contains(preview, "[truncated]") {
		t.Fatal("payload wrapper preview must not carry a marker")
	}
	if !utf8.ValidString(preview) {
		t.Fatal("payload preview invalid UTF-8")
	}
	// invalid input normalizes instead of corrupting
	bad := "ok\xff\xfe" + strings.Repeat("a", 5000)
	out := normalizeTruncateUTF8(bad, 4000, true)
	if !utf8.ValidString(out) {
		t.Fatal("invalid UTF-8 not normalized")
	}
	if !strings.HasPrefix(out, "ok") {
		t.Fatal("normalization dropped valid prefix")
	}
	// short values pass through untouched
	if got := normalizeTruncateUTF8("hello", 4000, true); got != "hello" {
		t.Fatalf("short value altered: %q", got)
	}
}

// Real task-log PostgreSQL persistence of a multi-byte boundary payload.
func TestUTF8TaskLogPersistence(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 3-byte runes crossing the 65535 DB budget boundary.
		_, _ = w.Write([]byte(strings.Repeat("界", 30000)))
	}))
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)
	res, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("owner test: %v", err)
	}
	if !utf8.ValidString(res.Post["response_body"].(string)) {
		t.Fatal("API response preview invalid UTF-8")
	}

	var bodyLog string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(response_body, '') FROM tb_task_logs WHERE task_code=$1 AND action='post_succeeded' ORDER BY id DESC LIMIT 1`,
		code).Scan(&bodyLog); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(bodyLog) {
		t.Fatal("persisted response_body invalid UTF-8")
	}
	if len(bodyLog) > 65535+len("... [truncated]") {
		t.Fatalf("persisted body beyond budget: %d", len(bodyLog))
	}
}

// Invalid UTF-8 from a PostAPI response: delivery semantics unchanged,
// log persistence succeeds, stored text valid UTF-8.
func TestInvalidUTF8ResponsePersistsValidLog(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("good\xff\xfe\xfa bad"))
	}))
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)
	res, err := TestTaskDeliver(context.Background(), pool, owner, code, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("invalid-utf8 response must not change delivery semantics: %v", err)
	}
	if !res.Post["delivered"].(bool) {
		t.Fatal("delivery not accepted")
	}
	if !utf8.ValidString(res.Post["response_body"].(string)) {
		t.Fatal("API preview invalid UTF-8")
	}
	var bodyLog string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(response_body, '') FROM tb_task_logs WHERE task_code=$1 AND action='post_succeeded' ORDER BY id DESC LIMIT 1`,
		code).Scan(&bodyLog); err != nil {
		t.Fatalf("log persistence failed: %v", err)
	}
	if !utf8.ValidString(bodyLog) {
		t.Fatal("persisted log invalid UTF-8")
	}
	if !strings.Contains(bodyLog, "good") || !strings.Contains(bodyLog, "bad") {
		t.Fatalf("valid content lost: %q", bodyLog)
	}
}

// -- A. redaction BEFORE truncation across the Submit 4000 boundary --

// TestSubmitLogRedactionAcrossTruncationBoundary: a raw API key placed so
// it straddles the 4000-byte preview boundary must be masked BEFORE the
// cut — asserting merely "the full key is absent" would pass the old bug
// (which left a partial unmasked key); here the key's hex prefix must not
// appear at all and the masked form must.
func TestSubmitLogRedactionAcrossTruncationBoundary(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)

	key := rawAPIKey()
	keyPrefix := key[:20] // unique-enough hex fragment of the raw key

	// 3960 ASCII bytes + key (73) + tail: the key crosses byte 4000.
	body := strings.Repeat("a", 3960) + key + strings.Repeat("b", 2000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1500)
	res, err := Submit(context.Background(), pool, code, agent, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("delivery must succeed: %v", err)
	}
	if !res.Post["delivered"].(bool) {
		t.Fatal("delivery not accepted")
	}

	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(response_body, '') FROM tb_task_logs WHERE task_code=$1 AND action='post_succeeded' ORDER BY id DESC LIMIT 1`,
		code).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// the raw key's hex prefix must not survive anywhere
	if strings.Contains(stored, keyPrefix) {
		t.Fatal("partial raw key survived truncation (redaction ran after truncation)")
	}
	if strings.Contains(stored, key) {
		t.Fatal("full raw key persisted")
	}
	// masked form present
	if !strings.Contains(stored, "kf_live_****") {
		t.Fatal("masked representation missing")
	}
	// budget + marker preserved
	if !strings.HasSuffix(stored, "... [truncated]") {
		t.Fatal("4000-boundary marker lost")
	}
	if len(stored) > 4000+len("... [truncated]") {
		t.Fatalf("stored body beyond budget: %d", len(stored))
	}
	if !utf8.ValidString(stored) {
		t.Fatal("stored body invalid UTF-8")
	}
}

// TestTaskLogLongErrorMessagePersistsWithinColumn: an error message over
// the VARCHAR(256) column size persists, stored <= 256, valid UTF-8, and
// carries no marker that could overflow the column.
func TestTaskLogLongErrorMessagePersistsWithinColumn(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)

	// network-failure path carries the provider error message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(srv.Close)

	code := tcSeedTask(t, pool, owner, "pending", srv.URL, 5, 1500)

	// drive testLogEvent with an oversized message via the failure path:
	// the handler sets errorMessage from the provider config; instead of
	// coupling to that, call the sink directly with a >256 message.
	longMsg := strings.Repeat("é", 300) // 2-byte runes, 600 bytes
	testLogEvent(context.Background(), pool, code, owner, "post_failed",
		map[string]interface{}{"a": 1}, false, nil, nil, "POSTAPI_TEST", longMsg)

	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(error_message, '') FROM tb_task_logs WHERE task_code=$1 AND action='post_failed' ORDER BY id DESC LIMIT 1`,
		code).Scan(&stored); err != nil {
		t.Fatalf("log INSERT failed (column overflow?): %v", err)
	}
	if len(stored) > 256 {
		t.Fatalf("stored error_message = %d bytes, exceeds VARCHAR(256)", len(stored))
	}
	if strings.Contains(stored, "[truncated]") {
		t.Fatal("marker appended to a VARCHAR(256) column")
	}
	if !utf8.ValidString(stored) {
		t.Fatal("stored error_message invalid UTF-8")
	}
}
