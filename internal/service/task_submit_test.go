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
	if first.State != task.SubSettled || first.Amount != 5 || first.Version != 1 || first.Paid != 5 {
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

	draft := pubCreateForTest(t, pool, publisher, submitContract(), 1000)
	_, err := submitOnce(t, pool, agent, draft, nil)
	appErr := appErrOf(t, err)
	if appErr.Code != "TASK_NOT_OPEN" || appErr.Details["status"] != task.TaskDraft {
		t.Fatalf("draft: %v (%#v), want TASK_NOT_OPEN/draft", err, appErr.Details)
	}
	submitState(t, pool, draft, err, "TASK_NOT_OPEN")

	closed := submitOpenedTask(t, pool, publisher, 1000, nil)
	if _, err := CloseTask(ctx, pool, publisher, closed); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err = submitOnce(t, pool, agent, closed, nil)
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
	if n != 1 || tr.Reserved != 1000 {
		t.Fatalf("n = %d reserved = %d, want 1/1000", n, tr.Reserved)
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
	if err := task.CheckInvariants(ctx, pool, mustTaskID(t, pool, rejected)); err != nil {
		t.Fatalf("CheckInvariants: %v", err)
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
	if view.Version != 1 || view.Amount != 5 || view.State != task.SubSettled || view.Paid != 5 {
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

func TestSubmitClaimVersionPinned(t *testing.T) {
	pool := pubTestPool(t)
	publisher := pubSeedBot(t, pool, 10_000)
	agent := pubSeedBot(t, pool, 0)
	ctx := context.Background()
	now := time.Now()

	code := claimOpenedTask(t, pool, publisher, 1000, nil)
	claim := claimOnTask(t, pool, agent, code, now)

	// v2 tightens the schema (requires an extra field); the v1 claim
	// keeps the submission on the v1 schema.
	if _, err := PauseTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("pause: %v", err)
	}
	updated := claimContract()
	updated.Output.Schema = []byte(`{
		"type": "object",
		"properties": {
			"url": {"type": "string"},
			"lang": {"type": "string"},
			"bullets": {"type": "array", "items": {"type": "string"}, "minItems": 3, "maxItems": 3}
		},
		"required": ["url", "lang", "bullets"]
	}`)
	updated.Sample = []byte(`{"url":"https://example.com/a","lang":"en","bullets":["s1","s2","s3"]}`)
	updated.Claim = task.ClaimConfig{} // v2 does not require a claim
	if _, err := UpdateTask(ctx, pool, publisher, code, updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := OpenTask(ctx, pool, publisher, code); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// the v1 payload has no "lang": valid for v1, invalid for v2
	view, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) { in.ClaimID = &claim.ClaimID })
	if err != nil {
		t.Fatalf("submit pinned to v1: %v", err)
	}
	if view.Version != 1 {
		t.Fatalf("submission version = %d, want 1 (claim's version)", view.Version)
	}
	claimTaskReserved(t, pool, code)

	// without the claim the same payload now fails the v2 schema
	_, err = submitOnce(t, pool, agent, code, nil)
	if appErrOf(t, err).Code != "SCHEMA_MISMATCH" {
		t.Fatalf("current-version submit: %v, want SCHEMA_MISMATCH", err)
	}
}

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
		TaskID: tr.ID, Version: 1, AgentID: agent, RequestKey: key,
		Payload: []byte(submitPayloadOK), PayloadHash: task.PayloadHash([]byte(submitPayloadOK)),
		Amount: 5,
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
		c.Output = task.Output{}
		c.Sample = []byte(`{"anything":"goes"}`)
	})
	view, err := submitOnce(t, pool, agent, code, func(in *SubmitInput) {
		in.Payload = []byte(`{"free":"form"}`)
	})
	if err != nil || view.State != task.SubSettled {
		t.Fatalf("submit without schema: %+v, %v", view, err)
	}
	claimTaskReserved(t, pool, code)
}
