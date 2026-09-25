package main

// runPeriodic contract proofs: the panic boundary (one bounded fatal
// report, worker-identifiable, panic value never leaked) and the stop
// semantics (ticks observed, clean stop is not a failure).

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunPeriodicPanicBoundary(t *testing.T) {
	failures := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runPeriodic("claim_expiry", stop, failures, 5*time.Millisecond, func() {
			panic("secret-periodic-panic")
		})
	}()

	var err error
	select {
	case err = <-failures:
	case <-time.After(3 * time.Second):
		t.Fatal("panic not reported within bound")
	}
	got := err.Error()
	if !strings.Contains(got, "claim_expiry") {
		t.Fatalf("error not identifiable as claim_expiry: %s", got)
	}
	if strings.Contains(got, "secret-periodic-panic") {
		t.Fatalf("panic value leaked into returned error: %s", got)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("panicking worker did not exit")
	}
}

func TestRunPeriodicStopSemantics(t *testing.T) {
	failures := make(chan error, 1)
	stop := make(chan struct{})

	var ticks int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPeriodic("probe_worker", stop, failures, 2*time.Millisecond, func() {
			atomic.AddInt32(&ticks, 1)
		})
	}()

	// the worker really ticks
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&ticks) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker never ticked")
		}
		time.Sleep(time.Millisecond)
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	select {
	case err := <-failures:
		t.Fatalf("clean stop must not be a failure, got %v", err)
	default:
	}
	if before := atomic.LoadInt32(&ticks); before == 0 {
		t.Fatal("no ticks recorded")
	}
}
