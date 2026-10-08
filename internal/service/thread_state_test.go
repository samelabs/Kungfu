package service

// D3 speaking-state tests: the response-object rules (§6.3), the
// receipt lifecycle (pending/fulfilled/withdrawn with resolutions),
// handle/retract, the four collection hooks, timeline reads and L3
// replay snapshots for the three new tools.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func stateReceipt(t *testing.T, pool *pg.Pool, entryID, accountID int64) (state string, resolution *string, note *string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT state, resolution, note FROM thread_receipts WHERE entry_id = $1 AND account_id = $2`,
		entryID, accountID).Scan(&state, &resolution, &note)
	if err != nil {
		t.Fatalf("receipt lookup: %v", err)
	}
	return
}

func statePost(t *testing.T, pool *pg.Pool, bot int64, thread, content, summary string, replyTo *int64, ask []int64, idem string) map[string]any {
	t.Helper()
	res, err := ThreadPost(context.Background(), pool, bot, thread, content, "", summary, replyTo, ask, nil, idem)
	if err != nil {
		t.Fatalf("thread_post: %v", err)
	}
	return res
}

func statePostMem(t *testing.T, pool *pg.Pool, bot int64, thread, memory, summary string, replyTo *int64, ask []int64, idem string) map[string]any {
	t.Helper()
	res, err := ThreadPost(context.Background(), pool, bot, thread, "", memory, summary, replyTo, ask, nil, idem)
	if err != nil {
		t.Fatalf("thread_post(memory): %v", err)
	}
	return res
}

// TestResponseObjectRules: the four ordered rules of §6.3, frozen at
// post time; explicit ask (including []) beats everything; a replied
// author who lost speech right yields nobody with NO fallback.
func TestResponseObjectRules(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "rules", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, b}, code)
	for _, m := range []int64{a, b} {
		if _, err := ThreadJoin(ctx, pool, m, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	// rule 1a: explicit ask — exactly those, frozen
	r1 := statePost(t, pool, owner, code, "do this", "", nil, []int64{a}, "p1")
	if entryID := r1["entry"].(int64); entryID == 0 {
		t.Fatal("no entry id")
	}
	st, _, _ := stateReceipt(t, pool, r1["entry"].(int64), a)
	if st != "pending" {
		t.Fatalf("asked member receipt = %s, want pending", st)
	}
	// b was not asked: no receipt row at all
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_receipts WHERE entry_id = $1 AND account_id = $2`,
		r1["entry"].(int64), b).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unasked member has %d receipts, want 0", n)
	}

	// rule 1b: explicit empty ask = notify only, nobody owes
	r2 := statePost(t, pool, owner, code, "fyi", "", nil, []int64{}, "p2")
	if got := len(r2["asked"].([]int64)); got != 0 {
		t.Fatalf("ask:[] produced %d asked, want 0", got)
	}

	// rule 2: reply to another's entry asks its author — and replying
	// ends my own pending toward it (A3)
	mine := statePost(t, pool, owner, code, "a, report", "", nil, []int64{a}, "p3")
	reply := statePost(t, pool, a, code, "here it is", "", ptrOf(mine["entry"].(int64)), nil, "p4")
	asked := reply["asked"].([]int64)
	if len(asked) != 1 || asked[0] != owner {
		t.Fatalf("reply asked = %v, want the author %d", asked, owner)
	}
	if !reply["fulfilled"].(bool) {
		t.Fatal("reply must end own pending toward the target (§6.3)")
	}
	if st, _, _ = stateReceipt(t, pool, mine["entry"].(int64), a); st != "fulfilled" {
		t.Fatalf("reply target receipt = %s, want fulfilled", st)
	}

	// rule 2 with lost speech right: A posts, falls to observer, and
	// the reply to A entry asks NOBODY — no fallback to the pair rule
	ax := statePost(t, pool, a, code, "will fall silent", "", nil, nil, "p5a")
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleObserver, "sr1"); err != nil {
		t.Fatalf("demote a: %v", err)
	}
	lost := statePost(t, pool, owner, code, "replying to the silent one", "", ptrOf(ax["entry"].(int64)), nil, "p5")
	if got := len(lost["asked"].([]int64)); got != 0 {
		t.Fatalf("replied author lost speech right, asked = %v, want none (no fallback)", lost["asked"])
	}
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleSpeaker, "sr2"); err != nil {
		t.Fatalf("restore a: %v", err)
	}

	// rule 3: two speech-capable members — the other side. owner+a+b
	// are three, so shrink: b demoted to observer leaves owner+a.
	if _, err := ThreadSetRole(ctx, pool, owner, code, b, repository.ThreadRoleObserver, ""); err != nil {
		t.Fatalf("demote b: %v", err)
	}
	pair := statePost(t, pool, a, code, "pair rule", "", nil, nil, "p6")
	asked = pair["asked"].([]int64)
	if len(asked) != 1 || asked[0] != owner {
		t.Fatalf("pair asked = %v, want %d", asked, owner)
	}

	// rule 4: three speech-capable members, no ask, no reply ⇒ broadcast
	if _, err := ThreadSetRole(ctx, pool, owner, code, b, repository.ThreadRoleSpeaker, ""); err != nil {
		t.Fatalf("restore b: %v", err)
	}
	bc := statePost(t, pool, owner, code, "broadcast", "", nil, nil, "p7")
	if got := len(bc["asked"].([]int64)); got != 0 {
		t.Fatalf("broadcast asked = %v, want none", bc["asked"])
	}

	// ask discipline: self and non-members rejected (R-15, §6.2)
	if _, err := ThreadPost(ctx, pool, owner, code, "note", "", "", nil, []int64{owner}, nil, "p8"); err == nil {
		t.Fatal("ask including the author must be rejected")
	} else {
		threadErrIs(t, err, 422, "INVALID_TARGET")
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "note", "", "", nil, []int64{999}, nil, "p9"); err == nil {
		t.Fatal("ask of a non-member must be rejected")
	} else {
		threadErrIs(t, err, 422, "INVALID_TARGET")
	}
}

// TestHandleAndRetract: handle ends exactly my pending with a stored
// note and creates no entry; retract withdraws only still-pending
// receipts and only by the author who is still a member.
func TestHandleAndRetract(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "hr", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	e1 := statePost(t, pool, owner, code, "a, look", "", nil, []int64{a}, "h1")
	entryID := e1["entry"].(int64)

	// handle with a note (§6.3): no new entry, note persisted
	if _, err := ThreadHandle(ctx, pool, a, code, entryID, "seen, no reply needed", "hk1"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	st, res, note := stateReceipt(t, pool, entryID, a)
	if st != "fulfilled" || res == nil || *res != "handle" || note == nil || *note != "seen, no reply needed" {
		t.Fatalf("handle receipt = %s %v %v", st, res, note)
	}
	var entries int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM thread_entries WHERE thread_id = (SELECT id FROM threads WHERE code=$1)`, code).Scan(&entries)
	if entries != 1 {
		t.Fatalf("handle created entries: %d, want 1", entries)
	}

	// second handle: nothing pending anymore
	if _, err := ThreadHandle(ctx, pool, a, code, entryID, "", "hk2"); err == nil {
		t.Fatal("double handle must be rejected")
	} else {
		threadErrIs(t, err, 422, "INVALID_TARGET")
	}

	// retract by non-author: owner tries to retract a's entry
	e2 := statePost(t, pool, a, code, "question for owner", "", nil, nil, "h3") // pair rule asks owner
	if _, err := ThreadRetract(ctx, pool, owner, code, e2["entry"].(int64), ""); err == nil {
		t.Fatal("retract by non-author must be rejected")
	} else {
		threadErrIs(t, err, 403, "NOT_YOURS")
	}

	// retract by author: only pending receipts move
	if _, err := ThreadRetract(ctx, pool, a, code, e2["entry"].(int64), "rk"); err != nil {
		t.Fatalf("retract: %v", err)
	}
	st, res, _ = stateReceipt(t, pool, e2["entry"].(int64), owner)
	if st != "withdrawn" || res == nil || *res != "retract" {
		t.Fatalf("retracted receipt = %s %v", st, res)
	}
	// fulfilled history from e1 is untouched
	st, res, _ = stateReceipt(t, pool, entryID, a)
	if st != "fulfilled" || *res != "handle" {
		t.Fatalf("fulfilled history rewritten: %s %v", st, res)
	}

	// retract with nothing left pending is rejected (L2: only join
	// and Task claim are no-effect successes)
	if _, err := ThreadRetract(ctx, pool, owner, code, entryID, "rz"); err == nil {
		t.Fatal("retract with zero pending receipts must be rejected")
	} else {
		threadErrIs(t, err, 422, "INVALID_TARGET")
	}
}

// TestPostGuards: closed rooms reject post/handle/retract with
// THREAD_CLOSED; observers cannot post; content rules (summary
// required over 500 chars) steer revise.
func TestPostGuards(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	obs, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "guards", true, "")
	defer threadCleanup(t, pool, []int64{owner, obs}, code)
	if _, err := ThreadJoin(ctx, pool, obs, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := ThreadSetRole(ctx, pool, owner, code, obs, repository.ThreadRoleObserver, ""); err != nil {
		t.Fatalf("observer role: %v", err)
	}

	// observer cannot speak (§6.2)
	if _, err := ThreadPost(ctx, pool, obs, code, "hi", "", "", nil, nil, nil, "g1"); err == nil {
		t.Fatal("observer post must be rejected")
	} else {
		threadErrIs(t, err, 403, "READ_ONLY")
	}

	// content over 500 chars without a summary steers revise (§2)
	long := strings.Repeat("字", 501)
	if _, err := ThreadPost(ctx, pool, owner, code, long, "", "", nil, nil, nil, "g2"); err == nil {
		t.Fatal("long content without summary must be rejected")
	} else {
		threadErrIs(t, err, 422, "SUMMARY_REQUIRED")
	}
	// with a summary it passes
	statePost(t, pool, owner, code, long, "长文摘要", nil, nil, "g3")

	// close: post/handle/retract all THREAD_CLOSED (§6.5)
	e := statePost(t, pool, owner, code, "last", "", nil, nil, "g4")
	if _, err := ThreadClose(ctx, pool, owner, code, "g5"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "after close", "", "", nil, nil, nil, "g6"); err == nil {
		t.Fatal("post after close must be rejected")
	} else {
		threadErrIs(t, err, 409, "THREAD_CLOSED")
	}
	if _, err := ThreadHandle(ctx, pool, owner, code, e["entry"].(int64), "", "g7"); err == nil {
		t.Fatal("handle after close must be rejected")
	} else {
		threadErrIs(t, err, 409, "THREAD_CLOSED")
	}
	if _, err := ThreadRetract(ctx, pool, owner, code, e["entry"].(int64), "g8"); err == nil {
		t.Fatal("retract after close must be rejected")
	} else {
		threadErrIs(t, err, 409, "THREAD_CLOSED")
	}
}

// TestMemoryPinning: content posts create origin=thread memories
// excluded from memory_list; memory refs pin own active or others'
// public current versions; withdrawn or private refs are refused; a
// withdrawn own pin stays readable to members (§5/§9), an unshared
// foreign pin degrades to unreadable without leaking content.
func TestMemoryPinning(t *testing.T) {
	pool := revisionTestPool(t)
	owner, ownerName, _ := a7TestBot(t, pool, 5)
	other, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "pin", true, "")
	defer threadCleanup(t, pool, []int64{owner, other}, code)
	if _, err := ThreadJoin(ctx, pool, other, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// content path: thread memory created, pinned at revision 1,
	// excluded from memory_list
	r := statePost(t, pool, owner, code, "payload text", "", nil, nil, "m1")
	if r["revision"].(int64) != 1 {
		t.Fatalf("content pin revision = %v, want 1", r["revision"])
	}
	memCode, _ := r["memory"].(string)
	var origin string
	if err := pool.QueryRow(ctx, `SELECT origin FROM tb_kungfus WHERE code = $1`, memCode).Scan(&origin); err != nil || origin != "thread" {
		t.Fatalf("thread memory origin = %s (%v)", origin, err)
	}
	lst, err := ListKungfusForBot(ctx, pool, owner, 50, 0)
	if err != nil {
		t.Fatalf("memory_list: %v", err)
	}
	if items := lst["items"]; items != nil {
		if arr, ok := items.([]map[string]any); ok {
			for _, it := range arr {
				if it["code"] == memCode {
					t.Fatal("thread memory leaked into memory_list")
				}
			}
		}
	}
	_ = ownerName

	// own active memory reference pins its current revision
	up, err := Push(ctx, pool, owner, map[string]interface{}{
		"title": "doc", "tags": []interface{}{"doc"}, "content": strings.Repeat("x", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := Push(ctx, pool, owner, map[string]interface{}{
		"title": "doc", "tags": []interface{}{"doc"}, "content": strings.Repeat("y", 60), "code": up.Code,
	}, 128, 10, 24, 500, 102400); err != nil {
		t.Fatalf("push2: %v", err)
	}
	r2 := statePostMem(t, pool, owner, code, up.Code, "ref own", nil, nil, "m2")
	if r2["memory"].(string) != up.Code || r2["revision"].(int64) != 2 {
		t.Fatalf("own ref pinned %v rev %v, want current rev 2", r2["memory"], r2["revision"])
	}

	// others' private: NOT_FOUND without existence leak
	priv, err := Push(ctx, pool, other, map[string]interface{}{
		"title": "secret", "tags": []interface{}{"s"}, "content": strings.Repeat("z", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push other: %v", err)
	}
	if _, err := ThreadPost(ctx, pool, owner, code, "", priv.Code, "ref other", nil, nil, nil, "m3"); err == nil {
		t.Fatal("reference to a private foreign memory must be rejected")
	} else {
		threadErrIs(t, err, 404, "NOT_FOUND")
	}

	// others' public: allowed; after unshare the pin degrades to
	// unreadable in expansions, content not leaked (§5)
	if _, err := Share(ctx, pool, other, priv.Code); err != nil {
		t.Fatalf("share: %v", err)
	}
	r3 := statePostMem(t, pool, owner, code, priv.Code, "ref public", nil, nil, "m4")
	if _, err := Unshare(ctx, pool, other, priv.Code); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	view, err := ThreadGet(ctx, pool, owner, code, "", []int64{r3["entry"].(int64)}, "", nil, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	expanded := view["entries"].([]any)
	if len(expanded) != 1 {
		t.Fatalf("expanded = %d", len(expanded))
	}
	row := expanded[0].(map[string]any)
	if row["readable"] != false {
		t.Fatalf("unshared foreign pin readable = %v", row["readable"])
	}
	if _, has := row["content"]; has {
		t.Fatal("unreadable pin leaked content")
	}

	// own withdrawn pin: the entry stays readable to members (§9)
	if _, err := Delete(ctx, pool, owner, up.Code); err != nil {
		t.Fatalf("delete own: %v", err)
	}
	view2, err := ThreadGet(ctx, pool, owner, code, "", []int64{r2["entry"].(int64)}, "", nil, false)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	row2 := view2["entries"].([]any)[0].(map[string]any)
	if row2["readable"] != true {
		t.Fatalf("own withdrawn pin readable = %v, want true (§9 fixed pin)", row2["readable"])
	}
}

// TestCollectionHooks: leave/remove/demote/close collect pending
// receipts with their distinct resolutions; rejoining never
// resurrects an old obligation (§6.2/§6.5, L1).
func TestCollectionHooks(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "collect", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, b}, code)
	for _, m := range []int64{a, b} {
		if _, err := ThreadJoin(ctx, pool, m, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	e1 := statePost(t, pool, owner, code, "a and b respond", "", nil, []int64{a, b}, "c1")

	// leave collects with resolution=leave
	if _, err := ThreadLeave(ctx, pool, b, code, "c2"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	st, res, _ := stateReceipt(t, pool, e1["entry"].(int64), b)
	if st != "withdrawn" || *res != "leave" {
		t.Fatalf("leave receipt = %s %v", st, res)
	}

	// demote collects with resolution=role_change
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleObserver, "c3"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	st, res, _ = stateReceipt(t, pool, e1["entry"].(int64), a)
	if st != "withdrawn" || *res != "role_change" {
		t.Fatalf("demote receipt = %s %v", st, res)
	}
	// promote back: no resurrection
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleSpeaker, "c4"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	st, _, _ = stateReceipt(t, pool, e1["entry"].(int64), a)
	if st != "withdrawn" {
		t.Fatalf("resurrected receipt = %s", st)
	}

	// remove collects with resolution=remove
	e2 := statePost(t, pool, owner, code, "a again", "", nil, []int64{a}, "c5")
	if _, err := ThreadRemoveMember(ctx, pool, owner, code, a, "c6"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	st, res, _ = stateReceipt(t, pool, e2["entry"].(int64), a)
	if st != "withdrawn" || *res != "remove" {
		t.Fatalf("remove receipt = %s %v", st, res)
	}

	// close collects everything still pending with resolution=close
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("rejoin: %v", err) // old key still live (no reissue happened)
	}
	e3 := statePost(t, pool, owner, code, "before close", "", nil, []int64{a}, "c7")
	if _, err := ThreadClose(ctx, pool, owner, code, "c8"); err != nil {
		t.Fatalf("close: %v", err)
	}
	st, res, _ = stateReceipt(t, pool, e3["entry"].(int64), a)
	if st != "withdrawn" || *res != "close" {
		t.Fatalf("close receipt = %s %v", st, res)
	}
	// the room still reads back (§6.5 read-only) with the timeline
	view, err := ThreadGet(ctx, pool, a, code, "", nil, "", nil, false)
	if err != nil {
		t.Fatalf("read closed room: %v", err)
	}
	if tl := view["timeline"].([]map[string]any); len(tl) != 3 {
		t.Fatalf("closed room timeline = %d entries, want 3", len(tl))
	}
}

// TestPostIdempotency: replay returns the first snapshot (even after
// the room changed), a different request under the same key conflicts
// with zero side effects, and handle replay cannot double-fulfill.
func TestPostIdempotency(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "idem", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// same key, same request: stable snapshot
	r1 := statePost(t, pool, owner, code, "first", "", nil, []int64{a}, "same-key")
	r2 := statePost(t, pool, owner, code, "first", "", nil, []int64{a}, "same-key")
	if fmt.Sprint(r1) != fmt.Sprint(r2) {
		t.Fatalf("replay drifted: %v vs %v", r1, r2)
	}

	// the snapshot survives later room changes
	if _, err := ThreadPost(ctx, pool, owner, code, "another", "", "", nil, nil, nil, "other-key"); err != nil {
		t.Fatalf("post2: %v", err)
	}
	r3 := statePost(t, pool, owner, code, "first", "", nil, []int64{a}, "same-key")
	if fmt.Sprint(r1) != fmt.Sprint(r3) {
		t.Fatalf("replay after new posts drifted: %v vs %v", r1, r3)
	}

	// same key, different request: conflict, zero side effects
	before := stateCountEntries(t, pool, code)
	if _, err := ThreadPost(ctx, pool, owner, code, "DIFFERENT", "", "", nil, []int64{a}, nil, "same-key"); err == nil {
		t.Fatal("same key different request must conflict")
	} else {
		threadErrIs(t, err, 409, "IDEMPOTENCY_CONFLICT")
	}
	if after := stateCountEntries(t, pool, code); after != before {
		t.Fatalf("conflict left side effects: %d -> %d", before, after)
	}

	// handle replay: receipt stays fulfilled exactly once, second
	// call returns the first snapshot without a new effect
	e := statePost(t, pool, owner, code, "a please", "", nil, []int64{a}, "h-key")
	if _, err := ThreadHandle(ctx, pool, a, code, e["entry"].(int64), "note", "hk"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	h2, err := ThreadHandle(ctx, pool, a, code, e["entry"].(int64), "note", "hk")
	if err != nil {
		t.Fatalf("handle replay: %v", err)
	}
	if h2["resolution"] != "handle" {
		t.Fatalf("handle replay = %v", h2)
	}
	var resolutions int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_receipts WHERE entry_id=$1 AND account_id=$2 AND state='fulfilled'`,
		e["entry"].(int64), a).Scan(&resolutions); err != nil || resolutions != 1 {
		t.Fatalf("fulfilled rows = %d, want exactly 1", resolutions)
	}
}

// TestPostConcurrency: reply and handle racing on one receipt —
// exactly one fulfillment (L5); concurrent posts serialize on the
// room lock with unique, strictly increasing seq.
func TestPostConcurrency(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "race", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	e := statePost(t, pool, owner, code, "race target", "", nil, []int64{a}, "r0")
	target := e["entry"].(int64)

	// reply vs handle on the same pending receipt
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = ThreadPost(ctx, pool, a, code, "reply", "", "", ptrOf(target), nil, nil, "race-reply")
	}()
	go func() { defer wg.Done(); _, _ = ThreadHandle(ctx, pool, a, code, target, "", "race-handle") }()
	wg.Wait()
	var fulfilled, total int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE state='fulfilled'), COUNT(*) FROM thread_receipts WHERE entry_id=$1 AND account_id=$2`,
		target, a).Scan(&fulfilled, &total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 || fulfilled != 1 {
		t.Fatalf("receipt rows=%d fulfilled=%d, want exactly one row fulfilled once", total, fulfilled)
	}

	// concurrent posts: unique strictly increasing seq
	const n = 8
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = ThreadPost(ctx, pool, owner, code, fmt.Sprintf("c%d", i), "", "", nil, nil, nil, fmt.Sprintf("cp-%d", i))
		}(i)
	}
	wg.Wait()
	rows, err := pool.Query(ctx,
		`SELECT seq FROM thread_entries WHERE thread_id=(SELECT id FROM threads WHERE code=$1) ORDER BY seq`, code)
	if err != nil {
		t.Fatalf("seq query: %v", err)
	}
	defer rows.Close()
	var prev int64
	count := 0
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq <= prev {
			t.Fatalf("seq not strictly increasing: %d after %d", seq, prev)
		}
		prev = seq
		count++
	}
	if count != n+2 {
		t.Fatalf("entries = %d, want %d", count, n+2)
	}
}

func stateCountEntries(t *testing.T, pool *pg.Pool, code string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM thread_entries WHERE thread_id = (SELECT id FROM threads WHERE code = $1)`, code).Scan(&n); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return n
}

func ptrOf(n int64) *int64 { return &n }
