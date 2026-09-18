package main

// R3.2 repair: background failure propagation proofs. The PRODUCTION
// runRateLimiterGC is exercised with deterministic injected gc funcs;
// the end-to-end proof runs a real panicking runner into a real
// ServeLifecycle on an ephemeral listener — no hand-fed sentinel.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closerFunc adapts a func to io.Closer for lifecycle tests.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// ---- ordinary panic: exactly one fatal report + runner exit ----

func TestR32RateLimiterGCPanicReportsFatalError(t *testing.T) {
	failures := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)

	var cycles int32
	done := make(chan struct{})
	interval := 5 * time.Millisecond
	go func() {
		defer close(done)
		runRateLimiterGC(stop, failures, interval, func() {
			atomic.AddInt32(&cycles, 1)
			panic("secret-r32-panic")
		})
	}()

	// exactly one fatal error, bounded
	var err error
	select {
	case err = <-failures:
	case <-time.After(3 * time.Second):
		t.Fatal("panic not reported within bound")
	}
	if err == nil {
		t.Fatal("nil fatal error")
	}
	got := err.Error()
	if !strings.Contains(got, "rate_limiter_gc") {
		t.Fatalf("error not identifiable as rate_limiter_gc: %s", got)
	}
	if strings.Contains(got, "secret-r32-panic") {
		t.Fatalf("panic value leaked into returned error: %s", got)
	}
	if !strings.Contains(got, "panic_type=string") {
		t.Fatalf("panic type missing: %s", got)
	}

	// runner goroutine bounded exit
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runner goroutine did not exit after panic")
	}

	// wait past at least one extra tick window: cycle count stays 1
	time.Sleep(4 * interval)
	if n := atomic.LoadInt32(&cycles); n != 1 {
		t.Fatalf("gc cycles = %d after panic, want exactly 1 (runner must not re-enter)", n)
	}
	// no second error ever sent
	select {
	case err2 := <-failures:
		t.Fatalf("second fatal error sent: %v", err2)
	default:
	}
}

// ---- ErrAbortHandler in the background is just a fatal panic ----

func TestR32RateLimiterGCAbortSentinelIsBackgroundFatal(t *testing.T) {
	failures := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runRateLimiterGC(stop, failures, 5*time.Millisecond, func() {
			panic(http.ErrAbortHandler) // must NOT re-panic out
		})
	}()

	select {
	case err := <-failures:
		if err == nil {
			t.Fatal("nil fatal error")
		}
		got := err.Error()
		if !strings.Contains(got, "rate_limiter_gc") {
			t.Fatalf("error not identifiable as rate_limiter_gc: %s", got)
		}
		if !strings.Contains(got, "panic_type=*errors.errorString") {
			t.Fatalf("panic type not identifiable: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ErrAbortHandler panic not reported — did it escape the worker?")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not exit after ErrAbortHandler panic")
	}
}

// ---- real end-to-end: panicking runner -> lifecycle shutdown ----

func TestR32BackgroundFailureTriggersLifecycleShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listener: %v", err)
	}

	var closeCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	hs := &http.Server{Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}

	backgroundErrors := make(chan error, 1) // production cap 1
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

	// confirm serving first
	result := make(chan error, 1)
	runnerDone := make(chan struct{})
	go func() { result <- ServeLifecycle(context.Background(), lc) }()

	probe := &http.Client{Timeout: 2 * time.Second}
	serving := false
	for i := 0; i < 50; i++ {
		if resp, err := probe.Get(fmt.Sprintf("http://%s/", ln.Addr().String())); err == nil {
			resp.Body.Close()
			serving = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !serving {
		t.Fatal("server did not start serving")
	}

	// the REAL panicking runner feeds the lifecycle — no hand-fed sentinel
	var cycles int32
	go func() {
		defer close(runnerDone)
		runRateLimiterGC(gcStop, backgroundErrors, 5*time.Millisecond, func() {
			atomic.AddInt32(&cycles, 1)
			panic("e2e-gc-boom")
		})
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("ServeLifecycle returned nil after background failure")
		}
		if !strings.Contains(err.Error(), "rate_limiter_gc") {
			t.Fatalf("returned error not identifiable as rate_limiter_gc: %v", err)
		}
		if strings.Contains(err.Error(), "e2e-gc-boom") {
			t.Fatalf("panic value leaked into lifecycle error: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ServeLifecycle did not return bounded after GC panic")
	}

	// new connections refused after shutdown
	reject := &http.Client{Timeout: 500 * time.Millisecond}
	if resp, err := reject.Get(fmt.Sprintf("http://%s/", ln.Addr().String())); err == nil {
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
	// runner exited and ran exactly one cycle
	select {
	case <-runnerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not exit")
	}
	if n := atomic.LoadInt32(&cycles); n != 1 {
		t.Fatalf("gc cycles = %d, want exactly 1", n)
	}
}

// ---- normal stop: clean exit, no fatal error, no gc cycle ----

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

var _ = errors.Is // keep errors import if assertions change shape
