package main

// R3.2: background failure propagation proofs. The production
// runRateLimiterGC + ServeLifecycle are exercised directly with
// deterministic injected gc funcs, real http.Server on an ephemeral
// listener, and synthetic shutdown triggers (R1 lifecycle_test style).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// ---- panic bridge: gc panic -> reported fatal error, no crash ----

func TestR32RateLimiterGCPanicReportsFatalError(t *testing.T) {
	failures := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// tiny interval so the first tick fires promptly; the
		// injected gc panics on its FIRST call — the production
		// runner path, not a hand-fed channel value.
		runGCCycle(stop, failures, func() { panic("secret-r32-panic") })
	}()

	select {
	case err := <-failures:
		if err == nil {
			t.Fatal("nil fatal error")
		}
		got := err.Error()
		if !contains(got, "rate_limiter_gc") {
			t.Fatalf("error not identifiable as rate_limiter_gc: %s", got)
		}
		if contains(got, "secret-r32-panic") {
			t.Fatalf("panic value leaked into returned error: %s", got)
		}
		if !contains(got, "panic_type=string") {
			t.Fatalf("panic type missing: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("panic not reported within bound")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker goroutine did not exit after panic")
	}
}

// ---- lifecycle propagation: sentinel fatal -> same shutdown path ----

func TestR32BackgroundFailureTriggersLifecycleShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listener: %v", err)
	}

	var closeCount int32
	hits := make(chan struct{}, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(200)
	})
	hs := &http.Server{Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}

	backgroundErrors := make(chan error, 1)
	signals := make(chan shutdownTrigger, 2)
	gcStop := make(chan struct{})

	lc := &lifecycle{
		httpServer:       hs,
		listener:         ln,
		shutdownBudget:   3 * time.Second,
		signals:          signals,
		backgroundStops:  []chan struct{}{gcStop},
		backgroundErrors: backgroundErrors,
		closers: []io.Closer{closerFunc(func() error {
			atomic.AddInt32(&closeCount, 1)
			return nil
		})},
	}

	serving := make(chan error, 1)
	served := make(chan struct{})
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		for i := 0; i < 50; i++ {
			resp, err := client.Get(fmt.Sprintf("http://%s/", ln.Addr().String()))
			if err == nil {
				resp.Body.Close()
				close(served)
				serving <- nil
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		serving <- fmt.Errorf("server never served")
	}()
	_ = lc

	result := make(chan error, 1)
	go func() { result <- ServeLifecycle(context.Background(), lc) }()

	select {
	case err := <-serving:
		if err != nil {
			t.Fatalf("serve probe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start serving")
	}
	<-served

	// inject the sentinel fatal error through the PRODUCTION input
	sentinel := errors.New("r32-sentinel-fatal")
	backgroundErrors <- sentinel

	select {
	case err := <-result:
		if !errors.Is(err, sentinel) {
			t.Fatalf("returned error = %v, want the sentinel fatal", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ServeLifecycle did not return bounded after background failure")
	}

	// new connections refused after shutdown
	client := &http.Client{Timeout: 500 * time.Millisecond}
	if resp, err := client.Get(fmt.Sprintf("http://%s/", ln.Addr().String())); err == nil {
		resp.Body.Close()
		t.Fatal("new HTTP connection still accepted after shutdown")
	}

	// resource closed EXACTLY once through the single shutdown path
	if n := atomic.LoadInt32(&closeCount); n != 1 {
		t.Fatalf("resource closed %d times, want exactly 1", n)
	}

	// background stop signal closed by the lifecycle owner
	select {
	case <-gcStop:
	default:
		t.Fatal("background stop signal not closed by lifecycle")
	}
}

// ---- normal stop: clean exit, no fatal error, no panic ----

func TestR32RateLimiterGCNormalStopIsNotFailure(t *testing.T) {
	failures := make(chan error, 1)
	stop := make(chan struct{})

	var cycles int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRateLimiterGC(stop, failures, time.Hour, func() { atomic.AddInt32(&cycles, 1) })
	}()

	// stop BEFORE any tick (interval 1h) — the normal shutdown path
	close(stop)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not exit cleanly on stop")
	}

	select {
	case err := <-failures:
		t.Fatalf("normal stop reported a fatal error: %v", err)
	default:
	}
	if atomic.LoadInt32(&cycles) != 0 {
		t.Fatalf("gc ran %d cycles before stop", cycles)
	}
}

// closerFunc adapts a func to io.Closer for lifecycle tests.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
