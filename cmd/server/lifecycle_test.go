package main

// R1 lifecycle behavior tests (deterministic — no OS-signal flakiness;
// production main wires real SIGINT/SIGTERM, these drive the same
// orchestration via the synthetic signals channel).

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"strings"

	"kungfu.md/internal/config"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/server"
)

// newTestLifecycle starts a real http.Server on an ephemeral port
// with the given handler and returns the lifecycle wiring.
func newTestLifecycle(t *testing.T, handler http.Handler, budget time.Duration, closed *int32) (*lifecycle, string, chan shutdownTrigger) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	httpServer := &http.Server{Handler: handler}
	signals := make(chan shutdownTrigger, 4)
	lc := &lifecycle{
		httpServer:      httpServer,
		listener:        ln,
		shutdownBudget:  budget,
		signals:         signals,
		backgroundStops: []chan struct{}{make(chan struct{})},
		closers:         []io.Closer{countingCloser{closed}},
	}
	return lc, addr, signals
}

type countingCloser struct{ counter *int32 }

func (c countingCloser) Close() error {
	if c.counter != nil {
		atomic.AddInt32(c.counter, 1)
	}
	return nil
}

// Shutdown stops accepting new requests: after ServeLifecycle's
// shutdown completes, a fresh connection is refused.
func TestR1ShutdownStopsAcceptingNewRequests(t *testing.T) {
	var closed int32
	lc, addr, signals := newTestLifecycle(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}), 2*time.Second, &closed)

	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()

	// wait for the listener to actually serve
	waitServing(t, addr)

	// server is serving
	resp, err := httpGet(addr)
	if err != nil || resp != 200 {
		t.Fatalf("pre-shutdown request: resp=%d err=%v", resp, err)
	}

	// trigger shutdown and wait for clean completion
	signals <- shutdownTrigger{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeLifecycle returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}

	// new connections are now refused
	if _, err := httpGet(addr); err == nil {
		t.Fatal("server still accepting connections after shutdown")
	}
	// closers ran exactly once
	if n := atomic.LoadInt32(&closed); n != 1 {
		t.Fatalf("closers ran %d times, want exactly 1", n)
	}
}

// An in-flight (slow) request gets the opportunity to complete
// within the grace budget.
func TestR1InFlightRequestCompletesWithinGrace(t *testing.T) {
	var closed int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release // hold the request in flight
		w.WriteHeader(200)
	})
	lc, addr, signals := newTestLifecycle(t, handler, 3*time.Second, &closed)

	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()

	waitServing(t, addr)

	type result struct {
		code int
		err  error
	}
	reqDone := make(chan result, 1)
	go func() {
		code, err := httpGet(addr)
		reqDone <- result{code, err}
	}()

	// wait until the request is actually in flight
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}

	// trigger shutdown; the in-flight request is still held
	signals <- shutdownTrigger{}

	// give shutdown a moment to enter its wait, then release
	time.Sleep(200 * time.Millisecond)
	close(release)

	select {
	case r := <-reqDone:
		if r.err != nil || r.code != 200 {
			t.Fatalf("in-flight request did not complete: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed within grace")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeLifecycle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung after in-flight completion")
	}
}

// Shutdown timeout does not hang forever: an un-releasable in-flight
// request plus a tiny budget → Shutdown returns on budget expiry and
// the lifecycle completes.
func TestR1ShutdownBudgetDoesNotHang(t *testing.T) {
	var closed int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release // hold the handler IN-FLIGHT past the budget
		w.WriteHeader(200)
	})
	lc, addr, signals := newTestLifecycle(t, handler, 300*time.Millisecond, &closed)

	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()
	waitServing(t, addr)

	// a REAL HTTP request whose handler provably entered (started)
	reqDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get("http://" + addr + "/")
		if err == nil {
			resp.Body.Close()
		}
		reqDone <- err
	}()
	select {
	case <-started:
		// handler is now actively in-flight
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}

	// trigger shutdown WITHOUT releasing: Shutdown must hit its
	// deadline on the ACTIVE request and return bounded
	signals <- shutdownTrigger{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeLifecycle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown exceeded its budget and hung on the active request")
	}
	if n := atomic.LoadInt32(&closed); n != 1 {
		t.Fatalf("closers ran %d times, want 1", n)
	}

	// release the held handler / drain the request goroutine (no leaks)
	close(release)
	select {
	case <-reqDone:
	case <-time.After(5 * time.Second):
		t.Fatal("held request goroutine never drained")
	}
}

// Repeated shutdown triggers do not create a second orchestration or
// panic (sync.Once + buffered signal drain).
func TestR1RepeatedShutdownTriggersAreSafe(t *testing.T) {
	var closed int32
	lc, _, signals := newTestLifecycle(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 2*time.Second, &closed)

	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()

	// burst of triggers
	signals <- shutdownTrigger{}
	signals <- shutdownTrigger{}
	signals <- shutdownTrigger{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeLifecycle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung on repeated triggers")
	}
	if n := atomic.LoadInt32(&closed); n != 1 {
		t.Fatalf("closers ran %d times on repeated triggers, want 1", n)
	}
}

// http.ErrServerClosed from the serve loop is a NORMAL shutdown, not
// a fatal error: ServeLifecycle returns nil.
func TestR1ErrServerClosedIsNormal(t *testing.T) {
	var closed int32
	lc, addr, signals := newTestLifecycle(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 2*time.Second, &closed)

	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()
	waitServing(t, addr)

	// normal signal shutdown: Shutdown() → Serve internally returns
	// http.ErrServerClosed → must map to nil, never fatal
	signals <- shutdownTrigger{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ErrServerClosed must be treated as normal, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle did not finish")
	}
	if n := atomic.LoadInt32(&closed); n != 1 {
		t.Fatalf("closers ran %d times, want 1", n)
	}
}

// A FATAL serve error (listener hijacked into a bad state) is
// returned and the shutdown orchestration still runs exactly once.
func TestR1FatalServeErrorReturnsAndShutsDown(t *testing.T) {
	var closed int32
	// pre-close the listener: ListenAndServe fails immediately with a
	// non-ErrServerClosed error
	lc, _, _ := newTestLifecycle(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 2*time.Second, &closed)
	if err := lc.listener.Close(); err != nil {
		t.Fatalf("listener close: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve error must be returned, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle hung on serve error")
	}
	if n := atomic.LoadInt32(&closed); n != 1 {
		t.Fatalf("closers ran %d times after serve error, want 1", n)
	}
}

// -- startup fail-closed (config + DB semantics) --

// withEnv temporarily sets env vars, runs fn, restores.
func withEnv(t *testing.T, vars map[string]string, fn func()) {
	t.Helper()
	saved := map[string]string{}
	for k, v := range vars {
		saved[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
	fn()
}

func TestR1MissingSessionSecretFailsStartup(t *testing.T) {
	var loadErr error
	withEnv(t, map[string]string{
		"DB_PASS":        "x",
		"SESSION_SECRET": "",
	}, func() {
		_, loadErr = config.Load()
	})
	if loadErr == nil {
		t.Fatal("config without SESSION_SECRET must fail")
	}
}

func TestR1MalformedConfigFailsStartup(t *testing.T) {
	var loadErr error
	withEnv(t, map[string]string{
		"DB_PASS":             "x",
		"SESSION_SECRET":      "s",
		"CREEM_PACKAGES_JSON": "{not json",
	}, func() {
		_, loadErr = config.Load()
	})
	if loadErr == nil {
		t.Fatal("malformed required config must fail")
	}
}

func TestR1ValidConfigLoads(t *testing.T) {
	var loadErr error
	var cfg *config.Config
	withEnv(t, map[string]string{
		"DB_PASS":        "x",
		"SESSION_SECRET": "s",
	}, func() {
		cfg, loadErr = config.Load()
	})
	if loadErr != nil {
		t.Fatalf("valid config must load: %v", loadErr)
	}
	if cfg.SessionSecret != "s" || cfg.ListenAddr == "" {
		t.Fatal("config values missing")
	}
}

func TestR1BadDBFailsBeforeServe(t *testing.T) {
	// invalid DSN → NewPool error (startup fails closed before serving)
	if _, err := pg.NewPool("://not a valid dsn"); err == nil {
		t.Fatal("invalid DB DSN must fail")
	}
	// syntactically valid but unreachable host → connectivity
	// verification fails (NewPool pings with a bounded timeout)
	if _, err := pg.NewPool("postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"); err == nil {
		t.Fatal("unreachable DB must fail")
	}
}

func httpGet(addr string) (int, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// waitServing polls until the address answers (bounded).
func waitServing(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("server at %s never started serving", addr)
}

// ===========================================================================
// R1 repair: full SUCCESS-side startup integration against the real
// CI PostgreSQL — the complete production construction path without
// spawning a cmd/server subprocess or using OS signals.
// ===========================================================================

func TestR1SuccessfulStartupIntegrationRealPG(t *testing.T) {
	// real test DB (CI provides KF_TEST_DATABASE_URL; skip-guard keeps
	// the no-skip CI contract honest in local envs without the var)
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}

	// 1) valid config per the existing Config contract (no env alias,
	//    no compatibility shims — construct the struct directly)
	cfg := &config.Config{
		SessionSecret: "r1-integration-secret",
		ListenAddr:    "127.0.0.1:0", // ephemeral listener supplied below
		RateLimits:    map[string]config.RateLimitConfig{},
	}

	// 2) DB open + connectivity verification on the REAL database
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Fatalf("pg.NewPool(real DB): %v", err)
	}

	// 3) application/router construction
	srv := server.New(cfg, pool)
	handler := srv.Router // the real production http.Handler

	// 4) explicit http.Server + lifecycle on an ephemeral listener;
	//    the lifecycle OWNER closes the pool exactly once
	var closed int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		pool.Close() // listener failure is pre-lifecycle: close manually
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	httpServer := &http.Server{Handler: handler}
	signals := make(chan shutdownTrigger, 2)
	lc := &lifecycle{
		httpServer:      httpServer,
		listener:        ln,
		shutdownBudget:  3 * time.Second,
		signals:         signals,
		backgroundStops: []chan struct{}{make(chan struct{})},
		closers:         []io.Closer{poolCloser{pool}, countingCloser{&closed}},
	}

	// 5) serve and make a REAL request through the production router
	done := make(chan error, 1)
	go func() { done <- ServeLifecycle(context.Background(), lc) }()
	waitServing(t, addr)

	client := &http.Client{Timeout: 5 * time.Second}
	// unauthenticated public route through the REAL production router
	resp, err := client.Get("http://" + addr + "/robots.txt")
	if err != nil {
		t.Fatalf("real router request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /robots.txt = %d, want 200", resp.StatusCode)
	}

	// 6) trigger shutdown → clean return
	signals <- shutdownTrigger{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeLifecycle returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete cleanly")
	}

	// lifecycle closed the pool exactly once (countingCloser is the
	// second closer; poolCloser is the first — both ran once each)
	if n := atomic.LoadInt32(&closed); n != 1 {
		t.Fatalf("closers ran %d times, want 1", n)
	}
	// pool is closed by the lifecycle; a second Close would be the bug
	// the single-owner invariant prevents. Nothing else to clean up.
}
