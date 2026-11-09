package service

// Room disposal tests (migration 035): dissolution settles and
// hard-deletes the aggregate, payload memories follow their last
// reference, cross-room pins survive, scrub redacts an author's own
// content without breaking others' references, and the retention
// sweep only touches closed rooms past the cutoff.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/delivery"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/task"
)

func disposeRoom(t *testing.T, pool *pg.Pool, owner, worker int64, code string) {
	t.Helper()
	res, err := ThreadDissolve(context.Background(), pool, owner, code, "")
	if err != nil {
		t.Fatalf("dissolve: %v", err)
	}
	if res["disposed"] != true {
		t.Fatalf("dissolve result = %v", res)
	}
}

func countRow(t *testing.T, pool *pg.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestThreadDissolveDisposesEverything(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "dispose", true, "")
	defer cleanupDisposed(t, pool, code)

	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	// an entry with payload + an ask, and a delivered assignment whose
	// judgment clock is still running when the hammer falls
	if _, err := ThreadPost(ctx, pool, owner, code, "payload body", "", "with ask", nil, []int64{w}, nil, "d1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "do it", "", "assign w", nil, []int64{},
		&AssignSpec{To: w, Requirements: "r"}, "d2"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	assignID := int64(countRow(t, pool, `SELECT id FROM assigns WHERE thread_id = (SELECT id FROM threads WHERE code=$1)`, code))
	if _, err := AssignTake(ctx, pool, w, assignID, `{"ok":true}`, "", "d3"); err != nil {
		t.Fatalf("take+deliver: %v", err)
	}

	var entries, payloadMem, assigns int
	entries = countRow(t, pool, `SELECT count(*) FROM thread_entries e JOIN threads t ON t.id=e.thread_id WHERE t.code=$1`, code)
	payloadMem = countRow(t, pool, `SELECT count(*) FROM tb_kungfus k WHERE k.origin='thread' AND k.bot_id=$1`, owner)
	assigns = countRow(t, pool, `SELECT count(*) FROM assigns WHERE id=$1`, assignID)
	if entries != 2 || payloadMem != 2 || assigns != 1 {
		t.Fatalf("seed: entries=%d payloads=%d assigns=%d", entries, payloadMem, assigns)
	}

	disposeRoom(t, pool, owner, w, code)

	if n := countRow(t, pool, `SELECT count(*) FROM threads WHERE code=$1`, code); n != 0 {
		t.Fatal("thread row survived dissolution")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM thread_entries WHERE thread_id NOT IN (SELECT id FROM threads)`); n != 0 {
		t.Fatal("orphaned entries survived dissolution")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM assigns WHERE id=$1`, assignID); n != 0 {
		t.Fatal("assign survived dissolution")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE origin='thread' AND bot_id=$1`, owner); n != 0 {
		t.Fatal("payload memories survived dissolution")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM thread_disposals WHERE code=$1 AND actor_kind='governor'`, code); n != 1 {
		t.Fatal("audit row missing")
	}
	// the turn projection is clean for both parties
	for _, bot := range []int64{owner, w} {
		todo, err := TodoList(ctx, pool, bot, 0, "")
		if err != nil {
			t.Fatalf("todo: %v", err)
		}
		if n := len(todo["todos"].([]map[string]any)); n != 0 {
			t.Fatalf("bot %d still owes %d after dissolution", bot, n)
		}
	}
	// dissolved room answers THREAD_NOT_FOUND
	if _, err := ThreadDissolve(ctx, pool, owner, code, "d-again"); err == nil {
		t.Fatal("second dissolve must fail")
	}
}

func TestThreadDissolveKeepsCrossRoomPayload(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	codeA, _ := threadStart(t, pool, owner, "a", true, "")
	codeB, _ := threadStart(t, pool, owner, "b", true, "")
	defer cleanupDisposed(t, pool, codeA)
	defer cleanupDisposed(t, pool, codeB)

	// one payload memory, pinned by entries in two rooms
	if _, err := ThreadPost(ctx, pool, owner, codeA, "shared payload", "", "", nil, nil, nil, "x1"); err != nil {
		t.Fatalf("postA: %v", err)
	}
	memCode := firstPayloadCode(t, pool, owner)
	if _, err := ThreadPost(ctx, pool, owner, codeB, "", memCode, "", nil, nil, nil, "x2"); err != nil {
		t.Fatalf("postB pin: %v", err)
	}

	disposeRoom(t, pool, owner, w, codeA)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1 AND origin='thread'`, memCode); n != 1 {
		t.Fatal("payload pinned by room B must survive room A's dissolution")
	}
	disposeRoom(t, pool, owner, w, codeB)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload must follow its last reference into disposal")
	}
}

func TestThreadDissolveRequiresGovernor(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "perm", true, "")
	defer cleanupDisposed(t, pool, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadDissolve(ctx, pool, w, code, ""); err == nil {
		t.Fatal("non-governor dissolve must be rejected")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM threads WHERE code=$1`, code); n != 1 {
		t.Fatal("rejected dissolve must leave the room intact")
	}
}

func TestThreadScrubRedactsOwnEntry(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "scrub", true, "")
	defer cleanupDisposed(t, pool, code)
	if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "secret content", "", "the summary", nil, []int64{w}, nil, "s1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	entryID := countRow(t, pool, `SELECT e.id FROM thread_entries e JOIN threads t ON t.id=e.thread_id WHERE t.code=$1 ORDER BY e.id DESC LIMIT 1`, code)

	// before scrub the same id expands with content (own pin: readable)
	pre, err := ThreadGet(ctx, pool, owner, code, "", []int64{int64(entryID)}, "", nil, false)
	if err != nil {
		t.Fatalf("expand pre-scrub: %v", err)
	}
	preRows := pre["entries"].([]any)
	if len(preRows) != 1 {
		t.Fatalf("pre-scrub expand returned %d rows, want 1", len(preRows))
	}
	preRow := preRows[0].(map[string]any)
	if preRow["redacted"] != false || preRow["readable"] != true || preRow["content"] != "secret content" {
		t.Fatalf("pre-scrub expand = %v", preRow)
	}

	// a non-author cannot scrub
	if _, err := ThreadScrubEntry(ctx, pool, w, code, int64(entryID), ""); err == nil {
		t.Fatal("non-author scrub must be rejected")
	}
	res, err := ThreadScrubEntry(ctx, pool, owner, code, int64(entryID), "s2")
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if res["redacted"] != true || res["payload_memory_removed"] != true {
		t.Fatalf("scrub result = %v", res)
	}
	// shell survives; content gone
	var summary *string
	var memory *int64
	if err := pool.QueryRow(ctx, `SELECT summary, memory_id FROM thread_entries WHERE id=$1`, entryID).Scan(&summary, &memory); err != nil {
		t.Fatalf("entry row: %v", err)
	}
	if summary != nil || memory != nil {
		t.Fatalf("scrubbed entry still carries content: summary=%v memory=%v", summary, memory)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM thread_entries WHERE id=$1`, entryID); n != 1 {
		t.Fatal("entry shell must survive scrub")
	}
	// the pending receipt toward the scrubbed entry still resolves
	if todo, err := TodoList(ctx, pool, w, 0, ""); err != nil || len(todo["todos"].([]map[string]any)) != 1 {
		t.Fatalf("receipt must survive scrub: %v %v", todo, err)
	}
	// the scrubbed entry still expands — shell facts only, never
	// content (external audit P1-02)
	post, err := ThreadGet(ctx, pool, w, code, "", []int64{int64(entryID)}, "", nil, false)
	if err != nil {
		t.Fatalf("expand post-scrub: %v", err)
	}
	postRows := post["entries"].([]any)
	if len(postRows) != 1 {
		t.Fatalf("post-scrub expand returned %d rows, want 1", len(postRows))
	}
	postRow := postRows[0].(map[string]any)
	if postRow["redacted"] != true || postRow["readable"] != false {
		t.Fatalf("post-scrub expand = %v", postRow)
	}
	if _, has := postRow["content"]; has {
		t.Fatalf("scrubbed entry leaked content: %v", postRow)
	}
	if postRow["seq"] != preRow["seq"] || postRow["author"] != preRow["author"] {
		t.Fatalf("shell must survive scrub: %v vs %v", postRow, preRow)
	}
	replyTo := int64(entryID)
	if _, err := ThreadPost(ctx, pool, w, code, "replied anyway", "", "", &replyTo, []int64{}, nil, "s3"); err != nil {
		t.Fatalf("reply to scrubbed entry: %v", err)
	}
	// re-scrub with a FRESH idem key (a new logical request, no replay
	// receipt) must stay idempotent: the redacted entry's memory_id is
	// NULL (migration 035), the entry has nothing left to remove —
	// redacted:true again, never an error
	res2, err := ThreadScrubEntry(ctx, pool, owner, code, int64(entryID), "s2-again")
	if err != nil {
		t.Fatalf("re-scrub: %v", err)
	}
	if res2["redacted"] != true || res2["payload_memory_removed"] != false {
		t.Fatalf("re-scrub result = %v", res2)
	}
	// a missing entry is NOT_FOUND, not an internal error
	_, err = ThreadScrubEntry(ctx, pool, owner, code, 1<<40, "s4")
	if ae, ok := errors.IsAppError(err); !ok || ae.Code != "NOT_FOUND" {
		t.Fatalf("scrub of a missing entry = %v, want NOT_FOUND", err)
	}
}

func TestRetentionSweepDisposesOnlyOldClosed(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	codeClosedOld, rawA := threadStart(t, pool, owner, "old", true, "")
	codeOpen, rawB := threadStart(t, pool, owner, "open", true, "")
	defer cleanupDisposed(t, pool, codeClosedOld)
	defer cleanupDisposed(t, pool, codeOpen)
	_ = rawA
	if _, err := ThreadClose(ctx, pool, owner, codeClosedOld, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	// backdate the closed room past the cutoff
	if _, err := pool.Exec(ctx, `UPDATE threads SET closed_at = NOW() - INTERVAL '30 days' WHERE code=$1`, codeClosedOld); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	_ = rawB
	n, err := RetentionSweep(ctx, pool, 7)
	if err != nil || n != 1 {
		t.Fatalf("sweep disposed %d (err=%v), want 1", n, err)
	}
	if gone := countRow(t, pool, `SELECT count(*) FROM threads WHERE code=$1`, codeClosedOld); gone != 0 {
		t.Fatal("old closed room survived the sweep")
	}
	if alive := countRow(t, pool, `SELECT count(*) FROM threads WHERE code=$1`, codeOpen); alive != 1 {
		t.Fatal("open room must never be swept")
	}
	// the audit row is metadata-only; the sweep has no actor identity,
	// so actor_ref is NULL (the column default), not a placeholder
	if n := countRow(t, pool,
		`SELECT count(*) FROM thread_disposals WHERE code=$1 AND actor_kind='retention' AND actor_ref IS NULL`,
		codeClosedOld); n != 1 {
		t.Fatal("retention audit row must carry actor_ref NULL")
	}
	// zero policy = no-op
	if n, _ := RetentionSweep(ctx, pool, 0); n != 0 {
		t.Fatal("retention 0 must be a no-op")
	}
}

// -- cross-object references: a payload's life outlives its room when
// live work elsewhere still needs it (external audit P1-01) --

// harnessTaskOnPayload posts one payload memory in the room and hangs
// an open task whose contract takes that memory as its harness input.
func harnessTaskOnPayload(t *testing.T, pool *pg.Pool, owner int64, code string) (string, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := ThreadPost(ctx, pool, owner, code, "harness payload", "", "", nil, nil, nil, "h1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	memCode := firstPayloadCode(t, pool, owner)
	c := pubContract("")
	c.HarnessRefs = []string{memCode}
	taskCode := pubCreateForTest(t, pool, owner, c, 5)
	if _, err := OpenTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("open: %v", err)
	}
	return memCode, taskCode
}

func TestDissolveKeepsHarnessReferencedPayload(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "harn", true, "")
	defer cleanupDisposed(t, pool, code)
	memCode, taskCode := harnessTaskOnPayload(t, pool, owner, code)

	disposeRoom(t, pool, owner, w, code)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 1 {
		t.Fatal("payload referenced by a live task contract must survive dissolution")
	}
	// a closed task can never reopen: the protection ends, the sweep
	// finishes what disposal deferred
	if _, err := CloseTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("close task: %v", err)
	}
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload of a closed, unclaimed task must be reclaimed by the sweep")
	}
}

func TestDissolveKeepsActiveClaimPin(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "pin", true, "")
	defer cleanupDisposed(t, pool, code)
	memCode, taskCode := harnessTaskOnPayload(t, pool, owner, code)

	if _, err := ClaimTask(ctx, pool, w, taskCode, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM claim_harness_revisions p
		JOIN tb_kungfus k ON k.id = p.memory_id WHERE k.code=$1`, memCode); n != 1 {
		t.Fatal("claim must pin the harness payload")
	}
	disposeRoom(t, pool, owner, w, code)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 1 {
		t.Fatal("payload pinned by a live engagement must survive dissolution")
	}
	claimID := countRow(t, pool, `SELECT claim_id FROM tb_task_claim c
		JOIN tb_task t ON t.id = c.task_id WHERE t.code=$1`, taskCode)
	// the closed task can never reopen; its still-active claim keeps
	// the pin alive until the engagement itself ends
	if _, err := CloseTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("close task: %v", err)
	}
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 1 {
		t.Fatal("payload pinned by the still-active claim of a closed task must survive")
	}
	if _, err := ReleaseClaim(ctx, pool, w, int64(claimID), time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload of an ended engagement must be reclaimed by the sweep")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM claim_harness_revisions p
		JOIN tb_kungfus k ON k.id = p.memory_id WHERE k.code=$1`, memCode); n != 0 {
		t.Fatal("dead pins must not survive their payload")
	}
}

func TestDissolveWithTerminalClaimDropsDeadPins(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "dead", true, "")
	defer cleanupDisposed(t, pool, code)
	memCode, taskCode := harnessTaskOnPayload(t, pool, owner, code)

	if _, err := ClaimTask(ctx, pool, w, taskCode, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimID := countRow(t, pool, `SELECT claim_id FROM tb_task_claim c
		JOIN tb_task t ON t.id = c.task_id WHERE t.code=$1`, taskCode)
	if _, err := ReleaseClaim(ctx, pool, w, int64(claimID), time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}
	memID := countRow(t, pool, `SELECT id FROM tb_kungfus WHERE code=$1`, memCode)
	if _, err := CloseTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("close task: %v", err)
	}
	// the ended engagement's pin rows must not turn this dissolve into
	// an FK failure — the pins go with the payload, same transaction
	disposeRoom(t, pool, owner, w, code)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload with only dead pins must follow its room")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM claim_harness_revisions WHERE memory_id=$1`, memID); n != 0 {
		t.Fatal("dead pins must be dropped with the payload")
	}
}

func TestDissolveKeepsUnjudgedDeliveryPayload(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	codeSrc, rawSrc := threadStart(t, pool, owner, "src", true, "")
	codeWork, rawWork := threadStart(t, pool, owner, "work", true, "")
	defer cleanupDisposed(t, pool, codeSrc)
	defer cleanupDisposed(t, pool, codeWork)
	for _, raw := range []string{rawSrc, rawWork} {
		if _, err := ThreadJoin(ctx, pool, w, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	// the worker's payload memory is born in one room…
	if _, err := ThreadPost(ctx, pool, w, codeSrc, "delivery input", "", "", nil, nil, nil, "d1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	memCode := firstPayloadCode(t, pool, w)
	// …and delivered against an assignment in another
	if _, err := ThreadPost(ctx, pool, owner, codeWork, "do it", "", "assign", nil, []int64{},
		&AssignSpec{To: w, Requirements: "r"}, "d2"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	assignID := countRow(t, pool, `SELECT a.id FROM assigns a JOIN threads t ON t.id=a.thread_id WHERE t.code=$1`, codeWork)
	memJSON := fmt.Sprintf(`[{"name":"m","code":%q}]`, memCode)
	if _, err := AssignTake(ctx, pool, w, int64(assignID), `{"ok":true}`, memJSON, "d3"); err != nil {
		t.Fatalf("take+deliver: %v", err)
	}

	disposeRoom(t, pool, owner, w, codeSrc)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 1 {
		t.Fatal("payload referenced by an unjudged delivery must survive dissolution")
	}
	if _, err := AssignJudge(ctx, pool, owner, int64(assignID), "adopt", "", "d4"); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload of a judged delivery must be reclaimed by the sweep")
	}
}

// TestDissolveKeepsExpiredUnsweptClaimPin: the claim ROW is the fact.
// A claim past expires_at that the recovery worker has not swept yet
// still says active — its pins stay protected until recovery
// materializes the expiry; the sweep then finishes the release.
func TestDissolveKeepsExpiredUnsweptClaimPin(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "exp", true, "")
	defer cleanupDisposed(t, pool, code)
	memCode, taskCode := harnessTaskOnPayload(t, pool, owner, code)

	if _, err := ClaimTask(ctx, pool, w, taskCode, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimID := countRow(t, pool, `SELECT claim_id FROM tb_task_claim c
		JOIN tb_task t ON t.id = c.task_id WHERE t.code=$1`, taskCode)
	// the clock ran out; the sweeper has NOT run — the row still says
	// active, and that row is the fact
	if _, err := pool.Exec(ctx,
		`UPDATE tb_task_claim SET expires_at = NOW() - INTERVAL '1 second' WHERE claim_id = $1`, claimID); err != nil {
		t.Fatal(err)
	}
	if _, err := CloseTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("close task: %v", err)
	}
	disposeRoom(t, pool, owner, w, code)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 1 {
		t.Fatal("payload pinned by an expired-but-unswept claim must survive dissolution")
	}
	// recovery materializes the expiry; the sweep finishes the release
	if _, err := ExpireClaims(ctx, pool, time.Now(), 50); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload of a materialized-expired claim must be reclaimed by the sweep")
	}
}

// TestDissolveKeepsInflightSubmissionPayload is the §4 closeout
// scenario end to end: task v1 references a thread payload M; the
// executor claims v1; the publisher pauses and revises to v2 without
// M; the executor submits under the OLD claim and the delivery goes
// uncertain; THEN the source thread dissolves. The submission
// recovers against its bound version, the executor's pinned-input
// read stays legal while the delivery is unresolved, and M is
// collected only after the engagement fully ends.
func TestDissolveKeepsInflightSubmissionPayload(t *testing.T) {
	pool := pubTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	executor := pubSeedBot(t, pool, 0)
	rcv := startProgReceiver(t)
	ctx := context.Background()

	code, _ := threadStart(t, pool, owner, "inflight", true, "")
	defer cleanupDisposed(t, pool, code)
	if _, err := ThreadPost(ctx, pool, owner, code, "v1 harness payload", "", "", nil, nil, nil, "i1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	memCode := firstPayloadCode(t, pool, owner)

	v1 := pubContract(rcv.url)
	v1.HarnessRefs = []string{memCode}
	taskCode := pubCreateForTest(t, pool, owner, v1, 5)
	if _, err := OpenTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("open: %v", err)
	}
	claim, err := ClaimTask(ctx, pool, executor, taskCode, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// pause + revise to v2 without the harness ref
	if _, err := PauseTask(ctx, pool, owner, taskCode); err != nil {
		t.Fatalf("pause: %v", err)
	}
	v2 := pubContract(rcv.url)
	if _, err := UpdateTask(ctx, pool, owner, taskCode, v2); err != nil {
		t.Fatalf("update: %v", err)
	}

	// submit under the OLD claim (paused tasks still accept active
	// claims); the receiver sleeps past the HTTP budget → uncertain
	rcv.mu.Lock()
	rcv.sleep = 3 * time.Second
	rcv.mu.Unlock()
	delivery.SetRequestTimeoutForTest(400 * time.Millisecond)
	defer delivery.SetRequestTimeoutForTest(0)
	claimRef := claim.ClaimID
	view, err := SubmitWork(ctx, pool, executor, SubmitInput{
		Code: taskCode, ClaimID: &claimRef,
		RequestKey: fmt.Sprintf("inflight-%d", time.Now().UnixNano()),
		Payload:    []byte(submitPayloadOK),
	}, testAgentRefKey, time.Now())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if view.State != task.SubUncertain {
		t.Fatalf("state = %s, want uncertain", view.State)
	}
	delivery.SetRequestTimeoutForTest(0)

	// the hammer falls on the source thread
	disposeRoom(t, pool, owner, executor, code)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 1 {
		t.Fatal("payload under an in-flight submission must survive dissolution")
	}
	// storage protection and the read interface agree: the used claim
	// still serves its pinned v1 harness while the delivery is
	// unresolved — explicit and implicit alike
	gh, err := GetHarness(ctx, pool, executor, taskCode, memCode, claim.ClaimID.Int64Ptr(), time.Now())
	if err != nil {
		t.Fatalf("harness under used claim: %v", err)
	}
	if gh["pinned"] != true || gh["content"] != "v1 harness payload" {
		t.Fatalf("pinned read under in-flight submission = %v", gh)
	}
	gh2, err := GetHarness(ctx, pool, executor, taskCode, memCode, nil, time.Now())
	if err != nil || gh2["pinned"] != true {
		t.Fatalf("implicit pinned read = %v err=%v", gh2, err)
	}
	// the engaged view still shows the bound version
	wv, err := GetWork(ctx, pool, executor, taskCode, time.Now())
	if err != nil {
		t.Fatalf("work get: %v", err)
	}
	if wv["contract_version"] != int64(1) {
		t.Fatalf("engaged view version = %v, want the bound 1", wv["contract_version"])
	}

	// recovery completes the delivery against the bound version; the
	// engagement ends; the sweep collects M and its pins
	rcv.mu.Lock()
	rcv.sleep = 0
	rcv.mu.Unlock()
	ageSubmission(t, pool, view.SubmissionID.Int64())
	if _, err := RecoverSubmissions(ctx, pool, testAgentRefKey, time.Now(), 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
	sub, _ := repository.FindSubmissionByID(ctx, pool, view.SubmissionID.Int64())
	if sub.State != task.SubSettled {
		t.Fatalf("recovered submission = %s, want settled", sub.State)
	}
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, memCode); n != 0 {
		t.Fatal("payload of an ended engagement must be reclaimed by the sweep")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM claim_harness_revisions p
		JOIN tb_task_claim c ON c.claim_id = p.claim_id WHERE c.claim_id = $1`, claim.ClaimID.Int64()); n != 0 {
		t.Fatal("pins of an ended engagement must be dropped")
	}
}

// TestAuthorAdoptsThreadPayload: the moment the author manages a
// thread payload as an independent knowledge asset — an explicit
// memory_put update, or publishing it — its life detaches from the
// room's: dissolution stops being a way for it to die.
func TestAuthorAdoptsThreadPayload(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	codeA, _ := threadStart(t, pool, owner, "adoptA", true, "")
	codeB, _ := threadStart(t, pool, owner, "adoptB", true, "")
	defer cleanupDisposed(t, pool, codeA)
	defer cleanupDisposed(t, pool, codeB)

	// room A: an explicit memory_put update adopts the payload
	if _, err := ThreadPost(ctx, pool, owner, codeA, "curated payload A", "", "", nil, nil, nil, "a1"); err != nil {
		t.Fatalf("postA: %v", err)
	}
	memA := firstPayloadCode(t, pool, owner)
	if _, err := Push(ctx, pool, owner, map[string]interface{}{
		"code": memA, "title": "curated A", "tags": []interface{}{"howto"},
		"content": strings.Repeat("curated standalone content ", 4),
	}, 128, 10, 32, 500, 100*1024); err != nil {
		t.Fatalf("adopting update: %v", err)
	}
	// room B: publishing adopts the payload
	if _, err := ThreadPost(ctx, pool, owner, codeB, "published payload B", "", "", nil, nil, nil, "b1"); err != nil {
		t.Fatalf("postB: %v", err)
	}
	var memB string
	if err := pool.QueryRow(ctx,
		`SELECT code FROM tb_kungfus WHERE origin='thread' AND bot_id=$1 AND code <> $2 LIMIT 1`, owner, memA).Scan(&memB); err != nil {
		t.Fatalf("memB: %v", err)
	}
	if _, err := Share(ctx, pool, owner, memB); err != nil {
		t.Fatalf("share: %v", err)
	}

	disposeRoom(t, pool, owner, w, codeA)
	disposeRoom(t, pool, owner, w, codeB)
	for _, mem := range []string{memA, memB} {
		if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, mem); n != 1 {
			t.Fatalf("adopted payload %s must survive its room's dissolution", mem)
		}
		if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1 AND origin='standalone'`, mem); n != 1 {
			t.Fatalf("adopted payload %s must be standalone", mem)
		}
	}
	// the sweep never touches adopted payloads
	if _, err := PayloadSweep(ctx, pool, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, mem := range []string{memA, memB} {
		if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE code=$1`, mem); n != 1 {
			t.Fatalf("sweep collected the adopted payload %s", mem)
		}
	}
}

// TestPayloadDeleteSerializesWithReferenceLocks: a transaction that
// holds a KEY SHARE lock on a payload memory (what harness validation
// and delivery pinning take) blocks the disposal's delete until it
// commits — reference establishment and physical deletion serialize
// on the row, never on a lock-free read-then-use.
func TestPayloadDeleteSerializesWithReferenceLocks(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	w, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, _ := threadStart(t, pool, owner, "lock", true, "")
	defer cleanupDisposed(t, pool, code)
	if _, err := ThreadPost(ctx, pool, owner, code, "locked payload", "", "", nil, nil, nil, "l1"); err != nil {
		t.Fatalf("post: %v", err)
	}
	memID := countRow(t, pool, `SELECT id FROM tb_kungfus WHERE origin='thread' AND bot_id=$1`, owner)

	// a referencing transaction holds the row
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM tb_kungfus WHERE id = $1 FOR KEY SHARE`, memID); err != nil {
		t.Fatal(err)
	}

	// the disposal cannot delete past the reference holder: with a
	// short budget it fails; the room stays
	shortCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	if _, err := ThreadDissolve(shortCtx, pool, owner, code, ""); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("disposal must serialize against a KEY SHARE reference holder")
	}
	if n := countRow(t, pool, `SELECT count(*) FROM threads WHERE code=$1`, code); n != 1 {
		_ = tx.Rollback(ctx)
		t.Fatal("blocked disposal must leave the room intact")
	}

	// the reference commits; the disposal goes through
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	disposeRoom(t, pool, owner, w, code)
	if n := countRow(t, pool, `SELECT count(*) FROM tb_kungfus WHERE id=$1`, memID); n != 0 {
		t.Fatal("payload must follow its room once the reference commits")
	}
}

// cleanupDisposed: best-effort teardown for disposal tests — the
// body may already have removed the room, in which case there is
// nothing to clean.
func cleanupDisposed(t *testing.T, pool *pg.Pool, code string) {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `SELECT id FROM threads WHERE code=$1`, code).Scan(&id)
	if err != nil {
		return // already gone — the test did its job
	}
	_, _ = repository.DisposeThreadCascade(context.Background(), pool, id)
}

// helpers ------------------------------------------------------------

func firstPayloadCode(t *testing.T, pool *pg.Pool, bot int64) string {
	t.Helper()
	var code string
	if err := pool.QueryRow(context.Background(),
		`SELECT code FROM tb_kungfus WHERE origin='thread' AND bot_id=$1 ORDER BY id DESC LIMIT 1`, bot).Scan(&code); err != nil {
		t.Fatalf("payload code: %v", err)
	}
	return code
}
