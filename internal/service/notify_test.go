package service

// D7 notify tests (A21): registration challenge, content-free signed
// dispatch on new obligations, and the harmlessness of loss — the
// turn list recomputes from facts with or without delivery.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/repository"
	"sync"
	"testing"
)

func tlsRoots(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

func TestNotifyRegisterAndDispatch(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	// dispatch drains the GLOBAL outbox (≤50 rows per pass): rows
	// left by earlier tests point at endpoints whose certificates are
	// NOT in this test's pinned roots, and their handshake failures
	// would mask our own row's delivery. Retire foreign unsent rows —
	// tests that run later create and assert their own rows.
	if _, err := pool.Exec(ctx,
		`UPDATE notify_outbox SET sent_at = NOW() WHERE sent_at IS NULL`); err != nil {
		t.Fatalf("outbox isolation: %v", err)
	}

	var mu sync.Mutex
	var payloads []map[string]any
	var sawChallenge bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		defer mu.Unlock()
		if m["type"] == "verification" {
			sawChallenge = true
			_, _ = w.Write([]byte(`{"echo":"` + m["challenge"].(string) + `"}`))
			return
		}
		payloads = append(payloads, m)
		if r.Header.Get("X-Kungfu-Signature") == "" {
			t.Error("dispatch without signature header")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	// TestMain already allows loopback for this whole test binary —
	// re-allowing here would restore to FALSE at cleanup and break
	// the delivery tests that run after us (found the hard way). We
	// only pin the TLS roots for our self-signed test server.
	restoreRoots := delivery.TrustRootsForTest(tlsRoots(srv))
	t.Cleanup(restoreRoots)

	// unverified endpoints are refused: a 404 server rejects first
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200) // 2xx but does NOT echo the challenge
	}))
	defer bad.Close()
	if _, err := NotifyRegister(ctx, pool, a, bad.URL); err == nil {
		t.Fatal("a bare 2xx without the challenge echo must be refused")
	}
	if _, err := NotifyRegister(ctx, pool, a, "http://plain.example/x"); err == nil {
		t.Fatal("non-https endpoint must be refused")
	}

	reg, err := NotifyRegister(ctx, pool, a, srv.URL)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !sawChallenge {
		t.Fatal("registration must probe with a challenge")
	}
	if secret, _ := reg["secret"].(string); len(secret) != 64 {
		t.Fatalf("register must disclose the signing secret once, got %v", reg["secret"])
	}

	// a new obligation queues a notification in the same transaction
	code, raw := threadStart(t, pool, owner, "notify", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "a, wake up", "", "", nil, []int64{a}, nil, "n1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	// A21 loss: do NOT dispatch — the turn list still has the item
	todo, err := TodoList(ctx, pool, a, 0, "")
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	if n := len(todo["todos"].([]map[string]any)); n != 1 {
		t.Fatalf("lost notification lost the obligation too: %d items", n)
	}
	// deliver now
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	mu.Lock()
	if len(payloads) != 1 || payloads[0]["kind"] != "reply" || payloads[0]["account"].(float64) != float64(a) {
		t.Fatalf("dispatches = %v", payloads)
	}
	if _, has := payloads[0]["content"]; has {
		t.Fatal("dispatch carried content")
	}
	mu.Unlock()
	// dispatch again: nothing left (at-least-once, not spam)
	if n, err := DispatchNotifyOutbox(ctx, pool); err != nil || n != 0 {
		t.Fatalf("second dispatch: n=%d err=%v", n, err)
	}

	// delete stops delivery; the obligation itself is untouched
	if _, err := NotifyDelete(ctx, pool, a); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "again", "", "", nil, []int64{a}, nil, "n2"); err != nil {
		t.Fatalf("post2: %v", err)
	}
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("dispatch2: %v", err)
	}
	mu.Lock()
	if len(payloads) != 1 {
		t.Fatalf("deleted endpoint still received: %d", len(payloads))
	}
	mu.Unlock()
	todo2, err := TodoList(ctx, pool, a, 0, "")
	if err != nil {
		t.Fatalf("todo2: %v", err)
	}
	if n := len(todo2["todos"].([]map[string]any)); n != 2 {
		t.Fatalf("obligations changed by notify delete: %d", n)
	}
}

// TestNotifyTestQueuesOnlyWithEndpoint: the self-test tool refuses
// accounts without a registered endpoint and queues exactly one
// kind="test" row when one exists — wiring verification without a
// collaborator creating a real obligation.
func TestNotifyTestQueuesOnlyWithEndpoint(t *testing.T) {
	pool := revisionTestPool(t)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()

	// no endpoint: refused, nothing queued
	if _, err := NotifyTest(ctx, pool, a); err == nil {
		t.Fatal("NotifyTest without endpoint must fail")
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notify_outbox WHERE account_id = $1 AND kind = 'test'`, a).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("refused call queued %d rows", n)
	}

	// with an endpoint: one row, and the result carries the contract
	if err := repository.UpsertAccountNotify(ctx, pool, a,
		"https://endpoint.example/wake", "s3cret"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	res, err := NotifyTest(ctx, pool, a)
	if err != nil {
		t.Fatalf("NotifyTest: %v", err)
	}
	if res["queued"] != true || res["kind"] != "test" {
		t.Fatalf("result = %v", res)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notify_outbox WHERE account_id = $1 AND kind = 'test' AND sent_at IS NULL`, a).Scan(&n); err != nil {
		t.Fatalf("count2: %v", err)
	}
	if n != 1 {
		t.Fatalf("queued %d test rows, want 1", n)
	}
}

// TestNotifyOutboxIsGatedAndBounded: an account without a registered
// endpoint is never written at all; a delivered signal is deleted,
// not archived; an abandoned one (attempts run out) is deleted too —
// the outbox is an accelerator, and finished or hopeless work leaves
// nothing behind.
func TestNotifyOutboxIsGatedAndBounded(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "gate", true, "")
	defer cleanupDisposed(t, pool, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// unregistered addressee: the fact exists, the outbox records
	// nothing — a signal nobody can receive is meaningless production
	if _, err := ThreadPost(ctx, pool, owner, code, "no endpoint", "", "", nil, []int64{a}, nil, "g1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM notify_outbox WHERE account_id=$1`, a); n != 0 {
		t.Fatal("unregistered account must not produce outbox rows")
	}

	// registered addressee: the row exists, and a successful dispatch
	// DELETES it (bounded recycling — sent rows are not archived)
	if err := repository.UpsertAccountNotify(ctx, pool, a, "https://endpoint.example/wake", "s3cret"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "with endpoint", "", "", nil, []int64{a}, nil, "g2"); err != nil {
		t.Fatalf("post2: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM notify_outbox WHERE account_id=$1 AND sent_at IS NULL`, a); n != 1 {
		t.Fatal("registered account must get exactly one queued row")
	}
	// point the endpoint at the working test receiver, dispatch
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	// pin the TLS roots for the self-signed test server (loopback is
	// already allowed for the whole binary by TestMain)
	t.Cleanup(delivery.TrustRootsForTest(tlsRoots(srv)))
	if err := repository.UpsertAccountNotify(ctx, pool, a, srv.URL, "s3cret"); err != nil {
		t.Fatalf("reregister: %v", err)
	}
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM notify_outbox WHERE account_id=$1`, a); n != 0 {
		t.Fatal("delivered signal must be deleted, not archived")
	}

	// abandoned: attempts run out, the pass drops the row
	if err := repository.InsertNotifyOutbox(ctx, pool, a, "reply", 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE notify_outbox SET attempts = 5 WHERE account_id=$1`, a); err != nil {
		t.Fatal(err)
	}
	// the endpoint no longer matters: the row's attempts are spent
	if err := repository.UpsertAccountNotify(ctx, pool, a, "https://endpoint.example/dead", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if _, err := DispatchNotifyOutbox(ctx, pool); err != nil {
		t.Fatalf("dispatch2: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM notify_outbox WHERE account_id=$1`, a); n != 0 {
		t.Fatal("abandoned signal must be recycled")
	}
}
