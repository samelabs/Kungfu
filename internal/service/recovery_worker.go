// recovery_worker.go — DB-backed durable submission recovery loop.
//
// tb_task_submissions IS the durable queue (no Redis/Kafka/new infra).
// The worker drives the SAME production primitives the synchronous path
// uses (RecoverPendingSubmissions -> processSubmission / settle), so
// there is exactly one authority — never a worker-side second
// implementation.
//
// Coverage:
//
//	reserved rows with an arrived next_attempt_at   -> first delivery
//	delivering rows with an EXPIRED lease            -> retry delivery
//	delivered rows not yet settled                   -> settle (no POST)
//	uncertain rows                                   -> NEVER auto-retried
//
// Lifecycle contract (wired in cmd/server): starts after the server is
// up, stops taking new work on the stop signal, and a panic is caught by
// the panic boundary and reported through the single background-error
// path — never silently swallowed.
package service

import (
	"context"
	"log"
	"runtime/debug"
	"time"

	"kungfu.md/internal/pg"
)

// RecoveryWorkerConfig tunes the loop. Defaults are deliberately boring:
// a short tick, a bounded batch, simple capped backoff.
type RecoveryWorkerConfig struct {
	Interval time.Duration // scan cadence
	Batch    int           // max submissions per scan
}

// DisputeReconciler re-reads unresolved Creem dispute transactions from
// the authoritative Transaction API and advances the existing payment
// adjustment + Credits reversal authority. Wired to the payment
// package's real implementation in main; nil disables the pass.
type DisputeReconciler func(ctx context.Context) (int, error)

// DefaultRecoveryWorkerConfig: scan every 5s, 20 rows per pass.
func DefaultRecoveryWorkerConfig() RecoveryWorkerConfig {
	return RecoveryWorkerConfig{Interval: 5 * time.Second, Batch: 20}
}

// RunSubmissionRecoveryWorker is the worker runner (mirrors
// runRateLimiterGC's panic-boundary contract): any panic unwinds here,
// is recovered ONCE, and reported as a single fatal background error;
// the runner never shuts down HTTP, never closes resources, never exits
// the process — ServeLifecycle owns all of that.
// RunSubmissionRecoveryWorker starts the worker and returns its done
// channel: closed exactly once when the worker function has fully exited
// (no claim in flight, no DB access after). Shutdown ordering contract:
//
//  1. close(stop) — the worker immediately stops CLAIMING new work
//  2. HTTP graceful shutdown (in parallel, owned by ServeLifecycle)
//  3. <-done — wait for any in-flight recovery pass to finish
//  4. only then may the caller close the PG pool
//
// A pass already running when stop arrives completes (bounded by its own
// 60s context and the delivery client's 10s per-attempt timeout); the
// loop then exits without starting another pass. No framework, no
// goroutine registry — one channel.
func RunSubmissionRecoveryWorker(stop <-chan struct{}, failures chan<- error,
	pool *pg.Pool, cfg RecoveryWorkerConfig, reconcileDisputes DisputeReconciler) <-chan struct{} {

	done := make(chan struct{})
	go func() {
		defer close(done)
		runSubmissionRecoveryWorker(stop, failures, pool, cfg, reconcileDisputes)
	}()
	return done
}

func runSubmissionRecoveryWorker(stop <-chan struct{}, failures chan<- error,
	pool *pg.Pool, cfg RecoveryWorkerConfig, reconcileDisputes DisputeReconciler) {

	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[kungfu.md] background worker=submission_recovery panicked (panic_type=%T)\n%s",
				rec, debug.Stack())
			// Bounded report: the panic VALUE stays out of the error.
			failures <- errRecoveryPanic{rec}
		}
	}()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			log.Println("[kungfu.md] submission recovery worker stopped")
			return
		case <-ticker.C:
			// Claim gate: a tick racing the stop signal must not start
			// a new pass once shutdown has been signalled.
			select {
			case <-stop:
				log.Println("[kungfu.md] submission recovery worker stopped")
				return
			default:
			}
			// Worker ticks are independent of any request deadline; the
			// HTTP attempts inside use the delivery package's own 10s
			// client timeout as the per-attempt bound.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := RecoverPendingSubmissions(ctx, pool, cfg.Batch); err != nil {
				log.Printf("[kungfu.md] submission recovery pass failed: %v", err)
			}
			cancel()

			// Unresolved-dispute reconciliation: Creem exposes no
			// dispute resolution webhook, so the authoritative
			// Transaction must be re-read periodically for disputes
			// that are still open (no refund movement recorded yet).
			// Same pass, same stop gate, same panic boundary; each
			// provider call is the client's own bounded GET.
			if reconcileDisputes != nil {
				rctx, rcancel := context.WithTimeout(context.Background(), 60*time.Second)
				if n, err := reconcileDisputes(rctx); err != nil {
					log.Printf("[kungfu.md] dispute reconciliation pass incomplete (ambiguous provider state, will retry): %v", err)
				} else if n > 0 {
					log.Printf("[kungfu.md] dispute reconciliation advanced %d dispute(s)", n)
				}
				rcancel()
			}
		}
	}
}

type errRecoveryPanic struct{ rec interface{} }

func (e errRecoveryPanic) Error() string {
	return "background worker submission_recovery panicked"
}
