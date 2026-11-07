package service

// Room disposal tests (migration 035): dissolution settles and
// hard-deletes the aggregate, payload memories follow their last
// reference, cross-room pins survive, scrub redacts an author's own
// content without breaking others' references, and the retention
// sweep only touches closed rooms past the cutoff.

import (
	"context"
	"testing"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
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
