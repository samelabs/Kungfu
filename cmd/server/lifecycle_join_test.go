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

// Shutdown-lifecycle regressions for the background worker join (P2-9):
//
//  1. after the stop signal the worker claims NO new work
//  2. an already-running pass must EXIT before the closers (DB pool) run
//  3. a worker still running when shutdown triggers must never touch a
//     closed pool (join precedes Close)
//
// The tests drive the REAL runPeriodic — the same runner production
// main wires into backgroundJoins — with a fn whose first pass blocks
// until the test releases it. No fake worker machinery.

// periodicWorker records the ordering of its own exit against the
// closer while its pass blocks on releasePass.
type periodicWorker struct {
	mu         sync.Mutex
	record     func(ev string)
	inFlight   chan struct{} // closed when the first pass starts
	releaseAll chan struct{} // closed to let every pass finish
	passes     int32
}

func newPeriodicWorker(record func(ev string)) *periodicWorker {
	return &periodicWorker{
		record:     record,
		inFlight:   make(chan struct{}),
		releaseAll: make(chan struct{}),
	}
}

// fn is the worker body: mark the pass, block until released.
func (w *periodicWorker) fn() {
	w.mu.Lock()
	first := atomic.AddInt32(&w.passes, 1) == 1
	rec := w.record
	w.mu.Unlock()
	rec("pass_start")
	if first {
		close(w.inFlight)
	}
	<-w.releaseAll
	rec("pass_end")
}

func (w *periodicWorker) startCount() int32 { return atomic.LoadInt32(&w.passes) }

// TestShutdownJoinsWorkerBeforeClosingPool: with a worker pass
// IN FLIGHT when the shutdown signal fires, ServeLifecycle must close
// the pool only AFTER the real runPeriodic worker fully exited.
func TestShutdownJoinsWorkerBeforeClosingPool(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	worker := newPeriodicWorker(record)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	done := runPeriodic("test_worker", stop, failures, 5*time.Millisecond, worker.fn)

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
	<-worker.inFlight
	record("shutdown_signal")
	signals <- shutdownTrigger{}

	// Let the pass finish shortly AFTER shutdown started — if the pool
	// were closed before the join, ordering would be pool_closed < pass_end.
	go func() {
		<-time.After(150 * time.Millisecond)
		close(worker.releaseAll)
	}()

	if err := <-finished; err != nil {
		t.Fatalf("ServeLifecycle: %v", err)
	}
	<-done // the real worker's own done channel closed

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
	last := func(name string) int {
		last := -1
		for i, e := range events {
			if e == name {
				last = i
			}
		}
		return last
	}
	if last("pass_end") == -1 || idx("pool_closed") == -1 {
		t.Fatalf("missing events: %v", events)
	}
	if last("pass_end") > idx("pool_closed") {
		t.Fatalf("pool closed BEFORE a worker pass exited: %v", events)
	}
	if idx("pass_start") != last("pass_start")-1 && worker.startCount() < 1 {
		t.Fatalf("unexpected pass accounting: %v", events)
	}
	if atomic.LoadInt32(&closed) != 1 {
		t.Fatalf("pool closed %d times, want exactly 1", closed)
	}
	select {
	case err := <-failures:
		t.Fatalf("worker reported a failure: %v", err)
	default:
	}
}

// TestWorkerClaimsNoNewWorkAfterStop: once stop is closed, the real
// runPeriodic claims no further passes — the per-tick claim gate and
// the stop branch both end the loop, and done closes.
func TestWorkerClaimsNoNewWorkAfterStop(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	worker := newPeriodicWorker(record)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	done := runPeriodic("test_worker", stop, failures, 2*time.Millisecond, worker.fn)

	// First pass in flight, then shutdown WHILE it runs.
	<-worker.inFlight
	record("stop_closed")
	close(stop)
	close(worker.releaseAll) // every later pass (if one wrongly started) may finish too

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit after stop")
	}
	mu.Lock()
	defer mu.Unlock()
	// No pass may START after the stop was recorded closed. (A pass
	// racing in before stop is legal; it must still end before done.)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i] == "stop_closed" {
			break
		}
		if events[i] == "pass_start" {
			t.Fatalf("worker claimed new work after stop: %v", events)
		}
	}
	// and no pass is left hanging: every started pass ended
	starts, ends := 0, 0
	for _, e := range events {
		if e == "pass_start" {
			starts++
		}
		if e == "pass_end" {
			ends++
		}
	}
	if starts != ends {
		t.Fatalf("pass_start=%d pass_end=%d — a pass never finished: %v", starts, ends, events)
	}
}

// TestShutdownStopsClaimsBeforeHTTPDrain proves the corrected order: the
// stop channel closes IMMEDIATELY on the shutdown trigger — BEFORE the
// HTTP graceful drain finishes. While the drain is still in progress (an
// in-flight request keeps it open), no worker pass may start.
func TestShutdownStopsClaimsBeforeHTTPDrain(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	worker := newPeriodicWorker(record)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	// Long interval: no tick may fire naturally during the test — a
	// pass starting here would be a claim AFTER the stop gate.
	done := runPeriodic("test_worker", stop, failures, 65*time.Second, worker.fn)

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
	// no worker pass may start.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-worker.inFlight:
		t.Fatal("new worker pass started after shutdown trigger, while HTTP drain still in progress")
	default:
		// expected: no pass started
	}

	// Release the request so the drain can finish and the lifecycle can
	// complete with the worker joined and pool closed after.
	close(releaseRequest)
	<-reqDone
	if err := <-finished; err != nil {
		t.Fatalf("ServeLifecycle: %v", err)
	}
	close(worker.releaseAll) // safe: no pass ever started

	mu.Lock()
	defer mu.Unlock()
	if !contains(events, "request_enter") {
		t.Fatalf("in-flight request never ran: %v", events)
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
