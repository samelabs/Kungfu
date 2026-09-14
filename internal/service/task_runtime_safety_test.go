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

// Multi-byte rune straddling each existing byte budget: the stored string
// stays valid UTF-8 and the truncation marker survives.
func TestUTF8TruncationBoundaries(t *testing.T) {
	for _, budget := range []int{4000, 16000, 65535} {
		// Oversized fixture engineered so the byte cut at `budget`
		// lands strictly inside a 3-byte rune: ASCII fill to budget-2,
		// then "世" (bytes budget-2..budget+1) straddles the cut.
		s2 := strings.Repeat("a", budget-2) + "世界"
		out := truncateUTF8ForLog(s2, budget)
		if !utf8.ValidString(out) {
			t.Fatalf("budget %d: invalid UTF-8 after truncation", budget)
		}
		if !strings.HasSuffix(out, "... [truncated]") {
			t.Fatalf("budget %d: truncation marker lost", budget)
		}
	}
	// invalid input normalizes instead of corrupting
	bad := "ok\xff\xfe" + strings.Repeat("a", 5000)
	out := truncateUTF8ForLog(bad, 4000)
	if !utf8.ValidString(out) {
		t.Fatal("invalid UTF-8 not normalized")
	}
	if !strings.HasPrefix(out, "ok") {
		t.Fatal("normalization dropped valid prefix")
	}
	// short values pass through untouched
	if got := truncateUTF8ForLog("hello", 4000); got != "hello" {
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
