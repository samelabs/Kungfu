package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Shutdown-lifecycle regressions for the recovery worker join (release
// closure, Task block):
//
//  1. after the stop signal the worker claims NO new work
//  2. an already-running pass must EXIT before the closers (DB pool) run
//  3. a worker still running when shutdown triggers must never touch a
//     closed pool (join precedes Close)
//
// These tests use the REAL lifecycle orchestration (ServeLifecycle) and a
// fake worker with the same stop/done contract as
// service.RunSubmissionRecoveryWorker.

// fakeWorker records the ordering of its own exit against the closer.
type fakeWorker struct {
	stoppedClaiming chan struct{}
	mu              sync.Mutex
	events          []string // ordered global event log
	record          func(ev string)
	inFlight        chan struct{} // closed when a pass is mid-flight
	releasePass     chan struct{} // test closes to let the pass finish
	claimsAfterStop int32
}

// markInFlight signals a pass has started (exactly once).
func (w *fakeWorker) markInFlight() {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.inFlight:
	default:
		close(w.inFlight)
	}
}

func newFakeWorker(record func(ev string)) *fakeWorker {
	return &fakeWorker{
		stoppedClaiming: make(chan struct{}),
		inFlight:        make(chan struct{}),
		releasePass:     make(chan struct{}),
		record:          record,
	}
}

// run mimics the recovery worker contract: stop -> no new claims; a pass
// already running completes before done closes.
func (w *fakeWorker) run(stop <-chan struct{}, ticks <-chan time.Time) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				w.record("worker_exit")
				return
			case <-ticks:
				// claim gate (mirrors the production worker)
				select {
				case <-stop:
					w.record("worker_exit")
					return
				default:
				}
				w.record("pass_start")
				w.markInFlight()
				<-w.releasePass // pass runs until the test releases it
				w.record("pass_end")
			}
		}
	}()
	return done
}

// TestShutdownJoinsWorkerBeforeClosingPool: with a worker pass
// IN FLIGHT when the shutdown signal fires, ServeLifecycle must close
// the pool only AFTER the worker function has fully exited.
func TestShutdownJoinsWorkerBeforeClosingPool(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	worker := newFakeWorker(record)
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	done := worker.run(stop, ticks)

	var closed int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ln := newListener(t)
	signals := make(chan shutdownTrigger, 4)
	lc := &lifecycle{
		httpServer:      &http.Server{Handler: handler},
		listener:        ln,
		shutdownBudget:  2 * time.Second,
		signals:         signals,
		backgroundStops: []chan struct{}{stop},
		backgroundJoins: []<-chan struct{}{done},
		closers:         []io.Closer{eventCloser{record: record, name: "pool_closed", counter: &closed}},
	}

	finished := make(chan error, 1)
	go func() { finished <- ServeLifecycle(testCtx(), lc) }()

	// Start a pass (in flight), then trigger shutdown.
	ticks <- time.Now()
	<-worker.inFlight
	record("shutdown_signal")
	signals <- shutdownTrigger{}

	// Let the pass finish shortly AFTER shutdown started — if the pool
	// were closed before the join, ordering would be pool_closed < pass_end.
	go func() {
		<-time.After(150 * time.Millisecond)
		record("pass_release")
		close(worker.releasePass)
	}()

	if err := <-finished; err != nil {
		t.Fatalf("ServeLifecycle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	idx := func(name string) int {
		for i, e := range events {
			if e == name {
				return i
			}
		}
		return -1
	}
	if idx("pass_end") == -1 || idx("pool_closed") == -1 {
		t.Fatalf("missing events: %v", events)
	}
	if idx("pass_end") > idx("pool_closed") {
		t.Fatalf("pool closed BEFORE worker pass exited: %v", events)
	}
	if idx("worker_exit") > idx("pool_closed") {
		t.Fatalf("pool closed BEFORE worker exited: %v", events)
	}
	if atomic.LoadInt32(&closed) != 1 {
		t.Fatalf("pool closed %d times, want exactly 1", closed)
	}
}

// TestWorkerClaimsNoNewWorkAfterStop: once stop is closed, a tick firing
// must NOT start a new pass (claim gate).
func TestWorkerClaimsNoNewWorkAfterStop(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	worker := newFakeWorker(record)
	stop := make(chan struct{})
	ticks := make(chan time.Time)
	done := worker.run(stop, ticks)

	close(stop) // shutdown signal first
	// Ticks arriving after stop must not claim.
	for i := 0; i < 3; i++ {
		select {
		case ticks <- time.Now():
		case <-time.After(50 * time.Millisecond):
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after stop")
	}
	close(worker.releasePass) // no pass should have started; safe to release
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e == "pass_start" {
			t.Fatalf("worker claimed new work after stop: %v", events)
		}
	}
	if len(events) == 0 || events[len(events)-1] != "worker_exit" {
		t.Fatalf("events = %v", events)
	}
}

type eventCloser struct {
	record  func(string)
	name    string
	counter *int32
}

func (c eventCloser) Close() error {
	c.record(c.name)
	if c.counter != nil {
		atomic.AddInt32(c.counter, 1)
	}
	return nil
}

func newListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func testCtx() context.Context { return context.Background() }
