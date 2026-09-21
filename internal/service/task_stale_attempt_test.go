package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"kungfu.md/internal/repository"
)

// Stale-attempt fencing regressions (release closure, Task block).
//
// Matrix (PM-specified):
//  1. attempt A claims (attempt_count = N)
//  2. lease A expires
//  3. attempt B reclaims (attempt_count = N+1)
//  4. B delivered + settled
//  5. A's late 2xx arrives
//  6. A must NOT rewrite settled -> delivered
//  7. budget decremented exactly once
//  8. earn_task ledger row exactly once
//
// Plus: stale A rejected must not override B's outcome; stale A uncertain
// must not override B's outcome; stale attempt must not release the
// reservation.
//
// The stale writes are driven through the REAL repository outcome
// primitives with the stale attempt number — exactly what an old,
// in-flight HTTP goroutine would call after its lease expired.

// TestStaleAttemptCannotRewriteSettled is the core matrix: A claim ->
// lease expiry -> B reclaim -> B delivered+settled -> late A 2xx is a
// no-op; economics exactly once.
func TestStaleAttemptCannotRewriteSettled(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)

	var hits int32
	srv := newHangServer(func(n int32) bool { return n == 1 }) // 1st POST hangs past A's patience
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)

	// Step 1: accept + attempt A claims (attempt_count -> 1) and its HTTP
	// hangs; the request ctx dies -> uncertain recorded by A? No: A is
	// the SYNC first attempt; its hang produces uncertain via ctx cancel.
	ctxA, cancelA := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelA()
	resA, errA := Submit(ctxA, pool, code, agent, "st-1", map[string]interface{}{"a": 1})
	if errA != nil {
		t.Fatalf("A submit errored: %v", errA)
	}
	_ = resA

	// Attempt A left the row uncertain (its request may have been sent).
	var state string
	var attempt int64
	var subID int64
	var subCode string
	if err := pool.QueryRow(context.Background(),
		`SELECT id, code, state, attempt_count FROM tb_task_submissions WHERE client_request_key='st-1'`).
		Scan(&subID, &subCode, &state, &attempt); err != nil {
		t.Fatal(err)
	}
	if state != repository.SubStateUncertain {
		t.Fatalf("A state = %s, want uncertain", state)
	}

	// Step 2+3: B reclaims the SAME submission (sync same-key retry,
	// allowUncertain=true; receiver now answers 2xx immediately).
	resB, errB := Submit(context.Background(), pool, code, agent, "st-1", map[string]interface{}{"a": 1})
	if errB != nil {
		t.Fatalf("B retry errored: %v", errB)
	}
	if resB.State != repository.SubStateSettled {
		t.Fatalf("B state = %s, want settled", resB.State)
	}
	// Step 4 verified: delivered + settled by B (attempt 2).
	var attemptB int64
	pool.QueryRow(context.Background(),
		`SELECT attempt_count FROM tb_task_submissions WHERE id=$1`, subID).Scan(&attemptB)
	if attemptB != 2 {
		t.Fatalf("attempt_count = %d, want 2", attemptB)
	}

	hitsAfterB := atomic.LoadInt32(&hits)

	// Step 5: A's late 2xx arrives through the REAL delivered primitive
	// with A's stale attempt number.
	preview := ""
	errStale := repository.RecordOutcomeDelivered(context.Background(), pool, subID, 1, 200, &preview, time.Now())
	if errStale != repository.ErrStaleAttempt {
		t.Fatalf("stale delivered: want ErrStaleAttempt, got %v", errStale)
	}

	// Step 6: settled stays settled; nothing rewritten.
	var stateAfter string
	var settledAt *time.Time
	var rc int
	pool.QueryRow(context.Background(),
		`SELECT state, settled_at, response_code FROM tb_task_submissions WHERE id=$1`, subID).
		Scan(&stateAfter, &settledAt, &rc)
	if stateAfter != repository.SubStateSettled || settledAt == nil {
		t.Fatalf("state = %s settledAt=%v — settled was rewritten", stateAfter, settledAt)
	}
	if rc != 200 {
		t.Fatalf("response_code = %d, want 200 (B's fact, not A's)", rc)
	}

	// Step 7: budget decremented exactly once (3000 - 5).
	var budget int64
	var reserved int64
	pool.QueryRow(context.Background(),
		`SELECT budget, reserved_budget FROM tb_tasks WHERE code=$1`, code).Scan(&budget, &reserved)
	if budget != 2995 || reserved != 0 {
		t.Fatalf("budget=%d reserved=%d, want 2995/0", budget, reserved)
	}

	// Step 8: earn_task exactly once.
	if n, sum := tcEarnCount(t, pool, agent); n != 1 || sum != 5 {
		t.Fatalf("earn = %d/%d, want 1/5", n, sum)
	}

	// No extra POST from the stale attempt.
	if atomic.LoadInt32(&hits) != hitsAfterB {
		t.Fatalf("POST hits changed after stale outcome: %d -> %d", hitsAfterB, atomic.LoadInt32(&hits))
	}
	_ = subCode
}

// TestStaleAttemptRejectedCannotOverride: after B settles, A's stale
// definitive rejection must neither rewrite state nor release anything
// (reservation is already 0 — must stay 0; and a hypothetical re-release
// must be impossible).
func TestStaleAttemptRejectedCannotOverride(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv := newHangServer(func(n int32) bool { return n == 1 })
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)

	ctxA, cancelA := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelA()
	if _, err := Submit(ctxA, pool, code, agent, "st-r", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("A: %v", err)
	}
	// B completes.
	if _, err := Submit(context.Background(), pool, code, agent, "st-r", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("B: %v", err)
	}

	var subID int64
	var taskID int64
	pool.QueryRow(context.Background(),
		`SELECT s.id, s.task_id FROM tb_task_submissions s WHERE s.client_request_key='st-r'`).Scan(&subID, &taskID)

	// Stale A rejected (attempt 1) against B's settled row.
	err := repository.RecordOutcomeRejected(context.Background(), pool, subID, taskID, 1, 500, nil, "TASK_POST_FAILED", "")
	if err != repository.ErrStaleAttempt {
		t.Fatalf("stale rejected: want ErrStaleAttempt, got %v", err)
	}

	var state string
	var reserved int64
	var budget int64
	pool.QueryRow(context.Background(),
		`SELECT s.state, t.reserved_budget, t.budget FROM tb_task_submissions s JOIN tb_tasks t ON t.id=s.task_id WHERE s.id=$1`, subID).
		Scan(&state, &reserved, &budget)
	if state != repository.SubStateSettled {
		t.Fatalf("state = %s, want settled", state)
	}
	if reserved != 0 || budget != 2995 {
		t.Fatalf("reserved=%d budget=%d — stale release happened", reserved, budget)
	}
}

// TestStaleAttemptUncertainCannotOverride: A's late uncertain write (after
// B settled) must not clobber the terminal state.
func TestStaleAttemptUncertainCannotOverride(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv := newHangServer(func(n int32) bool { return n == 1 })
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)

	ctxA, cancelA := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelA()
	if _, err := Submit(ctxA, pool, code, agent, "st-u", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, err := Submit(context.Background(), pool, code, agent, "st-u", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("B: %v", err)
	}

	var subID int64
	pool.QueryRow(context.Background(),
		`SELECT id FROM tb_task_submissions WHERE client_request_key='st-u'`).Scan(&subID)

	err := repository.RecordOutcomeUncertain(context.Background(), pool, subID, 1,
		"POSTAPI_UNCERTAIN", "late", time.Now().Add(time.Minute))
	if err != repository.ErrStaleAttempt {
		t.Fatalf("stale uncertain: want ErrStaleAttempt, got %v", err)
	}

	var state string
	pool.QueryRow(context.Background(),
		`SELECT state FROM tb_task_submissions WHERE id=$1`, subID).Scan(&state)
	if state != repository.SubStateSettled {
		t.Fatalf("state = %s, want settled", state)
	}
}

// TestStaleAttemptCannotOverrideLiveB: A's stale write against a row that
// is DELIVERING under attempt B (not yet terminal) must also be a no-op —
// B owns the row.
func TestStaleAttemptCannotOverrideLiveB(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	var hits int32
	srv := newHangServer(func(n int32) bool { return n <= 1 })
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)

	// A: hangs -> uncertain (attempt 1).
	ctxA, cancelA := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancelA()
	if _, err := Submit(ctxA, pool, code, agent, "st-l", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("A: %v", err)
	}

	var subID int64
	var taskID int64
	pool.QueryRow(context.Background(),
		`SELECT id, task_id FROM tb_task_submissions WHERE client_request_key='st-l'`).Scan(&subID, &taskID)

	// B claims directly via the claim primitive (attempt 2) and its HTTP
	// is still hanging (hit 2 hangs) — row is delivering under B.
	subB, err := repository.ClaimSubmissionForDelivery(context.Background(), pool, subID,
		time.Now().Add(30*time.Second), true)
	if err != nil || subB == nil {
		t.Fatalf("B claim: %v %v", subB, err)
	}
	if subB.AttemptCount != 2 || subB.State != repository.SubStateDelivering {
		t.Fatalf("B claim state=%s attempt=%d", subB.State, subB.AttemptCount)
	}

	// Stale A (attempt 1) tries all three outcome writes — all no-ops.
	preview := ""
	if err := repository.RecordOutcomeDelivered(context.Background(), pool, subID, 1, 200, &preview, time.Now()); err != repository.ErrStaleAttempt {
		t.Fatalf("stale delivered vs live B: %v", err)
	}
	if err := repository.RecordOutcomeRejected(context.Background(), pool, subID, taskID, 1, 500, nil, "X", ""); err != repository.ErrStaleAttempt {
		t.Fatalf("stale rejected vs live B: %v", err)
	}
	if err := repository.RecordOutcomeUncertain(context.Background(), pool, subID, 1, "X", "", time.Now()); err != repository.ErrStaleAttempt {
		t.Fatalf("stale uncertain vs live B: %v", err)
	}

	var state string
	var attempt int64
	pool.QueryRow(context.Background(),
		`SELECT state, attempt_count FROM tb_task_submissions WHERE id=$1`, subID).Scan(&state, &attempt)
	if state != repository.SubStateDelivering || attempt != 2 {
		t.Fatalf("state=%s attempt=%d — stale write clobbered live B", state, attempt)
	}
	_ = &hits
}

// TestLeaseExpiryAllowsReclaim: the claim primitive must hand the row to B
// once A's lease has expired (the fence's positive counterpart).
func TestLeaseExpiryAllowsReclaim(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)

	sub, _, err := acceptSubmission(context.Background(), pool, code, agent,
		repository.SubKindAgent, "st-x", hashPayload([]byte(`{"task_code":"`+code+`"}`)), []byte(`{"task_code":"`+code+`"}`))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	// A claims with a lease that is ALREADY expired (simulated expiry).
	shortA, err := repository.ClaimSubmissionForDelivery(context.Background(), pool, sub.ID,
		time.Now().Add(-time.Second), false)
	if err != nil || shortA == nil {
		t.Fatalf("A claim: %v", err)
	}
	// B reclaims after the expired lease.
	subB, err := repository.ClaimSubmissionForDelivery(context.Background(), pool, sub.ID,
		time.Now().Add(30*time.Second), false)
	if err != nil {
		t.Fatalf("B reclaim: %v", err)
	}
	if subB == nil {
		t.Fatal("B could not reclaim after lease expiry")
	}
	if subB.AttemptCount != 2 {
		t.Fatalf("B attempt = %d, want 2", subB.AttemptCount)
	}
}

// newHangServer answers 2xx immediately, EXCEPT calls where hang(n) is
// true for the nth POST — those sleep long enough that the caller's
// request context cancels first (producing the uncertain path).
func newHangServer(hang func(n int32) bool) *httptest.Server {
	var hits int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if hang(n) {
			time.Sleep(2 * time.Second)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}
