package main

// The WO-5b workers (submission_recovery, review_timeout) ride the
// same runPeriodic lifecycle as every background worker: clean stop is
// not a failure.

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestPeriodicWorkersStopCleanly(t *testing.T) {
	failures := make(chan error, 4) // one slot per worker, as in main

	var ticks int32
	workers := []struct {
		name string
		fn   func()
	}{
		{"submission_recovery", func() { atomic.AddInt32(&ticks, 1) }},
		{"review_timeout", func() { atomic.AddInt32(&ticks, 1) }},
	}

	stops := make([]chan struct{}, 0, len(workers))
	for _, w := range workers {
		stop := make(chan struct{})
		stops = append(stops, stop)
		go runPeriodic(w.name, stop, failures, 2*time.Millisecond, w.fn)
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&ticks) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("workers never ticked")
		}
		time.Sleep(time.Millisecond)
	}
	for _, stop := range stops {
		close(stop)
	}
	select {
	case err := <-failures:
		t.Fatalf("clean stop must not be a failure, got %v", err)
	default:
	}
}

// The retention worker (WO-6b) rides the same lifecycle.
func TestRetentionWorkerStopCleanly(t *testing.T) {
	failures := make(chan error, 5)
	stop := make(chan struct{})
	var ticks int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPeriodic("retention", stop, failures, 2*time.Millisecond, func() {
			atomic.AddInt32(&ticks, 1)
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&ticks) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("retention worker never ticked")
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retention worker did not stop")
	}
	select {
	case err := <-failures:
		t.Fatalf("clean stop must not be a failure, got %v", err)
	default:
	}
}
