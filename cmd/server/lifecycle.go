package main

// Lifecycle orchestration for the kungfu.md server process — the
// SINGLE owner of startup ordering and shutdown.
//
// Startup (main.go): config load (fail-closed) → DB open + verified
// ping (fail-closed) → server construction → ServeLifecycle.
// Nothing enters serving state after a failed startup dependency.
//
// Shutdown (here): OS signal OR fatal serve error → stop accepting
// new requests → bounded graceful HTTP shutdown → stop background
// ticker → close the DB exactly once → return. One orchestration
// owner; repeated signals are no-ops; no unbounded waits; no new
// background work during shutdown.
//
// Deterministic tests drive ServeLifecycle via the signals channel;
// production main wires it to real SIGINT/SIGTERM.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
)

// lifecycle wires the pieces ServeLifecycle owns.
type lifecycle struct {
	httpServer       *http.Server
	listener         net.Listener // optional: tests hand in a listener
	shutdownBudget   time.Duration
	signals          <-chan shutdownTrigger // OS signals (or a synthetic source in tests)
	backgroundStops  []chan struct{}        // tickers/background loops stopped at shutdown
	closers          []io.Closer            // resource owners closed AFTER HTTP shutdown
	backgroundErrors <-chan error           // fatal background-worker failures (R3.2)
}

// shutdownTrigger is the minimal shape main's signal.Notify feeds in
// (tests use the same channel type without involving the OS).
type shutdownTrigger = struct{}

// ServeLifecycle runs the ONLY shutdown orchestration:
//
//  1. serve in a goroutine; a fatal serve error (anything but
//     http.ErrServerClosed) triggers shutdown and is returned
//  2. on a signal (or serve error / ctx done), stop accepting new
//     requests and give in-flight requests the shutdown budget
//  3. stop background loops, close resource owners once
//
// It is safe to trigger repeatedly — sync.Once guarantees a single
// orchestration; later signals are logged and ignored.
func ServeLifecycle(ctx context.Context, lc *lifecycle) error {
	serveErr := make(chan error, 1)
	go func() {
		var err error
		if lc.listener != nil {
			err = lc.httpServer.Serve(lc.listener)
		} else {
			err = lc.httpServer.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil // normal shutdown, not fatal
		}
		serveErr <- err
	}()

	var once sync.Once
	shutdownDone := make(chan struct{})
	runShutdown := func(reason string) {
		once.Do(func() {
			log.Printf("[kungfu.md] Shutdown starting (%s): budget %s", reason, lc.shutdownBudget)
			defer close(shutdownDone)

			// Bounded graceful HTTP shutdown: stops accepting new
			// requests, waits for in-flight up to the budget.
			sctx, cancel := context.WithTimeout(context.Background(), lc.shutdownBudget)
			defer cancel()
			if err := lc.httpServer.Shutdown(sctx); err != nil {
				// Budget exhausted with connections still open —
				// explicit handling, never a silent pass.
				log.Printf("[kungfu.md] HTTP shutdown: %v (budget exceeded or listener error)", err)
			}
			// Stop background loops before closing resources; shutdown
			// never STARTS new background work.
			for _, stop := range lc.backgroundStops {
				close(stop)
			}
			// Close resource owners exactly once, after HTTP.
			for _, c := range lc.closers {
				if err := c.Close(); err != nil {
					log.Printf("[kungfu.md] resource close: %v", err)
				}
			}
			log.Println("[kungfu.md] Shutdown complete")
		})
	}

	select {
	case err := <-serveErr:
		if err != nil {
			// Fatal runtime serve error → same single shutdown path.
			runShutdown("serve error")
			<-shutdownDone
			return err
		}
		// ErrServerClosed without our orchestration (should not
		// happen); nothing to shut down, listeners are gone.
		runShutdown("server closed")
		<-shutdownDone
		return nil
	case err := <-lc.backgroundErrors:
		// Fatal background-worker failure → the SAME single shutdown
		// path as a fatal serve error (R3.2). The worker itself only
		// REPORTS; lifecycle remains the only shutdown owner.
		runShutdown("background error")
		<-shutdownDone
		return err
	case <-lc.signals:
		runShutdown("signal")
	case <-ctx.Done():
		runShutdown("context canceled")
	}

	// Wait for the shutdown orchestration AND the serve goroutine to
	// finish; repeated signals arriving now are no-ops.
	done := false
	for !done {
		select {
		case <-shutdownDone:
			done = true
		case <-lc.signals:
			log.Println("[kungfu.md] Additional shutdown signal ignored (already shutting down)")
		case err := <-serveErr:
			if err != nil {
				log.Printf("[kungfu.md] serve error during shutdown: %v", err)
			}
		}
	}
	// Drain a final serve result if any (non-blocking).
	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	default:
	}
	return nil
}

// runRateLimiterGC runs the (single) rate-limiter GC loop with a
// panic boundary: a panic inside gc is recovered, logged with a
// diagnostic, and REPORTED as a fatal error on failures. The worker
// never shuts down HTTP, never closes resources, never exits the
// process — ServeLifecycle owns all of that. This is the runner for
// the one managed GC loop, not a generic worker framework.
func runRateLimiterGC(stop <-chan struct{}, failures chan<- error, interval time.Duration, gc func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			runGCCycle(stop, failures, gc)
		case <-stop:
			return
		}
	}
}

// runGCCycle executes one GC tick inside its panic boundary.
func runGCCycle(stop <-chan struct{}, failures chan<- error, gc func()) {
	defer func() {
		if rec := recover(); rec != nil {
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.Printf("[kungfu.md] background worker=rate_limiter_gc panicked (panic_type=%T)\n%s",
				rec, debug.Stack())
			// Bounded report: the panic VALUE stays out of the
			// returned error; only the worker identity and type.
			failures <- fmt.Errorf("background worker rate_limiter_gc panicked (panic_type=%T)", rec)
		}
	}()
	gc()
}
