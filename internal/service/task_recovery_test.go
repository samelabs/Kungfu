package service

// Recovery and review-timeout tests (WO-5b) — real PostgreSQL, TLS
// receivers, injectable clocks. Every test ends with
// task.CheckInvariants.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// ageSubmission backdates updated_at so the recovery lease picks the
// row up (test-only seeding).
func ageSubmission(t *testing.T, pool *pg.Pool, subID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tb_task_submission SET updated_at = NOW() - interval '60 seconds' WHERE submission_id = $1`,
		subID); err != nil {
		t.Fatalf("age submission: %v", err)
	}
}

// makeUncertain submits against a sleeping receiver with a short
// caller deadline (§7.2 timeout → uncertain), leaving the reservation
// held.
func makeUncertain(t *testing.T, pool *pg.Pool, publisher, agent int64, rcv *progReceiver) (string, int64) {
	t.Helper()
	rcv.mu.Lock()
	rcv.sleep = 3 * time.Second
	rcv.mu.Unlock()
	code := deliverSyncTask(t, pool, publisher, rcv, 1000) // opens fast (200)
	deadlineCtx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	view, err := SubmitWork(deadlineCtx, pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("unc-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if view.State != task.SubUncertain {
		t.Fatalf("state = %s, want uncertain", view.State)
	}
	return code, view.SubmissionID
}

// -- uncertain redelivery --

func TestRecoverUncertainRedelivers2xx(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code, subID := makeUncertain(t, pool, publisher, agent, rcv)
	ageSubmission(t, pool, subID)
	rcv.mu.Lock()
	rcv.sleep = 0
	rcv.mu.Unlock()

	n, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now(), 50)
	if err != nil || n < 1 {
		t.Fatalf("recover (n=%d, err=%v)", n, err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubSettled {
		t.Fatalf("state = %s, want settled", sub.State)
	}
	var earned int64
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions
		WHERE type='earn_task' AND ref_type='task_submission' AND ref_id=$1`,
		fmt.Sprint(subID)).Scan(&earned)
	if earned != 1 {
		t.Fatalf("earn_task rows = %d, want exactly 1", earned)
	}
	var balance int64
	_ = pool.QueryRow(ctx, `SELECT balance FROM tb_bots WHERE id=$1`, agent).Scan(&balance)
	if balance != 5 {
		t.Fatalf("agent balance = %d, want 5", balance)
	}
	assertEvents(t, pool, subID, task.SubUncertain, task.SubSettled)
	deliverInvariants(t, pool, code)
}

func TestRecoverUncertainRedelivers5xx(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code, subID := makeUncertain(t, pool, publisher, agent, rcv)
	ageSubmission(t, pool, subID)
	rcv.mu.Lock()
	rcv.sleep = 0
	rcv.status = http.StatusInternalServerError
	rcv.mu.Unlock()

	if _, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now(), 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubFailed || sub.Failure == nil || *sub.Failure != "RECEIVER_FAULT" {
		t.Fatalf("sub = %s/%v, want failed/RECEIVER_FAULT", sub.State, sub.Failure)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubUncertain, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// -- 24h unresolved --

func TestRecoverUncertainUnresolved24h(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code, subID := makeUncertain(t, pool, publisher, agent, rcv)
	ageSubmission(t, pool, subID)
	rcv.mu.Lock()
	rcv.sleep = 0
	rcv.mu.Unlock()

	// the recovery clock sits 25h past the first uncertain event
	n, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now().Add(25*time.Hour), 50)
	if err != nil || n < 1 {
		t.Fatalf("recover (n=%d, err=%v)", n, err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubFailed || sub.Failure == nil || *sub.Failure != "DELIVERY_UNRESOLVED" {
		t.Fatalf("sub = %s/%v, want failed/DELIVERY_UNRESOLVED", sub.State, sub.Failure)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0 (released)", tr.Reserved)
	}
	assertEvents(t, pool, subID, task.SubUncertain, task.SubFailed)
	deliverInvariants(t, pool, code)
}

// -- stuck delivering --

func TestRecoverStuckDelivering(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	tr, _ := repository.FindTaskByCode(ctx, pool, code)

	// a submission inserted straight into delivering (crash between
	// intake commit and delivery), its reservation held
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, Version: 1, AgentID: agent,
		RequestKey: fmt.Sprintf("stuck-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)),
		Amount: 5,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repository.ReserveTaskAmount(ctx, tx, tr.ID, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ageSubmission(t, pool, subID)

	if _, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now(), 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubSettled {
		t.Fatalf("state = %s, want settled (delivering → uncertain → redelivered)", sub.State)
	}
	assertEvents(t, pool, subID, task.SubUncertain, task.SubSettled)
	deliverInvariants(t, pool, code)
}

// -- lease: concurrent passes deliver exactly once --

func TestRecoverLeaseSingleDelivery(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code, subID := makeUncertain(t, pool, publisher, agent, rcv)
	ageSubmission(t, pool, subID)
	rcv.mu.Lock()
	rcv.sleep = 0
	rcv.mu.Unlock()

	// count from a clean slate: the aborted original request also
	// reached the server (only the client gave up)
	rcv.mu.Lock()
	rcv.keyHits = map[string]int{}
	rcv.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now(), 50)
		}()
	}
	wg.Wait()

	// the receiver saw exactly ONE request carrying this submission's
	// Idempotency-Key (the lease prevents double redelivery)
	if got := rcv.hitsFor(fmt.Sprint(subID)); got != 1 {
		t.Fatalf("redelivery requests for submission = %d, want exactly 1", got)
	}

	// authoritative check: exactly one earn_task row and one settled event chain
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubSettled {
		t.Fatalf("state = %s, want settled", sub.State)
	}
	var earned int64
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions
		WHERE type='earn_task' AND ref_type='task_submission' AND ref_id=$1`,
		fmt.Sprint(subID)).Scan(&earned)
	if earned != 1 {
		t.Fatalf("earn_task rows = %d, want 1 (single settlement)", earned)
	}
	assertEvents(t, pool, subID, task.SubUncertain, task.SubSettled)
	deliverInvariants(t, pool, code)
}

// -- review timeout --

func makeUnderReview(t *testing.T, pool *pg.Pool, publisher, agent int64, rcv *progReceiver) (string, int64) {
	t.Helper()
	rcv.mu.Lock()
	rcv.status = http.StatusAccepted
	rcv.mu.Unlock()
	c := deliverContract(rcv.url)
	c.Acceptance.Mode = task.ModeAsync
	w := int64(3600)
	c.Acceptance.ReviewWindow = &w
	code := pubCreateForTest(t, pool, publisher, c, 1000)
	if _, err := OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	view, err := SubmitWork(context.Background(), pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("rev-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if view.State != task.SubUnderReview {
		t.Fatalf("state = %s, want under_review", view.State)
	}
	return code, view.SubmissionID
}

func TestExpireReviewsOverdue(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code, subID := makeUnderReview(t, pool, publisher, pubSeedBot(t, pool, 0), rcv)

	// not yet overdue: nothing happens (deadline is submit-time + 3600s)
	if n, err := ExpireReviews(ctx, pool, time.Now(), 100); err != nil || n != 0 {
		t.Fatalf("early expire (n=%d, err=%v)", n, err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubUnderReview {
		t.Fatalf("premature settlement: %s", sub.State)
	}

	// past the window: timeout acceptance with source=timeout. Other
	// tests' overdue reviews in this database may expire too — assert
	// on this submission, not on the pass count.
	if n, err := ExpireReviews(ctx, pool, time.Now().Add(2*time.Hour), 100); err != nil || n < 1 {
		t.Fatalf("expire (n=%d, err=%v)", n, err)
	}
	sub, _ = repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubSettled {
		t.Fatalf("state = %s, want settled", sub.State)
	}
	if !strings.Contains(string(sub.Verdict), `"timeout"`) || !strings.Contains(string(sub.Verdict), `"accepted"`) {
		t.Fatalf("verdict = %s, want accepted/source=timeout", sub.Verdict)
	}
	var earned int64
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tb_transactions
		WHERE type='earn_task' AND ref_type='task_submission' AND ref_id=$1`,
		fmt.Sprint(subID)).Scan(&earned)
	if earned != 1 {
		t.Fatalf("earn_task rows = %d, want 1", earned)
	}
	assertEvents(t, pool, subID, task.SubUnderReview, task.SubSettled)
	deliverInvariants(t, pool, code)
}
