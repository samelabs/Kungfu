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
	record          func(ev string)
	inFlight        chan struct{} // closed when a pass is mid-flight
	releasePass     chan struct{} // test closes to let the pass finish
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

// TestShutdownStopsClaimsBeforeHTTPDrain proves the corrected order: the
// stop channel closes IMMEDIATELY on the shutdown trigger — BEFORE the
// HTTP graceful drain finishes. While the drain is still in progress (an
// in-flight request keeps it open), a recovery tick must NOT start a new
// pass; the pass only becomes claimable before shutdown was triggered.
func TestShutdownStopsClaimsBeforeHTTPDrain(t *testing.T) {
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

	// In-flight HTTP request that blocks until the test releases it —
	// the graceful drain cannot finish while it is open.
	releaseRequest := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("request_enter")
		<-releaseRequest
		w.WriteHeader(http.StatusOK)
	})
	ln := newListener(t)
	signals := make(chan shutdownTrigger, 4)
	lc := &lifecycle{
		httpServer:      &http.Server{Handler: handler},
		listener:        ln,
		shutdownBudget:  5 * time.Second,
		signals:         signals,
		backgroundStops: []chan struct{}{stop},
		backgroundJoins: []<-chan struct{}{done},
		closers:         []io.Closer{eventCloser{record: record, name: "pool_closed", counter: new(int32)}},
	}

	finished := make(chan error, 1)
	go func() { finished <- ServeLifecycle(testCtx(), lc) }()

	// Fire one in-flight request (retry dial until the serve goroutine
	// has bound the listener — no sleep-based sync).
	reqDone := make(chan struct{})
	go func() {
		var conn net.Conn
		for attempt := 0; ; attempt++ {
			var err error
			conn, err = net.Dial("tcp", ln.Addr().String())
			if err == nil {
				break
			}
			if attempt > 200 {
				t.Error(err)
				close(reqDone)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: test\r\n\r\n"))
		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		_ = n
		close(reqDone)
	}()

	// Trigger shutdown while the request is in flight. Wait first until
	// the request handler has entered — the drain is definitely in
	// progress from this point on.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return contains(events, "request_enter")
	}, 2*time.Second, "in-flight request never entered handler")
	record("shutdown_signal")
	signals <- shutdownTrigger{}

	// Wait until the stop channel is actually closed — i.e. the
	// shutdown orchestration has gated claims. This must happen while
	// the HTTP drain is STILL in progress (the in-flight request keeps
	// it open), proving stop precedes drain completion.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-stop:
			deadline = nil
		default:
		}
		if deadline == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("stop channel not closed while HTTP drain in progress — claims not gated before drain")
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case <-worker.inFlight:
		t.Fatal("no pass may have started yet")
	default:
	}

	// While the HTTP drain is still blocked on the in-flight request,
	// send a recovery tick: the worker must NOT start a pass. (The send
	// may go unheard — the worker exits at its claim gate — so it runs
	// in its own goroutine.)
	go func() { ticks <- time.Now() }()
	select {
	case <-worker.inFlight:
		t.Fatal("new recovery pass started after shutdown trigger, while HTTP drain still in progress")
	case <-time.After(200 * time.Millisecond):
		// expected: no pass started
	}

	// Release the request so the drain can finish and the lifecycle can
	// complete with the worker joined and pool closed after.
	close(releaseRequest)
	<-reqDone
	if err := <-finished; err != nil {
		t.Fatalf("ServeLifecycle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !contains(events, "request_enter") {
		t.Fatalf("in-flight request never ran: %v", events)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// waitFor polls cond until true or timeout — deterministic observation
// gating for cross-goroutine test synchronization.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
