package service

// Claim mechanism tests (WO-3) — real PostgreSQL, injectable clock,
// every test ends with task.CheckInvariants.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

// claimContract is the default test contract (claim required) on the
// package-wide accept-everything receiver.
func claimContract() task.Contract {
	return pubContract("")
}

func claimOpenedTask(t *testing.T, pool *pg.Pool, publisher int64, budget int64, mutate func(*task.Contract)) string {
	t.Helper()
	c := claimContract()
	if mutate != nil {
		mutate(&c)
	}
	code := pubCreateForTest(t, pool, publisher, c, budget)
	if _, err := OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code
}

func claimTaskReserved(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	tr, err := repository.FindTaskByCode(context.Background(), pool, code)
	if err != nil || tr == nil {
		t.Fatalf("reload task: %v", err)
	}
	if err := task.CheckInvariants(context.Background(), pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
	return tr.Reserved
}

// -- claim success + idempotency --

func TestClaimTaskSuccessAndIdempotent(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)

	first, err := ClaimTask(ctx, pool, agent, code, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first.Amount != 5 || first.Status != task.ClaimActive {
		t.Fatalf("claim view = %+v", first)
	}
	if got := claimTaskReserved(t, pool, code); got != 5 {
		t.Fatalf("reserved = %d, want 5 (price)", got)
	}

	// §5.2 amendment: an existing active claim is returned as-is.
	second, err := ClaimTask(ctx, pool, agent, code, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.ClaimID != first.ClaimID {
		t.Fatalf("second claim created a new claim: %d vs %d", second.ClaimID.Int64(), first.ClaimID.Int64())
	}
	if got := claimTaskReserved(t, pool, code); got != 5 {
		t.Fatalf("reserved after idempotent claim = %d, want 5", got)
	}
}

// -- guards --

func TestClaimTaskNotOpen(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	paused := claimOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := PauseTask(ctx, pool, publisher, paused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	closed := claimOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := CloseTask(ctx, pool, publisher, closed); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, tc := range []struct{ code, want string }{
		{paused, task.TaskPaused},
		{closed, task.TaskClosed},
	} {
		_, err := ClaimTask(ctx, pool, agent, tc.code, time.Now())
		appErr := appErrOf(t, err)
		if appErr.Code != "TASK_NOT_OPEN" || appErr.Details["status"] != tc.want {
			t.Fatalf("%s: %v (%#v), want TASK_NOT_OPEN/%s", tc.want, err, appErr.Details, tc.want)
		}
	}
	for _, code := range []string{paused, closed} {
		claimTaskReserved(t, pool, code) // + invariants
	}
}

func TestClaimTaskOwnTask(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := ClaimTask(ctx, pool, publisher, code, time.Now()); appErrOf(t, err).Code != "OWN_TASK" {
		t.Fatalf("own task claim: %v, want OWN_TASK", err)
	}
	claimTaskReserved(t, pool, code)
}

func TestClaimTaskSlotsExhausted(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	a1 := pubSeedBot(t, pool, 0)
	a2 := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// price = budget → exactly one slot
	code := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) { c.Price = 1000 })
	if _, err := ClaimTask(ctx, pool, a1, code, time.Now()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := ClaimTask(ctx, pool, a2, code, time.Now()); appErrOf(t, err).Code != "SLOTS_EXHAUSTED" {
		t.Fatalf("second claim: %v, want SLOTS_EXHAUSTED", err)
	}
	if got := claimTaskReserved(t, pool, code); got != 1000 {
		t.Fatalf("reserved = %d, want 1000", got)
	}
}

// -- §5.3 step 5 limits --

// seedSubmission inserts a submission in `state` (with reservation and
// settlement for the settled variant) directly through the repository
// primitives, so limit counting is exercised against real rows.
func seedSubmission(t *testing.T, pool *pg.Pool, code string, agent int64, state string) {
	t.Helper()
	ctx := context.Background()
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	key := fmt.Sprintf("lim-%s-%d-%d", code, agent, time.Now().UnixNano())
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, AgentID: agent, RequestKey: key,
		Payload:     []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
		PayloadHash: strings.Repeat("a", 64), Amount: tr2price(t, pool, code),
		ContractVersion: tr.ContractVersion,
	})
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	switch state {
	case task.SubRejected:
		if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventDeliver4XX, nil); err != nil {
			t.Fatalf("reject: %v", err)
		}
	case task.SubSettled:
		price := tr2price(t, pool, code)
		if err := repository.ReserveTaskAmount(ctx, tx, tr.ID, price); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventDeliver2XX, nil); err != nil {
			t.Fatalf("settle state: %v", err)
		}
		if err := repository.SettleTaskSubmission(ctx, pool, tx, tr.ID, subID, agent, price); err != nil {
			t.Fatalf("settle money: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func tr2price(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	tr, _ := repository.FindTaskByCode(context.Background(), pool, code)
	v, err := repository.FindTaskByID(context.Background(), pool, tr.ID)
	if err != nil || v == nil {
		t.Fatalf("version: %v", err)
	}
	var c task.Contract
	if err := json.Unmarshal(v.Contract, &c); err != nil {
		t.Fatalf("contract: %v", err)
	}
	return c.Price
}

func TestClaimTaskSubmissionLimitRejected(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	for i := 0; i < 5; i++ { // 缺省 max_rejected_per_agent = 5
		seedSubmission(t, pool, code, agent, task.SubRejected)
	}
	_, err := ClaimTask(ctx, pool, agent, code, time.Now())
	appErr := appErrOf(t, err)
	if appErr.Code != "SUBMISSION_LIMIT" || appErr.Details["limit"] != "rejected" {
		t.Fatalf("claim: %v (%#v), want SUBMISSION_LIMIT/rejected", err, appErr.Details)
	}
	claimTaskReserved(t, pool, code)
}

// -- concurrency --

func TestClaimTaskConcurrentSlotsOne(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	a1 := pubSeedBot(t, pool, 0)
	a2 := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) { c.Price = 1000 })
	now := time.Now()

	var wg sync.WaitGroup
	results := make([]error, 2)
	claim := func(agent int64, i int) {
		defer wg.Done()
		_, err := ClaimTask(ctx, pool, agent, code, now)
		results[i] = err
	}
	wg.Add(2)
	go claim(a1, 0)
	go claim(a2, 1)
	wg.Wait()

	codes := [2]string{}
	for i, err := range results {
		if err == nil {
			codes[i] = "ok"
			continue
		}
		appErr, ok := err.(*errors.AppError)
		if !ok {
			t.Fatalf("result %d: unexpected error %v", i, err)
		}
		codes[i] = appErr.Code
	}
	if codes[0] == codes[1] {
		t.Fatalf("both goroutines got %q, want exactly one success", codes[0])
	}
	for _, c := range codes {
		if c != "ok" && c != "SLOTS_EXHAUSTED" {
			t.Fatalf("unexpected result %q", c)
		}
	}
	if got := claimTaskReserved(t, pool, code); got != 1000 {
		t.Fatalf("reserved = %d, want exactly one price reserved", got)
	}
}

// -- renew --

func TestClaimRenew(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	t0 := time.Now()

	// ttl 300, max_duration 600 → deadline = t0+600
	code := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) {
		ttl, maxd := int64(300), int64(600)
		c.Claim.TTL, c.Claim.MaxDuration = &ttl, &maxd
	})
	claim, err := ClaimTask(ctx, pool, agent, code, t0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	t0us := t0.Truncate(time.Microsecond)
	if !claim.ExpiresAt.Equal(t0us.Add(300 * time.Second)) {
		t.Fatalf("expires_at = %v, want t0+300s", claim.ExpiresAt)
	}

	// renew mid-flight: moves forward, still within the deadline
	r1, err := RenewClaim(ctx, pool, agent, claim.ClaimID.Int64(), t0.Add(200*time.Second))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !r1.ExpiresAt.Equal(t0us.Add(500 * time.Second)) {
		t.Fatalf("renewed expires_at = %v, want t0+500s", r1.ExpiresAt)
	}

	// renew near the deadline: capped at the deadline
	r2, err := RenewClaim(ctx, pool, agent, claim.ClaimID.Int64(), t0.Add(400*time.Second))
	if err != nil {
		t.Fatalf("renew near deadline: %v", err)
	}
	if !r2.ExpiresAt.Equal(t0us.Add(600 * time.Second)) {
		t.Fatalf("capped expires_at = %v, want t0+600s (deadline)", r2.ExpiresAt)
	}

	// at/after the deadline → CLAIM_INVALID
	if _, err := RenewClaim(ctx, pool, agent, claim.ClaimID.Int64(), t0.Add(601*time.Second)); appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("renew past deadline: %v, want CLAIM_INVALID", err)
	}

	// someone else's claim → CLAIM_INVALID
	if _, err := RenewClaim(ctx, pool, stranger, claim.ClaimID.Int64(), t0.Add(250*time.Second)); appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("stranger renew: %v, want CLAIM_INVALID", err)
	}
	claimTaskReserved(t, pool, code)
}

func TestClaimRenewTaskNotOpen(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	t0 := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim, err := ClaimTask(ctx, pool, agent, code, t0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// §4 pause: existing active claims may still submit but cannot renew
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, err = RenewClaim(ctx, pool, agent, claim.ClaimID.Int64(), t0.Add(60*time.Second))
	appErr := appErrOf(t, err)
	if appErr.Code != "TASK_NOT_OPEN" || appErr.Details["status"] != task.TaskPaused {
		t.Fatalf("renew on paused: %v (%#v), want TASK_NOT_OPEN/paused", err, appErr.Details)
	}
	claimTaskReserved(t, pool, code)
}

// -- release --

func TestClaimRelease(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim, err := ClaimTask(ctx, pool, agent, code, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// someone else → CLAIM_INVALID, reservation intact
	if _, err := ReleaseClaim(ctx, pool, stranger, claim.ClaimID.Int64(), now); appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("stranger release: %v, want CLAIM_INVALID", err)
	}
	if got := claimTaskReserved(t, pool, code); got != 5 {
		t.Fatalf("reserved = %d, want 5", got)
	}

	released, err := ReleaseClaim(ctx, pool, agent, claim.ClaimID.Int64(), now)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.Status != task.ClaimReleased {
		t.Fatalf("released status = %s", released.Status)
	}
	if got := claimTaskReserved(t, pool, code); got != 0 {
		t.Fatalf("reserved after release = %d, want 0", got)
	}

	// second release → CLAIM_INVALID
	if _, err := ReleaseClaim(ctx, pool, agent, claim.ClaimID.Int64(), now); appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("second release: %v, want CLAIM_INVALID", err)
	}
	claimTaskReserved(t, pool, code)
}

// -- expiry reclaimer --

func TestExpireClaims(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	keeper := pubSeedBot(t, pool, 0)
	expiring := pubSeedBot(t, pool, 0)
	user := pubSeedBot(t, pool, 0)
	releaser := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	t0 := time.Now()

	// ttl 300: budget 2000 price 5 → 400 slots, several agents
	mk := func(mutate func(*task.Contract)) string {
		return claimOpenedTask(t, pool, publisher, 2000, mutate)
	}
	shortTTL := func(c *task.Contract) {
		ttl := int64(300)
		c.Claim.TTL = &ttl
	}

	tExp := mk(shortTTL)
	tKeep := mk(shortTTL) // active, not yet expired at the pass time
	tUsed := mk(shortTTL)
	tRel := mk(shortTTL)

	cExp, err := ClaimTask(ctx, pool, expiring, tExp, t0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := ClaimTask(ctx, pool, keeper, tKeep, t0.Add(200*time.Second)); err != nil {
		t.Fatalf("claim keep: %v", err)
	}
	cUsed, err := ClaimTask(ctx, pool, user, tUsed, t0)
	if err != nil {
		t.Fatalf("claim used: %v", err)
	}
	// the used claim's reservation transfers to its submission (WO-4
	// will do this atomically; here the same end-state is seeded)
	{
		tx, err := pool.TxBegin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := repository.ApplyClaimStatus(ctx, tx, cUsed.ClaimID.Int64(), task.ClaimActive, task.EventClaimUse); err != nil {
			t.Fatalf("use claim: %v", err)
		}
		if _, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
			TaskID: mustTaskID(t, pool, tUsed), AgentID: user,
			RequestKey:  fmt.Sprintf("used-%d", cUsed.ClaimID.Int64()),
			Payload:     []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
			PayloadHash: strings.Repeat("b", 64), Amount: cUsed.Amount, ClaimID: cUsed.ClaimID.Int64Ptr(),
			ContractVersion: cUsed.ContractVersion,
		}); err != nil {
			t.Fatalf("insert used submission: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if _, err := ReleaseClaim(ctx, pool, releaser, 0, t0); err == nil {
		t.Fatal("claim 0 should not exist")
	}
	cRel, err := ClaimTask(ctx, pool, releaser, tRel, t0)
	if err != nil {
		t.Fatalf("claim rel: %v", err)
	}
	if _, err := ReleaseClaim(ctx, pool, releaser, cRel.ClaimID.Int64(), t0); err != nil {
		t.Fatalf("release: %v", err)
	}

	// pass at t0+301s: only cExp is expired (t0+300). Earlier tests in
	// THIS package leave their own short-TTL active claims (ttl 300
	// claimed seconds ago), which the same cutoff also expires —
	// assert n >= 1 plus THIS claim's exact outcome below.
	n, err := ExpireClaims(ctx, pool, t0.Add(301*time.Second), 100)
	if err != nil {
		t.Fatalf("ExpireClaims: %v", err)
	}
	if n < 1 {
		t.Fatalf("expired = %d, want >= 1", n)
	}
	after, _ := repository.FindClaimByID(ctx, pool, cExp.ClaimID.Int64())
	if after.Status != task.ClaimExpired {
		t.Fatalf("expired claim status = %s", after.Status)
	}
	if got := claimTaskReserved(t, pool, tExp); got != 0 {
		t.Fatalf("reserved after expiry = %d, want 0", got)
	}
	// untouched: keeper still active with reservation; used and
	// released keep their statuses
	k, _ := repository.FindActiveClaimByTaskAgent(ctx, pool, mustTaskID(t, pool, tKeep), keeper)
	if k == nil {
		t.Fatal("unexpired active claim was expired")
	}
	u, _ := repository.FindClaimByID(ctx, pool, cUsed.ClaimID.Int64())
	if u.Status != task.ClaimUsed {
		t.Fatalf("used claim status = %s", u.Status)
	}
	r, _ := repository.FindClaimByID(ctx, pool, cRel.ClaimID.Int64())
	if r.Status != task.ClaimReleased {
		t.Fatalf("released claim status = %s", r.Status)
	}
	claimTaskReserved(t, pool, tKeep)
	claimTaskReserved(t, pool, tUsed)
	claimTaskReserved(t, pool, tRel)
}

func mustTaskID(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	tr, err := repository.FindTaskByCode(context.Background(), pool, code)
	if err != nil || tr == nil {
		t.Fatalf("task %s: %v", code, err)
	}
	return tr.ID
}
