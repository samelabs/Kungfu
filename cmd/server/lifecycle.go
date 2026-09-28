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
	backgroundJoins  []<-chan struct{}      // worker done channels joined BEFORE closers run
	closers          []io.Closer            // resource owners closed AFTER HTTP shutdown and worker join
	backgroundErrors <-chan error           // fatal background-worker failures
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

			// FIRST: stop background loops the instant shutdown is
			// triggered — before the HTTP drain. Closing stop makes the
			// recovery workers stop CLAIMING new work immediately; no
			// new recovery pass may start while in-flight HTTP requests
			// are still being drained.
			for _, stop := range lc.backgroundStops {
				close(stop)
			}
			// Bounded graceful HTTP shutdown: stops accepting new
			// requests, waits for in-flight up to the budget.
			sctx, cancel := context.WithTimeout(context.Background(), lc.shutdownBudget)
			defer cancel()
			if err := lc.httpServer.Shutdown(sctx); err != nil {
				// Budget exhausted with connections still open —
				// explicit handling, never a silent pass.
				log.Printf("[kungfu.md] HTTP shutdown: %v (budget exceeded or listener error)", err)
			}
			// JOIN the workers' in-flight passes before closing the
			// resources they use (the PG pool): a recovery pass mid-HTTP
			// must finish its DB writes before pool.Close(). Bounded by
			// the worker's own pass context; never an unbounded wait.
			for _, done := range lc.backgroundJoins {
				<-done
			}
			// Close resource owners exactly once, after HTTP and workers.
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
		// path as a fatal serve error. The worker itself only
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

// runRateLimiterGC runs the (single) rate-limiter GC loop. The panic
// boundary belongs to the WHOLE runner: ANY panic from gc unwinds the
// entire function — recover once, log a diagnostic, report ONE fatal
// background error, and the runner exits (no further ticks, no
// second send). http.ErrAbortHandler is just a panic value here:
// The HTTP request sentinel contract does NOT extend to the
// background domain. The worker never shuts down HTTP, never closes
// resources, never exits the process — ServeLifecycle owns all of
// that. This is the runner for the one managed GC loop, not a
// generic worker framework.
func runRateLimiterGC(stop <-chan struct{}, failures chan<- error, interval time.Duration, gc func()) <-chan struct{} {
	return runPeriodic("rate_limiter_gc", stop, failures, interval, gc)
}

// runPeriodic runs any periodic background worker on the shared
// lifecycle contract: tick -> fn, stop -> return; a panic in fn is
// caught by this boundary and reported (bounded, without the panic
// value) on failures as a fatal background error routed through the
// single ServeLifecycle shutdown path.
//
// The returned done channel closes exactly when the worker has fully
// exited — main wires it into lifecycle.backgroundJoins so shutdown
// JOINS any in-flight pass BEFORE the closers (the PG pool) run. A
// tick racing shutdown claims no new work: each tick re-checks stop
// (non-blocking) immediately before calling fn.
func runPeriodic(name string, stop <-chan struct{}, failures chan<- error, interval time.Duration, fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[kungfu.md] background worker=%s panicked (panic_type=%T)\n%s",
					name, rec, debug.Stack())
				// Bounded report: the panic VALUE stays out of the
				// returned error; only the worker identity and type.
				failures <- fmt.Errorf("background worker %s panicked (panic_type=%T)", name, rec)
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// claim gate: a tick already received when shutdown
				// began must not start a new pass
				select {
				case <-stop:
					return
				default:
				}
				fn()
			case <-stop:
				return
			}
		}
	}()
	return done
}
