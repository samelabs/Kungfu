package service

// D4 assignment tests: the §6.4 lifecycle against kungfu.md and
// R-18 — take/drop/void exits, judgment in closed rooms, deadlines
// (inline guard + sweeper), departure semantics on both sides.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

func assignState(t *testing.T, pool *pg.Pool, id int64) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM assigns WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("assign lookup: %v", err)
	}
	return state
}

func assignPost(t *testing.T, pool *pg.Pool, bot int64, thread, content string, to int64, idem string) (entryID, assignID int64) {
	return assignPostAsk(t, pool, bot, thread, content, to, false, idem)
}

func assignPostAsk(t *testing.T, pool *pg.Pool, bot int64, thread, content string, to int64, ask bool, idem string) (entryID, assignID int64) {
	t.Helper()
	var askList []int64
	if ask {
		askList = []int64{to}
	}
	res, err := ThreadPost(context.Background(), pool, bot, thread, content, "", "", nil, askList,
		&AssignSpec{To: to, Requirements: "do the thing", DeliverDueS: 120, JudgeDueS: 120}, idem)
	if err != nil {
		t.Fatalf("post+assign(%s): %v", idem, err)
	}
	return res["entry"].(int64), res["assign"].(int64)
}

// TestAssignLifecycle: unaccepted creates no obligations (A11); take
// fulfills the carrying receipt (A12); duplicate take rejected; drop
// and void exits (A23); reject reason readable by the assignee.
func TestAssignLifecycle(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	b, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "assign", true, "")
	defer threadCleanup(t, pool, []int64{owner, a, b}, code)
	for _, m := range []int64{a, b} {
		if _, err := ThreadJoin(ctx, pool, m, raw, ""); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	// A11: riding entry asks nobody; unaccepted assignment binds no one
	entryID, assignID := assignPost(t, pool, owner, code, "task for a", a, "a1")
	var receipts int64
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thread_receipts WHERE entry_id = $1`, entryID).Scan(&receipts)
	if receipts != 0 {
		t.Fatalf("unaccepted assign created %d receipts, want 0", receipts)
	}

	// A12: the carrying entry itself asks the assignee — take must
	// fulfill that receipt (§6.4)
	askEntry, askAssign := assignPostAsk(t, pool, owner, code, "a, take this one", a, true, "a2")
	if _, err := AssignTake(ctx, pool, a, askAssign, "", "", "tk1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	st, res, _ := stateReceipt(t, pool, askEntry, a)
	if st != "fulfilled" || res == nil || *res != "take" {
		t.Fatalf("take must fulfill the carrying receipt: %s %v", st, res)
	}
	if _, err := AssignTake(ctx, pool, a, assignID, "", "", "tk1b"); err != nil {
		t.Fatalf("take first assign: %v", err)
	}

	// duplicate take rejected (L2 has no take exception)
	if _, err := AssignTake(ctx, pool, a, assignID, "", "", "tk2"); err == nil {
		t.Fatal("duplicate take must be rejected")
	} else {
		threadErrIs(t, err, 409, "INVALID_STATE")
	}

	// submit: payload against bound schema; delivery immutable
	delivered := `{"report":"done","pages":3}`
	if _, err := AssignSubmit(ctx, pool, a, assignID, delivered, "", "sm1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if assignState(t, pool, assignID) != "delivered" {
		t.Fatal("not delivered after submit")
	}
	if _, err := AssignSubmit(ctx, pool, a, assignID, `{"x":1}`, "", "sm2"); err == nil {
		t.Fatal("second submit must be rejected")
	} else {
		threadErrIs(t, err, 409, "INVALID_STATE")
	}

	// judge: adopt; wrong party rejected; reject requires reason
	if _, err := AssignJudge(ctx, pool, a, assignID, "adopt", "", "j1"); err == nil {
		t.Fatal("judge by assignee must be rejected")
	} else {
		threadErrIs(t, err, 403, "NOT_YOURS")
	}
	if _, err := AssignJudge(ctx, pool, owner, assignID, "reject", "", "j2"); err == nil {
		t.Fatal("reject without reason must be rejected")
	} else {
		threadErrIs(t, err, 422, "VALIDATION_FAILED")
	}
	if _, err := AssignJudge(ctx, pool, owner, assignID, "reject", "pages missing", "j3"); err != nil {
		t.Fatalf("judge reject: %v", err)
	}
	if assignState(t, pool, assignID) != "rejected" {
		t.Fatal("not rejected after judge")
	}
	var reason string
	_ = pool.QueryRow(ctx,
		`SELECT reason FROM assign_deliveries WHERE assign_id = $1`, assignID).Scan(&reason)
	if reason != "pages missing" {
		t.Fatalf("reason = %q", reason)
	}

	// A23: drop before delivery; void of open and taken
	_, a2 := assignPost(t, pool, owner, code, "task2 for a", a, "a3")
	if _, err := AssignTake(ctx, pool, a, a2, "", "", "tk3"); err != nil {
		t.Fatalf("take2: %v", err)
	}
	if _, err := AssignDrop(ctx, pool, a, a2, "dr1"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if assignState(t, pool, a2) != "dropped" {
		t.Fatal("not dropped")
	}
	_, a3 := assignPost(t, pool, owner, code, "task3 for a", a, "a4")
	if _, err := AssignVoid(ctx, pool, a, a3, "vd1"); err == nil {
		t.Fatal("void by assignee must be rejected")
	} else {
		threadErrIs(t, err, 403, "NOT_YOURS")
	}
	if _, err := AssignVoid(ctx, pool, owner, a3, "vd2"); err != nil {
		t.Fatalf("void open: %v", err)
	}
	if assignState(t, pool, a3) != "voided" {
		t.Fatal("not voided")
	}
	_, a4 := assignPost(t, pool, owner, code, "task4 for a", a, "a5")
	if _, err := AssignTake(ctx, pool, a, a4, "", "", "tk4"); err != nil {
		t.Fatalf("take4: %v", err)
	}
	if _, err := AssignVoid(ctx, pool, owner, a4, "vd3"); err != nil {
		t.Fatalf("void taken: %v", err)
	}
	if assignState(t, pool, a4) != "voided" {
		t.Fatal("in-progress void failed")
	}
}

// TestAssignMembershipAndDeparture: R-18 — membership (not speech)
// suffices; demoted assignee keeps working (A9); departure voids
// undelivered on BOTH sides; delivered work survives departure and
// settles at the deadline (A10).
func TestAssignMembershipAndDeparture(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "dep", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// A9: demote the assignee mid-work — submit still works
	_, s1 := assignPost(t, pool, owner, code, "work while demoted", a, "d1")
	if _, err := AssignTake(ctx, pool, a, s1, "", "", "t1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleObserver, "r1"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if _, err := AssignSubmit(ctx, pool, a, s1, `{"ok":true}`, "", "s1"); err != nil {
		t.Fatalf("demoted assignee must still deliver (§6.2): %v", err)
	}
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleSpeaker, "r2"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// A10 assignee side: departure voids undelivered
	_, s2 := assignPost(t, pool, owner, code, "will be voided", a, "d2")
	if _, err := AssignTake(ctx, pool, a, s2, "", "", "t2"); err != nil {
		t.Fatalf("take2: %v", err)
	}
	if _, err := ThreadLeave(ctx, pool, a, code, "l1"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if got := assignState(t, pool, s2); got != "voided" {
		t.Fatalf("departing assignee's undelivered assign = %s, want voided", got)
	}

	// A10 delivered side: survives the assignee's departure; the
	// creator still judges (assignee gone does not freeze judgment)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	_, s3 := assignPost(t, pool, owner, code, "survives my leave", a, "d3")
	if _, err := AssignTake(ctx, pool, a, s3, `{"final":true}`, "", "t3"); err != nil {
		t.Fatalf("take3 (merged): %v", err)
	}
	if _, err := ThreadLeave(ctx, pool, a, code, "l2"); err != nil {
		t.Fatalf("leave2: %v", err)
	}
	if got := assignState(t, pool, s3); got != "delivered" {
		t.Fatalf("delivered assign after assignee left = %s, want delivered", got)
	}
	if _, err := AssignJudge(ctx, pool, owner, s3, "adopt", "", "j1"); err != nil {
		t.Fatalf("creator judges after assignee left: %v", err)
	}

	// A10 creator side (R-18): creator leaves — undelivered voided,
	// delivered keeps its clock and settles undecided at deadline
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("rejoin2: %v", err)
	}
	_, s4 := assignPost(t, pool, owner, code, "creator will leave", a, "d4")
	_, s5 := assignPost(t, pool, owner, code, "undelivered too", a, "d5")
	if _, err := AssignTake(ctx, pool, a, s5, "", "", "t5"); err != nil {
		t.Fatalf("take5: %v", err)
	}
	if _, err := AssignTake(ctx, pool, a, s4, `{"late":true}`, "", "t4"); err != nil {
		t.Fatalf("take4 (merged): %v", err)
	}
	// hand governance over so the creator may leave at all (§6.2)
	if _, err := ThreadSetRole(ctx, pool, owner, code, a, repository.ThreadRoleGovernor, "r3"); err != nil {
		t.Fatalf("handover: %v", err)
	}
	if _, err := ThreadLeave(ctx, pool, owner, code, "l3"); err != nil {
		t.Fatalf("creator leave: %v", err)
	}
	if got := assignState(t, pool, s5); got != "voided" {
		t.Fatalf("creator-left undelivered = %s, want voided (R-18)", got)
	}
	if got := assignState(t, pool, s4); got != "delivered" {
		t.Fatalf("creator-left delivered = %s, want delivered", got)
	}
	// departed creator cannot judge (NOT_MEMBER), deadline settles it
	if _, err := AssignJudge(ctx, pool, owner, s4, "adopt", "", "j2"); err == nil {
		t.Fatal("departed creator must not judge (R-18)")
	} else {
		threadErrIs(t, err, 403, "NOT_MEMBER")
	}
	// deadline (120s) not yet due: the sweep is a no-op
	if n, err := RecoverAssigns(ctx, pool, "now", 50); err != nil || n != 0 {
		t.Fatalf("premature recover: n=%d err=%v", n, err)
	}
	// force the clock: deadline was 120s — move it into the past
	if _, err := pool.Exec(ctx,
		`UPDATE assign_deliveries SET judge_due_at = NOW() - INTERVAL '1 second' WHERE assign_id = $1`, s4); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatalf("recover2: %v", err)
	}
	if got := assignState(t, pool, s4); got != "undecided" {
		t.Fatalf("creator-left delivered after deadline = %s, want undecided (R-18)", got)
	}
}

// TestAssignDeadlines: deliver deadline materializes timed_out (A13);
// an in-flight submit past the deadline is beaten by expiry (L4); the
// judge deadline settles undecided and a late judge is rejected.
func TestAssignDeadlines(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "due", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// deliver deadline: 60s minimum — pull it into the past, submit late
	_, s1 := assignPost(t, pool, owner, code, "late submit", a, "e1")
	if _, err := AssignTake(ctx, pool, a, s1, "", "", "t1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE assigns SET deliver_due_at = NOW() - INTERVAL '1 second' WHERE id = $1`, s1); err != nil {
		t.Fatal(err)
	}
	if _, err := AssignSubmit(ctx, pool, a, s1, `{"late":1}`, "", "s1"); err == nil {
		t.Fatal("submit past deliver deadline must be beaten by expiry (L4)")
	} else {
		threadErrIs(t, err, 409, "INVALID_STATE")
	}
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := assignState(t, pool, s1); got != "timed_out" {
		t.Fatalf("late submit assign = %s, want timed_out after sweep", got)
	}

	// judge deadline: late judge rejected and undecided materialized
	_, s2 := assignPost(t, pool, owner, code, "late judge", a, "e2")
	if _, err := AssignTake(ctx, pool, a, s2, `{"v":1}`, "", "t2"); err != nil {
		t.Fatalf("take2: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE assign_deliveries SET judge_due_at = NOW() - INTERVAL '1 second' WHERE assign_id = $1`, s2); err != nil {
		t.Fatal(err)
	}
	if _, err := AssignJudge(ctx, pool, owner, s2, "adopt", "", "j1"); err == nil {
		t.Fatal("judge past deadline must be beaten by expiry (L4)")
	} else {
		threadErrIs(t, err, 409, "INVALID_STATE")
	}
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := assignState(t, pool, s2); got != "undecided" {
		t.Fatalf("late judge assign = %s, want undecided after sweep", got)
	}

	// sweeper pass over a fresh batch: both transitions recoverable
	if _, err := RecoverAssigns(ctx, pool, "now", 50); err != nil {
		t.Fatalf("recover: %v", err)
	}
}

// TestAssignClosedRoom: delivered work keeps its judgment in a closed
// room (§6.5); undelivered is voided by the close; late take rejected.
func TestAssignClosedRoom(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "closed", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, s1 := assignPost(t, pool, owner, code, "judged after close", a, "c1")
	_, s2 := assignPost(t, pool, owner, code, "voided by close", a, "c2")
	if _, err := AssignTake(ctx, pool, a, s1, `{"done":1}`, "", "t1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := ThreadClose(ctx, pool, owner, code, "cl1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := assignState(t, pool, s2); got != "voided" {
		t.Fatalf("close must void undelivered: %s", got)
	}
	if got := assignState(t, pool, s1); got != "delivered" {
		t.Fatalf("close must not touch delivered: %s", got)
	}
	// judge works in the closed room (§6.5 bypass)
	if _, err := AssignJudge(ctx, pool, owner, s1, "adopt", "", "j1"); err != nil {
		t.Fatalf("judge in closed room: %v", err)
	}
	if got := assignState(t, pool, s1); got != "adopted" {
		t.Fatalf("closed-room judge = %s", got)
	}
	// late take on the voided assign rejected
	if _, err := AssignTake(ctx, pool, a, s2, "", "", "t2"); err == nil {
		t.Fatal("take after close must be rejected")
	} else {
		threadErrIs(t, err, 409, "INVALID_STATE")
	}
}

// TestAssignIdempotencyAndRaces: replay returns the take snapshot;
// same key different request conflicts with zero effects; take vs
// void and take vs take races produce exactly one outcome (L5).
func TestAssignIdempotencyAndRaces(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "race", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}

	// replay stability
	_, s1 := assignPost(t, pool, owner, code, "idem", a, "i1")
	r1, err := AssignTake(ctx, pool, a, s1, "", "", "k1")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	r2, err := AssignTake(ctx, pool, a, s1, "", "", "k1")
	if err != nil {
		t.Fatalf("take replay: %v", err)
	}
	if fmt.Sprint(r1) != fmt.Sprint(r2) {
		t.Fatalf("replay drifted: %v vs %v", r1, r2)
	}
	if _, err := AssignTake(ctx, pool, a, s1, `{"other":1}`, "", "k1"); err == nil {
		t.Fatal("same key different request must conflict")
	} else {
		threadErrIs(t, err, 409, "IDEMPOTENCY_CONFLICT")
	}

	// take vs void race: exactly one wins
	_, s2 := assignPost(t, pool, owner, code, "race tv", a, "i2")
	var wg sync.WaitGroup
	wg.Add(2)
	var took, voided bool
	go func() { defer wg.Done(); _, err := AssignTake(ctx, pool, a, s2, "", "", "rk1"); took = err == nil }()
	go func() { defer wg.Done(); _, err := AssignVoid(ctx, pool, owner, s2, "rk2"); voided = err == nil }()
	wg.Wait()
	st := assignState(t, pool, s2)
	// legal serial orders: take→void (both succeed, terminal voided);
	// take only (void rejected on… no — void of taken is legal, so
	// the take-only order arises when void ran first and failed);
	// void first (take then rejected). At least one succeeds, and the
	// state always tells which side won last.
	if !took && !voided {
		t.Fatalf("take/void race: both failed")
	}
	if took && voided && st != "voided" {
		t.Fatalf("take→void order ended in %s", st)
	}
	if took && !voided && st != "taken" {
		t.Fatalf("take-only order ended in %s", st)
	}
	if !took && voided && st != "voided" {
		t.Fatalf("void-first order ended in %s", st)
	}

	// take vs take: one winner, the other sees INVALID_STATE
	_, s3 := assignPost(t, pool, owner, code, "race tt", a, "i3")
	wg.Add(2)
	var w1, w2 bool
	go func() { defer wg.Done(); _, err := AssignTake(ctx, pool, a, s3, "", "", "tk-a"); w1 = err == nil }()
	go func() { defer wg.Done(); _, err := AssignTake(ctx, pool, a, s3, "", "", "tk-b"); w2 = err == nil }()
	wg.Wait()
	if w1 == w2 {
		t.Fatalf("double take both resolved ok: %v %v", w1, w2)
	}
	if got := assignState(t, pool, s3); got != "taken" {
		t.Fatalf("race state = %s", got)
	}
}

// TestAssignOutputValidation: payload size cap, foreign memory
// rejection, revision pinned at submit.
func TestAssignOutputValidation(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "out", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, s1 := assignPost(t, pool, owner, code, "big payload", a, "o1")
	if _, err := AssignTake(ctx, pool, a, s1, "", "", "t1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	big := `{"blob":"` + strings.Repeat("x", 257*1024) + `"}`
	if _, err := AssignSubmit(ctx, pool, a, s1, big, "", "s1"); err == nil {
		t.Fatal("oversized payload must be rejected")
	} else {
		threadErrIs(t, err, 413, "CONTENT_TOO_LARGE")
	}

	// foreign memory in outputs rejected; own memory pinned at revision
	foreign, err := Push(ctx, pool, owner, map[string]interface{}{
		"title": "mine", "tags": []interface{}{"t"}, "content": strings.Repeat("m", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := AssignSubmit(ctx, pool, a, s1, "{}",
		fmt.Sprintf(`[{"name":"r","code":%q}]`, foreign.Code), "s2"); err == nil {
		t.Fatal("foreign memory output must be rejected")
	} else {
		threadErrIs(t, err, 422, "INVALID_TARGET")
	}
	own, err := Push(ctx, pool, a, map[string]interface{}{
		"title": "own", "tags": []interface{}{"t"}, "content": strings.Repeat("o", 60),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push own: %v", err)
	}
	if _, err := AssignSubmit(ctx, pool, a, s1, "{}",
		fmt.Sprintf(`[{"name":"r","code":%q}]`, own.Code), "s3"); err != nil {
		t.Fatalf("submit with own memory: %v", err)
	}
	var pinned string
	_ = pool.QueryRow(ctx,
		`SELECT memories_json::text FROM assign_deliveries WHERE assign_id = $1`, s1).Scan(&pinned)
	if !strings.Contains(pinned, `"revision": 1`) && !strings.Contains(pinned, `"revision":1`) {
		t.Fatalf("memory pin missing revision: %s", pinned)
	}
}

// TestDeactivationCollectsReceipts (residue audit #2): disabling an
// account collects their pending receipts — no ghost obligations
// survive to resurrect on reactivation.
func TestDeactivationCollectsReceipts(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "deact", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	e := statePost(t, pool, owner, code, "a owes this", "", nil, []int64{a}, "dc1")
	// disable via the admin path (the cascade entry point)
	if _, err := pool.Exec(ctx, `UPDATE tb_bots SET status='disabled' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	// the cascade is invoked by AdminSetBotStatus; call the repository
	// directly to assert the collection itself
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TerminateAccountThreadMemberships(ctx, tx, a); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("cascade: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	st, res, _ := stateReceipt(t, pool, e["entry"].(int64), a)
	if st != "withdrawn" || res == nil || *res != "remove" {
		t.Fatalf("ghost receipt survived deactivation: %s %v", st, res)
	}
}

// TestExpandedEntryServesPinnedRevision (residue audit #3): after the
// author updates the memory, an entry pinned at the old revision
// serves the OLD content under the OLD revision label (§5, L1).
func TestExpandedEntryServesPinnedRevision(t *testing.T) {
	pool := revisionTestPool(t)
	owner, _, _ := a7TestBot(t, pool, 5)
	a, _, _ := a7TestBot(t, pool, 5)
	ctx := context.Background()
	code, raw := threadStart(t, pool, owner, "pinned", true, "")
	defer threadCleanup(t, pool, []int64{owner, a}, code)
	if _, err := ThreadJoin(ctx, pool, a, raw, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	mem, err := Push(ctx, pool, owner, map[string]interface{}{
		"title": "doc", "tags": []interface{}{"t"}, "content": strings.Repeat("v1-", 30),
	}, 128, 10, 24, 500, 102400)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	r := statePostMem(t, pool, owner, code, mem.Code, "pinned v1", nil, nil, "pv1")
	if r["revision"].(int64) != 1 {
		t.Fatalf("pin revision = %v", r["revision"])
	}
	// author updates the memory twice — the entry must still serve v1
	for _, v := range []string{strings.Repeat("v2-", 30), strings.Repeat("v3-", 30)} {
		if _, err := Push(ctx, pool, owner, map[string]interface{}{
			"title": "doc", "tags": []interface{}{"t"}, "content": v, "code": mem.Code,
		}, 128, 10, 24, 500, 102400); err != nil {
			t.Fatalf("push update: %v", err)
		}
	}
	view, err := ThreadGet(ctx, pool, a, code, "", []int64{r["entry"].(int64)}, "", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	row := view["entries"].([]any)[0].(map[string]any)
	if row["revision"].(int64) != 1 || !strings.Contains(row["content"].(string), "v1-") {
		t.Fatalf("pinned entry drifted: rev=%v content=%v", row["revision"], row["content"])
	}
}
