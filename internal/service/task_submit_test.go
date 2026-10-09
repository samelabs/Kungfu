package service

// Submission intake tests (WO-4) — real PostgreSQL, spec §5.3 steps
// a–h one failure each, priority combos, idempotency, claim usage and
// version pinning, pointer-level schema errors, concurrency. Every
// test ends with task.CheckInvariants.

import (
	"context"
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

const submitPayloadOK = `{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`

// submitContract: the accept-everything receiver, claim NOT required,
// price 5 — a submission settles on delivery.
func submitContract() task.Contract {
	c := claimContract()
	c.Claim = task.ClaimConfig{}
	return c
}

func submitOpenedTask(t *testing.T, pool *pg.Pool, publisher int64, budget int64, mutate func(*task.Contract)) string {
	t.Helper()
	c := submitContract()
	if mutate != nil {
		mutate(&c)
	}
	code := pubCreateForTest(t, pool, publisher, c, budget)
	if _, err := OpenTask(context.Background(), pool, publisher, code); err != nil {
		t.Fatalf("open: %v", err)
	}
	return code
}

func submitOnce(t *testing.T, pool *pg.Pool, agent int64, code string, mutate func(*SubmitInput)) (SubmissionView, error) {
	t.Helper()
	in := SubmitInput{Code: code, RequestKey: fmt.Sprintf("k-%d", time.Now().UnixNano()), Payload: []byte(submitPayloadOK)}
	if mutate != nil {
		mutate(&in)
	}
	return SubmitWork(context.Background(), pool, agent, in, testAgentRefKey, time.Now())
}

// testAgentRefKey keys the anonymous agent_ref in tests.
var testAgentRefKey = []byte("wo5a-agent-ref-key")

// submitState asserts the error code and that nothing was written.
func submitState(t *testing.T, pool *pg.Pool, code string, err error, wantCode string) {
	t.Helper()
	if appErrOf(t, err).Code != wantCode {
		t.Fatalf("code = %v, want %s", err, wantCode)
	}
	tr, _ := repository.FindTaskByCode(context.Background(), pool, code)
	var n int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_task_submission WHERE task_id = $1`, tr.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("submissions = %d, want 0 after a rejected intake", n)
	}
	if tr.Reserved != 0 {
		t.Fatalf("reserved = %d, want 0 after a rejected intake", tr.Reserved)
	}
	if err := task.CheckInvariants(context.Background(), pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- (a) format and size, before any query --

func TestSubmitInvalidRequestKey(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	_, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.RequestKey = "bad key!" })
	submitState(t, pool, code, err, "INVALID_REQUEST_KEY")

	// priority: the format error fires even for a nonexistent task
	_, err = SubmitWork(context.Background(), pool, agent, SubmitInput{
		Code: "nope00000000", RequestKey: "bad key!", Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if appErrOf(t, err).Code != "INVALID_REQUEST_KEY" {
		t.Fatalf("nonexistent task with a bad key: %v, want INVALID_REQUEST_KEY", err)
	}
}

func TestSubmitPayloadTooLarge(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	huge := append([]byte(`{"url":"https://example.com/a","bullets":["`), strings.Repeat("x", 512*1024)...)
	_, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.Payload = huge })
	submitState(t, pool, code, err, "PAYLOAD_TOO_LARGE")
}

// -- (b) idempotency --

func TestSubmitIdempotency(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	first, err := submitOnce(t, pool, agent, code, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if first.State != task.SubSettled || first.Amount != 5 || first.Paid != 5 {
		t.Fatalf("view = %+v", first)
	}

	key := fmt.Sprintf("idem-%d", time.Now().UnixNano())
	in := SubmitInput{Code: code, RequestKey: key, Payload: []byte(submitPayloadOK)}
	a, err := SubmitWork(ctx, pool, agent, in, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit a: %v", err)
	}
	// same key, same payload, different field order
	b, err := SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: key,
		Payload: []byte(`{"bullets":["s1","s2","s3"],"url":"https://example.com/a"}`),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit b: %v", err)
	}
	if a.SubmissionID != b.SubmissionID {
		t.Fatalf("idempotent retry created a new submission: %d vs %d", a.SubmissionID.Int64(), b.SubmissionID.Int64())
	}

	// the same key still returns the original after the task closes
	if _, err := CloseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("close: %v", err)
	}
	c, err := SubmitWork(ctx, pool, agent, in, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("closed-task retry: %v", err)
	}
	if c.SubmissionID != a.SubmissionID {
		t.Fatalf("closed-task retry returned %d, want the original %d", c.SubmissionID.Int64(), a.SubmissionID.Int64())
	}

	// same key, different payload → conflict (priority: idempotency is
	// checked before the task-closed failure this submission would hit)
	_, err = SubmitWork(ctx, pool, agent, SubmitInput{
		Code: code, RequestKey: key,
		Payload: []byte(`{"url":"https://example.com/other","bullets":["s1","s2","s3"]}`),
	}, testAgentRefKey, time.Now())
	submitErr := appErrOf(t, err)
	if submitErr.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("code = %v, want IDEMPOTENCY_CONFLICT", err)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, code)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- (c) task guards --

func TestSubmitTaskNotFound(t *testing.T) {
	pool := pubTestPool(t)
	agent := pubSeedBot(t, pool, 0)
	_, err := SubmitWork(context.Background(), pool, agent, SubmitInput{
		Code: "nope00000000", RequestKey: "k-404", Payload: []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if appErrOf(t, err).Code != "TASK_NOT_FOUND" {
		t.Fatalf("code = %v, want TASK_NOT_FOUND", err)
	}
}

func TestSubmitTaskNotOpen(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	closed := submitOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := CloseTask(ctx, pool, publisher, closed); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := submitOnce(t, pool, agent, closed, nil)
	submitState(t, pool, closed, err, "TASK_NOT_OPEN")
}

func TestSubmitSlotsExhausted(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	a1 := pubSeedBot(t, pool, 0)
	a2 := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	// price = budget → one slot
	code := submitOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) { c.Price = 1000 })
	if _, err := submitOnce(t, pool, a1, code, nil); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	_, err := submitOnce(t, pool, a2, code, nil)
	if appErrOf(t, err).Code != "SLOTS_EXHAUSTED" {
		t.Fatalf("code = %v, want SLOTS_EXHAUSTED", err)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	var n int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tb_task_submission WHERE task_id = $1`, tr.ID).Scan(&n)
	// the first submission settled on the receiver's 200: its unit is
	// spent, so no slot is left
	if n != 1 || tr.Settled != 1000 || tr.Reserved != 0 {
		t.Fatalf("n = %d settled = %d reserved = %d, want 1/1000/0", n, tr.Settled, tr.Reserved)
	}
	if err := task.CheckInvariants(ctx, pool, tr.ID); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}
}

// -- (d)+(e) --

func TestSubmitOwnTask(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	_, err := submitOnce(t, pool, publisher, code, nil)
	submitState(t, pool, code, err, "OWN_TASK")
}

func TestSubmitSubmissionLimit(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	rejected := submitOpenedTask(t, pool, publisher, 1000, nil)
	for i := 0; i < 5; i++ { // 缺省 5
		seedSubmission(t, pool, rejected, agent, task.SubRejected)
	}
	_, err := submitOnce(t, pool, agent, rejected, nil)
	appErr := appErrOf(t, err)
	if appErr.Code != "SUBMISSION_LIMIT" || appErr.Details["limit"] != "rejected" {
		t.Fatalf("%v (%#v), want SUBMISSION_LIMIT/rejected", err, appErr.Details)
	}
	// the limit counts the last 24 hours and says when one frees up
	wait, _ := appErr.Details["retry_after"].(int)
	if appErr.Details["rejected_24h"] != int64(5) || appErr.Details["max"] != int64(5) ||
		appErr.Details["window_hours"] != task.RejectionWindowHours || wait < 86000 || wait > 86400 ||
		appErr.Details["retry_after_at"] == "" {
		t.Fatalf("details = %#v, want rejected_24h 5, max 5, window 24h, retry_after ≈ 86400", appErr.Details)
	}
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, rejected)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
	}

	// 25 hours later the same five rejections have aged out of the
	// window: the agent may submit again
	later := time.Now().Add(25 * time.Hour)
	in := SubmitInput{Code: rejected, RequestKey: fmt.Sprintf("k-later-%d", time.Now().UnixNano()), Payload: []byte(submitPayloadOK)}
	if _, err := SubmitWork(ctx, pool, agent, in, testAgentRefKey, later); err != nil {
		if ae, ok := errors.IsAppError(err); ok && ae.Code == "SUBMISSION_LIMIT" {
			t.Fatalf("rejections older than 24h still count: %v", err)
		}
	}
}

// -- (f) claim --

func claimOnTask(t *testing.T, pool *pg.Pool, agent int64, code string, now time.Time) claimView {
	t.Helper()
	claim, err := ClaimTask(context.Background(), pool, agent, code, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return claim
}

func TestSubmitClaimRequired(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	// claimAsyncContract already has claim.required = true
	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	_, err := submitOnce(t, pool, agent, code, nil)
	submitState(t, pool, code, err, "CLAIM_REQUIRED")
}

func TestSubmitClaimInvalid(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)

	// someone else's claim (it legitimately holds a reservation)
	foreign := claimOnTask(t, pool, stranger, code, now)
	reservedBefore := claimTaskReserved(t, pool, code)
	_, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.ClaimID = &foreign.ClaimID })
	if appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("foreign claim: %v, want CLAIM_INVALID", err)
	}
	if got := claimTaskReserved(t, pool, code); got != reservedBefore {
		t.Fatalf("reserved = %d, want unchanged %d", got, reservedBefore)
	}

	// a claim on a different task
	other := claimOpenedTask(t, pool, publisher, 1000, nil)
	otherClaim := claimOnTask(t, pool, agent, other, now)
	_, err = submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.ClaimID = &otherClaim.ClaimID })
	if appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("cross-task claim: %v, want CLAIM_INVALID", err)
	}

	// expired claim (ttl 300 claimed two hours ago)
	t0 := now.Add(-2 * time.Hour)
	expiredTask := claimOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) {
		ttl := int64(300)
		c.Claim.TTL = &ttl
	})
	expired := claimOnTask(t, pool, agent, expiredTask, t0)
	_, err = submitOnce(t, pool, agent, expiredTask, func(in *SubmitInput) { in.ClaimID = &expired.ClaimID })
	if appErrOf(t, err).Code != "CLAIM_INVALID" {
		t.Fatalf("expired claim: %v, want CLAIM_INVALID", err)
	}
	claimTaskReserved(t, pool, code)
	claimTaskReserved(t, pool, expiredTask)
	_ = ctx
}

func TestSubmitWithClaim(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim := claimOnTask(t, pool, agent, code, now)
	reservedBefore := claimTaskReserved(t, pool, code)

	view, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.ClaimID = &claim.ClaimID })
	if err != nil {
		t.Fatalf("submit with claim: %v", err)
	}
	// the receiver answers 200: settled and paid
	if view.Amount != 5 || view.State != task.SubSettled || view.Paid != 5 {
		t.Fatalf("view = %+v", view)
	}
	after, _ := repository.FindClaimByID(ctx, pool, claim.ClaimID.Int64())
	if after.Status != task.ClaimUsed {
		t.Fatalf("claim status = %s, want used", after.Status)
	}
	// the claim's reservation transferred to the submission (no new
	// one) and was consumed by the settlement
	if got := claimTaskReserved(t, pool, code); got != reservedBefore-5 {
		t.Fatalf("reserved = %d, want %d", got, reservedBefore-5)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, view.SubmissionID.Int64())
	if sub.ClaimID == nil || *sub.ClaimID != claim.ClaimID.Int64() {
		t.Fatalf("submission claim_id = %v, want %d", *sub.ClaimID, claim.ClaimID.Int64())
	}
}

func TestSubmitWithClaimOnPausedAndClosedTasks(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	paused := claimOpenedTask(t, pool, publisher, 1000, nil)
	claimP := claimOnTask(t, pool, agent, paused, now)
	if _, err := PauseTask(ctx, pool, publisher, paused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := submitOnce(t, pool, agent, paused, func(in *SubmitInput) { in.ClaimID = &claimP.ClaimID }); err != nil {
		t.Fatalf("submit on paused with claim: %v", err)
	}

	closed := claimOpenedTask(t, pool, publisher, 1000, nil)
	claimC := claimOnTask(t, pool, agent, closed, now)
	if _, err := CloseTask(ctx, pool, publisher, closed); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := submitOnce(t, pool, agent, closed, func(in *SubmitInput) { in.ClaimID = &claimC.ClaimID }); err != nil {
		t.Fatalf("submit on closed with claim: %v", err)
	}
	claimTaskReserved(t, pool, paused)
	claimTaskReserved(t, pool, closed)
}

// -- version pinning --

// -- (g) revises --

func TestSubmitRevisesInvalid(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	stranger := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	countSubs := func() int64 {
		var n int64
		_ = pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tb_task_submission WHERE task_id = $1`, mustTaskID(t, pool, code)).Scan(&n)
		return n
	}

	// a DELIVERING (not rejected) target
	delivering := seedSubmissionReturningID(t, pool, code, agent, task.SubDelivering, nil)
	before := countSubs()
	_, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.Revises = WireIDPtr(&delivering) })
	if appErrOf(t, err).Code != "INVALID_REVISES" {
		t.Fatalf("delivering target: %v, want INVALID_REVISES", err)
	}
	if countSubs() != before {
		t.Fatalf("submissions = %d, want %d (no new rows)", countSubs(), before)
	}

	// someone else's rejected submission
	foreignRejected := seedSubmissionReturningID(t, pool, code, stranger, task.SubRejected, nil)
	_, err = submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.Revises = WireIDPtr(&foreignRejected) })
	if appErrOf(t, err).Code != "INVALID_REVISES" {
		t.Fatalf("foreign rejected target: %v, want INVALID_REVISES", err)
	}

	if countSubs() != 2 {
		t.Fatalf("submissions = %d, want 2 (seeded only)", countSubs())
	}
	claimTaskReserved(t, pool, code)
	_ = stranger
	_ = ctx
}

// fakeCredential builds a kf_live_-shaped string (the credential
// shape internal/security detects).
func fakeCredential() string {
	return "kf_live_" + strings.Repeat("ab", 32)
}

// seedSubmissionReturningID seeds a submission in the given terminal
// (or delivering) state and returns its id.
func seedSubmissionReturningID(t *testing.T, pool *pg.Pool, code string, agent int64, state string, opts *repository.SetSubmissionStateOpts) int64 {
	t.Helper()
	ctx := context.Background()
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pg.Rollback(tx) }()
	key := fmt.Sprintf("rev-%s-%d-%d", code, agent, time.Now().UnixNano())
	subID, err := repository.InsertSubmission(ctx, tx, repository.NewSubmissionRow{
		TaskID: tr.ID, AgentID: agent, RequestKey: key,
		Payload: []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)),
		Amount: 5, ContractVersion: tr.ContractVersion,
	})
	if err != nil {
		t.Fatalf("insert submission: %v", err)
	}
	switch state {
	case task.SubDelivering:
		// a live intake reserves its price (§10.2)
		if err := repository.ReserveTaskAmount(ctx, tx, tr.ID, 5); err != nil {
			t.Fatalf("reserve seeded submission: %v", err)
		}
	case task.SubRejected:
		if err := repository.SetSubmissionState(ctx, tx, subID, task.SubDelivering, task.EventDeliver4XX, opts); err != nil {
			t.Fatalf("reject: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return subID
}

// -- (h) payload checks --

func TestSubmitSchemaMismatchPointer(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	code := submitOpenedTask(t, pool, publisher, 1000, nil)

	// two bullets violates minItems 3 at /bullets
	_, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) {
		in.Payload = []byte(`{"url":"https://example.com/a","bullets":["s1","s2"]}`)
	})
	appErr := appErrOf(t, err)
	if appErr.Code != "SCHEMA_MISMATCH" {
		t.Fatalf("code = %v, want SCHEMA_MISMATCH", err)
	}
	items, _ := appErr.Details["errors"].([]map[string]string)
	if len(items) == 0 {
		t.Fatalf("details.errors = %#v", appErr.Details)
	}
	if ptr := items[0]["pointer"]; !strings.HasPrefix(ptr, "/") || ptr != "/bullets" {
		t.Fatalf("first error pointer = %q, want /bullets", ptr)
	}
	// a wrong type points at the field itself
	_, err = submitOnce(t, pool, agent, code, func(in *SubmitInput) {
		in.Payload = []byte(`{"url":42,"bullets":["s1","s2","s3"]}`)
	})
	items, _ = appErrOf(t, err).Details["errors"].([]map[string]string)
	if len(items) == 0 || items[0]["pointer"] != "/url" {
		t.Fatalf("type error pointer = %#v, want /url", appErrOf(t, err).Details)
	}
	// non-object payload
	_, err = submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.Payload = []byte(`[1,2,3]`) })
	if appErrOf(t, err).Code != "SCHEMA_MISMATCH" {
		t.Fatalf("non-object: %v, want SCHEMA_MISMATCH", err)
	}
	submitState(t, pool, code, err, "SCHEMA_MISMATCH")
}

func TestSubmitCredentialPointer(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	_, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) {
		in.Payload = []byte(`{"url":"https://example.com/a","bullets":["s1","s2","` + fakeCredential() + `"]}`)
	})
	appErr := appErrOf(t, err)
	if appErr.Code != "CREDENTIAL_IN_PAYLOAD" {
		t.Fatalf("code = %v, want CREDENTIAL_IN_PAYLOAD", err)
	}
	if ptr, _ := appErr.Details["pointer"].(string); ptr != "/bullets/2" {
		t.Fatalf("details.pointer = %#v, want /bullets/2", appErr.Details["pointer"])
	}
	submitState(t, pool, code, err, "CREDENTIAL_IN_PAYLOAD")
}

// -- concurrency --

func TestSubmitConcurrentSameKey(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	key := fmt.Sprintf("conc-%d", time.Now().UnixNano())
	in := SubmitInput{Code: code, RequestKey: key, Payload: []byte(submitPayloadOK)}

	var wg sync.WaitGroup
	views := make([]SubmissionView, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			views[i], errs[i] = SubmitWork(ctx, pool, agent, in, testAgentRefKey, time.Now())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent submit %d: %v", i, err)
		}
	}
	if views[0].SubmissionID != views[1].SubmissionID {
		t.Fatalf("concurrent twins diverged: %d vs %d", views[0].SubmissionID, views[1].SubmissionID)
	}
	tr, _ := repository.FindTaskByCode(ctx, pool, code)
	var n int64
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tb_task_submission WHERE task_id = $1 AND request_key = $2`, tr.ID, key).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	claimTaskReserved(t, pool, code)
}

// §3: output.schema is optional — without one, any JSON object is
// delivered and the receiver's reply decides.
func TestSubmitWithoutSchema(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	code := submitOpenedTask(t, pool, publisher, 1000, func(c *task.Contract) {
		c.Output = task.Output{} // no schema: free-form payloads settle
	})
	view, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) {
		in.Payload = []byte(`{"free":"form"}`)
	})
	if err != nil || view.State != task.SubSettled {
		t.Fatalf("submit without schema: %+v, %v", view, err)
	}
	claimTaskReserved(t, pool, code)
}

// TestSubmitPayloadNullRejected: JSON null unmarshals into a nil map
// without error — it must be rejected as SCHEMA_MISMATCH before any
// submission is created.
func TestSubmitPayloadNullRejected(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)

	code := submitOpenedTask(t, pool, publisher, 1000, nil)
	_, err := SubmitWork(context.Background(), pool, agent, SubmitInput{
		Code:       code,
		RequestKey: fmt.Sprintf("null-%d", time.Now().UnixNano()),
		Payload:    []byte("null"),
	}, testAgentRefKey, time.Now())
	submitState(t, pool, code, err, "SCHEMA_MISMATCH")
}

// TestSubmitOwnTaskPrecedesClaimParsing (T4): §5.3 order — OWN_TASK is
// step 4, claim parsing step 6. A publisher probing its own task with
// a bogus claim_id hears OWN_TASK, never CLAIM_INVALID.
func TestSubmitOwnTaskPrecedesClaimParsing(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	code := submitOpenedTask(t, pool, publisher, 1000, nil)

	bogus := WireID(999_999_999)
	_, err := submitOnce(t, pool, publisher, code, func(in *SubmitInput) { in.ClaimID = &bogus })
	submitState(t, pool, code, err, "OWN_TASK")
}
