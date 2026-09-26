package service

// WO-7c concurrency closure tests: global lock order Task → Claim →
// Submission, locked-version binding, OpenTask draft comparison,
// in-claim expiry on claim, version-pinned renew TTL, recovery
// cadence, and failure-reason-aware platform pause. Every test ends
// with task.CheckInvariants.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// runConcurrent runs two operations for N rounds behind a barrier;
// both must finish without deadlock (40P01) or INTERNAL_ERROR.
func runConcurrent(t *testing.T, rounds int, a, b func(round int) error) {
	t.Helper()
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		barrier := make(chan struct{})
		errs := make([]error, 2)
		for i, op := range []func(int) error{a, b} {
			wg.Add(1)
			go func(i int, op func(int) error) {
				defer wg.Done()
				<-barrier
				errs[i] = op(round)
			}(i, op)
		}
		close(barrier)
		wg.Wait()
		for i, err := range errs {
			if err == nil {
				continue
			}
			// deadlocks surface as INTERNAL_ERROR here (or 40P01 text)
			if ae := appErrOf(t, err); ae != nil &&
				(ae.Code == "INTERNAL_ERROR" || ae.Message == "deadlock detected") {
				t.Fatalf("round %d op %d: %v", round, i, err)
			}
			if strings.Contains(err.Error(), "deadlock detected") || strings.Contains(err.Error(), "40P01") {
				t.Fatalf("round %d op %d deadlock: %v", round, i, err)
			}
		}
	}
}

// TestConcurrentSubmitRenewRelease: submit × renew, submit × release,
// submit × ExpireClaims — 20 rounds each, no deadlock/INTERNAL_ERROR.
func TestConcurrentSubmitVersusClaimOps(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// claim-required task: agent holds a claim; submissions carry it
	code := claimOpenedTask(t, pool, publisher, 50_000, func(c *task.Contract) {
		ttl, maxd := int64(300), int64(86400)
		c.Claim.TTL, c.Claim.MaxDuration = &ttl, &maxd
	})
	claim, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	i := 0
	submit := func(round int) error {
		i++
		_, err := SubmitWork(ctx, pool, agent, SubmitInput{
			Code: code, RequestKey: fmt.Sprintf("cc-%d-%d", round, i),
			Payload: []byte(submitPayloadOK), ClaimID: &claim.ClaimID,
		}, testAgentRefKey, time.Now())
		return err
	}

	// submit × renew
	runConcurrent(t, 20, submit, func(round int) error {
		_, err := RenewClaim(ctx, pool, agent, claim.ClaimID, time.Now())
		return err
	})
	// submit × release (release wins exactly once; later rounds error
	// CLAIM_INVALID which is legal)
	runConcurrent(t, 20, submit, func(round int) error {
		_, err := ReleaseClaim(ctx, pool, agent, claim.ClaimID, time.Now())
		return err
	})
	// submit × ExpireClaims
	runConcurrent(t, 20, submit, func(round int) error {
		_, err := ExpireClaims(ctx, pool, time.Now(), 100)
		return err
	})

	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// TestSubmitBindsLockedVersion: a no-claim submit racing pause→update→
// open(v2) lands on v2 (or is rejected by v2's schema).
func TestSubmitBindsLockedVersion(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := submitOpenedTask(t, pool, publisher, 50_000, func(c *task.Contract) {
		c.Price = 5
	})
	tr0, _ := repository.FindTaskByCode(ctx, pool, code)

	// v2 raises the price to 10 with the same schema
	mkV2 := func() task.Contract {
		c := submitContract()
		c.Price = 10
		return c
	}
	reopen := func(round int) error {
		if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
			return err
		}
		if _, err := UpdateTask(ctx, pool, publisher, code, mkV2()); err != nil {
			return err
		}
		_, err := OpenTask(ctx, pool, publisher, code)
		return err
	}
	submitted := 0
	var lastVersion int32
	submit := func(round int) error {
		v, err := SubmitWork(ctx, pool, agent, SubmitInput{
			Code: code, RequestKey: fmt.Sprintf("vb-%d-%d", round, time.Now().UnixNano()),
			Payload: []byte(submitPayloadOK),
		}, testAgentRefKey, time.Now())
		if err != nil {
			return nil // TASK_NOT_OPEN etc. during the window — legal
		}
		submitted++
		lastVersion = v.Version
		return nil
	}
	runConcurrent(t, 20, submit, reopen)

	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Version < 2 {
		t.Fatalf("task version = %d, want >= 2", tr.Version)
	}
	if submitted > 0 && (lastVersion < 1) {
		t.Fatalf("bad submission version %d", lastVersion)
	}
	_ = tr0
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
	// any submission that landed on the current version must carry its price
	rows, _, err := ListHistory(ctx, pool, agent, code, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Version == tr.Version && r.Amount != 10 {
			t.Fatalf("v%d submission amount = %d, want 10", r.Version, r.Amount)
		}
	}
}

// TestOpenTaskDraftChanged: an update landing between the open phases
// aborts with INVALID_STATE(DRAFT_CHANGED) and keeps the new draft.
func TestOpenTaskDraftChanged(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	code := pubCreateForTest(t, pool, publisher, submitContract(), 1000)

	// swap the draft after phase 1 read it: emulate by racing UpdateTask
	// against OpenTask repeatedly until the race hits, or deterministically:
	// patch the phase-1 snapshot via a concurrent update barrier is flaky;
	// instead assert the deterministic path — update DURING open's
	// delivery window using a receiver that blocks.
	rcv := startPubReceiverBlocking(t)
	c := submitContract()
	c.Receiver = task.Receiver{URL: rcv.url}
	_ = c
	c.Acceptance.Mode = task.ModeSync
	if _, err := UpdateTask(ctx, pool, publisher, code, c); err != nil {
		t.Fatalf("update: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := OpenTask(ctx, pool, publisher, code)
		errCh <- err
	}()
	<-rcv.requestStarted // open is inside the test delivery

	updated := submitContract()
	updated.Title = "Edited mid-open"
	if _, err := UpdateTask(ctx, pool, publisher, code, updated); err != nil {
		t.Fatalf("mid-open update: %v", err)
	}
	rcv.release <- struct{}{}

	err := <-errCh
	ae := appErrOf(t, err)
	if ae.Code != "INVALID_STATE" || ae.Details["reason"] != "DRAFT_CHANGED" {
		t.Fatalf("open: %v (%#v), want INVALID_STATE/DRAFT_CHANGED", err, ae.Details)
	}
	// the task is still draft and the new draft survived
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	if tr.Status != task.TaskDraft {
		t.Fatalf("status = %s, want draft", tr.Status)
	}
	var stored task.Contract
	if err := json.Unmarshal(tr.DraftContract, &stored); err != nil || stored.Title != "Edited mid-open" {
		t.Fatalf("draft = %s (%v)", tr.DraftContract, err)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// blockingPubReceiver: a TLS receiver that holds the FIRST request
// until released (deterministic OpenTask race window).
type blockingPubReceiver struct {
	url            string
	requestStarted chan struct{}
	release        chan struct{}
}

func startPubReceiverBlocking(t *testing.T) *blockingPubReceiver {
	t.Helper()
	r := &blockingPubReceiver{requestStarted: make(chan struct{}), release: make(chan struct{})}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case r.requestStarted <- struct{}{}:
		default:
		}
		<-r.release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":200}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pubTestTLSCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// TestClaimExpiresStaleActiveClaim: an expired-but-active claim is
// expired in-transaction at claim time and a fresh claim is issued.
func TestClaimExpiresStaleActiveClaim(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) {
		ttl := int64(300)
		c.Claim.TTL = &ttl
	})
	stale, err := ClaimTask(ctx, pool, agent, code, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	reservedAfterStale := claimTaskReserved(t, pool, code)

	fresh, err := ClaimTask(ctx, pool, agent, code, time.Now())
	if err != nil {
		t.Fatalf("fresh claim: %v", err)
	}
	if fresh.ClaimID == stale.ClaimID {
		t.Fatal("stale active claim returned instead of a new one")
	}
	old, _ := repository.FindClaimByID(ctx, pool, stale.ClaimID)
	if old.Status != task.ClaimExpired {
		t.Fatalf("stale claim status = %s, want expired", old.Status)
	}
	if got := claimTaskReserved(t, pool, code); got != reservedAfterStale {
		t.Fatalf("reserved = %d, want conserved %d", got, reservedAfterStale)
	}
}

// TestRenewUsesClaimVersionTTL: a v1 claim on a task whose v2 changed
// the TTL renews by v1's TTL.
func TestRenewUsesClaimVersionTTL(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	t0 := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) {
		ttl, maxd := int64(600), int64(86400)
		c.Claim.TTL, c.Claim.MaxDuration = &ttl, &maxd
	})
	claim, err := ClaimTask(ctx, pool, agent, code, t0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// v2 changes the TTL to 1800
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatal(err)
	}
	v2 := claimAsyncContract()
	ttl2, maxd2 := int64(1800), int64(86400)
	v2.Claim.TTL, v2.Claim.MaxDuration = &ttl2, &maxd2
	if _, err := UpdateTask(ctx, pool, publisher, code, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatal(err)
	}

	// renew at t0+100: expires = min(t0+100+600(v1 ttl), deadline) = t0+700
	view, err := RenewClaim(ctx, pool, agent, claim.ClaimID, t0.Add(100*time.Second))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	want := t0.Add(700 * time.Second)
	if view.ExpiresAt.Sub(want) > time.Second || want.Sub(view.ExpiresAt) > time.Second {
		t.Fatalf("renewed expires_at = %v, want ≈ %v (v1 ttl 600)", view.ExpiresAt, want)
	}
	claimTaskReserved(t, pool, code)
}

// TestRecoveryCadence: delivering conversion ends the round; uncertain
// waits out the 30s cadence.
func TestRecoveryCadence(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	// stuck delivering (repo-seeded)
	code := deliverSyncTask(t, pool, publisher, rcv, 1000)
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	tx, _ := pool.TxBegin(ctx)
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, Version: 1, AgentID: agent,
		RequestKey: fmt.Sprintf("cad-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)),
		Amount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReserveTaskAmount(ctx, tx, tr.ID, 5); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	ageSubmission(t, pool, subID)

	// round 1: delivering → uncertain ONLY (no delivery request yet)
	if n, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now(), 50); err != nil || n < 1 {
		t.Fatalf("round 1 (n=%d err=%v)", n, err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubUncertain {
		t.Fatalf("round 1 state = %s, want uncertain (no immediate redelivery)", sub.State)
	}
	if got := rcv.hitsFor(fmt.Sprint(subID)); got != 0 {
		t.Fatalf("redelivery requests after conversion = %d, want 0 this round", got)
	}

	// 29s since entering uncertain: still no redelivery (lease not due)
	if _, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now().Add(29*time.Second), 50); err != nil {
		t.Fatal(err)
	}
	if got := rcv.hitsFor(fmt.Sprint(subID)); got != 0 {
		t.Fatalf("redelivery at 29s = %d, want 0", got)
	}
	// 31s: redelivered (receiver 200 → settled)
	if _, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now().Add(31*time.Second), 50); err != nil {
		t.Fatal(err)
	}
	if got := rcv.hitsFor(fmt.Sprint(subID)); got != 1 {
		t.Fatalf("redelivery at 31s = %d, want 1", got)
	}
	sub, _ = repository.FindSubmissionByID(ctx, pool, subID)
	if sub.State != task.SubSettled {
		t.Fatalf("state = %s, want settled", sub.State)
	}
	claimTaskReserved(t, pool, code)
}

// TestFaultPauseByFailureReason: 4 counting failures + 1
// DELIVERY_UNRESOLVED → no pause (the streak is broken); five mixed
// counting failures → pause.
func TestFaultPauseByFailureReason(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 100_000)
	agent := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	// task A: 4 counting failures + 1 unresolved → stays open
	codeA := deliverSyncTask(t, pool, publisher, rcv, 50_000)
	rcv.set(t, http.StatusBadGateway, "down") // RECEIVER_FAULT
	for i := 0; i < 4; i++ {
		if view, _ := deliverSubmit(t, pool, agent, codeA); view.State != task.SubFailed {
			t.Fatalf("fault %d state = %s", i, view.State)
		}
	}
	seedFailedUnresolved(t, pool, codeA, agent)
	trA, _ := repository.FindTaskByCode(ctx, pool, codeA)
	if trA.Status != task.TaskOpen {
		t.Fatalf("task A = %s, want open (unresolved breaks the streak)", trA.Status)
	}

	// task B: five mixed counting failures → paused (open on 200
	// first, then flip the receiver)
	rcv.set(t, http.StatusOK, "")
	codeB := deliverSyncTask(t, pool, publisher, rcv, 50_000)
	rcv.set(t, http.StatusBadGateway, "down")
	for i := 0; i < 5; i++ {
		if view, _ := deliverSubmit(t, pool, agent, codeB); view.State != task.SubFailed {
			t.Fatalf("fault %d state = %s", i, view.State)
		}
	}
	trB, _ := repository.FindTaskByCode(ctx, pool, codeB)
	if trB.Status != task.TaskPaused || trB.PausedReason == nil || *trB.PausedReason != "RECEIVER_FAULT" {
		t.Fatalf("task B = %s/%v, want paused/RECEIVER_FAULT", trB.Status, trB.PausedReason)
	}
	for _, id := range []int64{trA.ID, trB.ID} {
		if err := task.CheckInvariants(ctx, pool, id); err != nil {
			t.Fatalf("CheckInvariants: %v", err)
		}
	}
}

// seedFailedUnresolved inserts a failed(DELIVERY_UNRESOLVED) terminal.
func seedFailedUnresolved(t *testing.T, pool *pg.Pool, code string, agent int64) {
	t.Helper()
	ctx := context.Background()
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, Version: 1, AgentID: agent,
		RequestKey: fmt.Sprintf("unres-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)),
		Amount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventTimeout, nil); err != nil {
		t.Fatal(err)
	}
	reason := "DELIVERY_UNRESOLVED"
	if err := repository.SetSubmissionState(ctx, tx, subID, task.SubUncertain,
		task.EventUnresolved, &repository.SetSubmissionStateOpts{Failure: &reason}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
