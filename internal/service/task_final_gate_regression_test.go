package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/repository"
)

// PM final-gate regressions (BASE 2460f49):
//  1. terminal rejected same-key/same-payload retry replays the SAME 424
//     rejected business fact — no re-POST, no new reservation, no settlement;
//  2. post-settlement auto-close judges the AVAILABLE budget
//     (budget - reserved_budget), so an active reservation plus a settlement
//     that leaves available below the floor closes the task;
//  3. homepage fundability judges the AVAILABLE budget — a task whose
//     available budget cannot fund one submission is not listed (its
//     displayed budget is unchanged).

// -- R1: rejected duplicate replay --
func TestRejectedDuplicateReplayKeepsBusinessFact(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, setOK, hits := newGateServer()
	t.Cleanup(srv.Close)
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 3000)

	// First submit: PostAPI returns 500 -> definitive non-2xx -> rejected.
	// The durable business fact surfaces as a 424 AppError carrying the
	// submission identity and state; the row is durably rejected.
	setOK(false)
	res1, err := Submit(context.Background(), pool, code, agent, "rg1-key", map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("definitive rejection must surface as a 424 error")
	}
	ae, ok := errors.IsAppError(err)
	if !ok || ae.HTTPCode != 424 {
		t.Fatalf("want 424 AppError, got %v", err)
	}
	details := ae.Details
	if details["state"] != repository.SubStateRejected {
		t.Fatalf("424 details state = %v, want rejected", details["state"])
	}
	if details["submission_id"] == "" {
		t.Fatalf("424 details missing submission_id: %v", details)
	}
	var st string
	var rc *int
	pool.QueryRow(context.Background(),
		`SELECT state, response_code FROM tb_task_submissions WHERE client_request_key='rg1-key'`).Scan(&st, &rc)
	if st != repository.SubStateRejected {
		t.Fatalf("durable state = %s, want rejected", st)
	}
	if rc == nil || *rc != 500 {
		t.Fatalf("durable response_code = %v, want 500", rc)
	}
	hitsAfterFirst := atomic.LoadInt32(hits)
	if hitsAfterFirst != 1 {
		t.Fatalf("POST hits = %d, want 1", hitsAfterFirst)
	}
	_ = res1

	// Same key, same payload: replay of the SAME rejected fact — the
	// 424 error with identical code and submission identity, NOT a new
	// generic error and NOT a success/202 result.
	_, err = Submit(context.Background(), pool, code, agent, "rg1-key", map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("rejected replay must re-surface the 424 fact, not a success result")
	}
	ae2, ok2 := errors.IsAppError(err)
	if !ok2 || ae2.HTTPCode != 424 {
		t.Fatalf("replay want 424 AppError, got %v", err)
	}
	if ae2.Code != ae.Code {
		t.Fatalf("replay code = %s, want original %s (durable fact, not new generic)", ae2.Code, ae.Code)
	}
	d2 := ae2.Details
	if d2["submission_id"] != details["submission_id"] {
		t.Fatalf("replay submission_id = %v, want original %v", d2["submission_id"], details["submission_id"])
	}
	if d2["state"] != repository.SubStateRejected {
		t.Fatalf("replay state = %v, want rejected", d2["state"])
	}
	if atomic.LoadInt32(hits) != hitsAfterFirst {
		t.Fatalf("replay re-POSTed: hits = %d, want %d", atomic.LoadInt32(hits), hitsAfterFirst)
	}

	// Reservation released, single row, no settlement side effects.
	var cnt int
	var reserved int64
	var budget int64
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*), (SELECT reserved_budget FROM tb_tasks WHERE code=$1), (SELECT budget FROM tb_tasks WHERE code=$1) FROM tb_task_submissions WHERE task_code=$1`,
		code).Scan(&cnt, &reserved, &budget)
	if cnt != 1 {
		t.Fatalf("submission rows = %d, want 1", cnt)
	}
	if reserved != 0 {
		t.Fatalf("reserved = %d, want 0", reserved)
	}
	if budget != 3000 {
		t.Fatalf("budget = %d, want 3000", budget)
	}
	if n, _ := tcEarnCount(t, pool, agent); n != 0 {
		t.Fatalf("rejected replay settled a ledger row: n=%d", n)
	}
}

// -- R2: active reservation + settlement below floor -> auto-close --
func TestSettleAutoCloseOnAvailableBudget(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	agent := tcSeedBot(t, pool, 0)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)
	// price 5, budget 1010: one ACTIVE reservation (5) held by agent B
	// without delivering (crash-simulated); available before settle = 1005.
	code := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1010)

	holder := tcSeedBot(t, pool, 0)
	if _, _, err := acceptSubmission(context.Background(), pool, code, holder,
		repository.SubKindAgent, "rg2-hold", hashPayload([]byte(`{"task_code":"`+code+`"}`)), []byte(`{"task_code":"`+code+`"}`)); err != nil {
		t.Fatalf("hold: %v", err)
	}

	// Agent A settles a full submission: budget 1010-5=1005,
	// reserved 5, available = 1000 >= 1000 -> stays open.
	if _, err := Submit(context.Background(), pool, code, agent, "rg2-a", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var status string
	pool.QueryRow(context.Background(), `SELECT status FROM tb_tasks WHERE code=$1`, code).Scan(&status)
	if status != "open" {
		t.Fatalf("status = %s, want open (available 1000)", status)
	}

	// Another settlement: budget 1000, reserved 5, available = 995 < 1000
	// -> MUST auto-close even though raw budget (1000) still meets the
	// legacy floor.
	agent2 := tcSeedBot(t, pool, 0)
	if _, err := Submit(context.Background(), pool, code, agent2, "rg2-b", map[string]interface{}{"a": 1}); err != nil {
		t.Fatalf("submit2: %v", err)
	}
	pool.QueryRow(context.Background(), `SELECT status FROM tb_tasks WHERE code=$1`, code).Scan(&status)
	if status != "closed" {
		t.Fatalf("status = %s, want closed (available 995 < floor; active reservation must count)", status)
	}
}

// -- R3: homepage excludes tasks whose AVAILABLE budget cannot fund a submission --
func TestHomepageFundabilityOnAvailableBudget(t *testing.T) {
	pool := tcTestPool(t)
	owner := tcSeedBot(t, pool, 5000)
	srv, _, _ := newGateServer()
	t.Cleanup(srv.Close)

	// Fully-available task: listed.
	codeListed := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 2000)

	// Task whose AVAILABLE budget cannot fund one more submission:
	// budget 1010, two live crash-simulated reservations (5+5) leave
	// available 1000 (= floor, still fundable); a third reserved row
	// (accepted in an earlier state of the world — e.g. before the task
	// budget was reduced) leaves available 995 < floor. The admission
	// authority itself refuses a NEW accept at that point, which is
	// exactly the state homepage fundability must mirror.
	codeHidden := tcSeedTask(t, pool, owner, "open", srv.URL, 5, 1010)
	for i := 0; i < 2; i++ {
		holder := tcSeedBot(t, pool, 0)
		if _, _, err := acceptSubmission(context.Background(), pool, codeHidden, holder,
			repository.SubKindAgent, fmt.Sprintf("rg3-hold-%d", i), hashPayload([]byte(`{"task_code":"`+codeHidden+`"}`)), []byte(`{"task_code":"`+codeHidden+`"}`)); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
	}
	// Third reservation constructed directly (a durable reserved row the
	// current admission would refuse — legacy/reduced-budget shape):
	// reserved state + reserved_budget bumped to 15.
	third := tcSeedBot(t, pool, 0)
	if _, _, err := acceptSubmission(context.Background(), pool, codeHidden, third,
		repository.SubKindAgent, "rg3-hold-legacy", hashPayload([]byte(`{"task_code":"`+codeHidden+`"}`)), []byte(`{"task_code":"`+codeHidden+`"}`)); err != nil {
		t.Fatalf("hold-legacy (admission still open at available 1000): %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tb_tasks SET reserved_budget = 15 WHERE code=$1`, codeHidden); err != nil {
		t.Fatal(err)
	}
	// Sanity: admission now refuses (available 995 < floor).
	if _, _, err := acceptSubmission(context.Background(), pool, codeHidden, tcSeedBot(t, pool, 0),
		repository.SubKindAgent, "rg3-refused", hashPayload([]byte(`{"task_code":"`+codeHidden+`"}`)), []byte(`{"task_code":"`+codeHidden+`"}`)); err == nil {
		t.Fatal("admission must refuse at available 995 — test premise broken")
	}

	rows, err := repository.QueryHomepageTasks(context.Background(), pool, MinOpenBudget)
	if err != nil {
		t.Fatalf("homepage: %v", err)
	}
	var foundListed, foundHidden bool
	for _, r := range rows {
		if r.Code == codeListed {
			foundListed = true
		}
		if r.Code == codeHidden {
			foundHidden = true
		}
	}
	if !foundListed {
		t.Fatal("fully-available task missing from homepage")
	}
	if foundHidden {
		t.Fatal("task with exhausted available budget listed on homepage (reserved_budget ignored)")
	}

	// Displayed budget semantics unchanged: the hidden task's row itself
	// still reports its raw budget.
	var disp int64
	pool.QueryRow(context.Background(), `SELECT budget FROM tb_tasks WHERE code=$1`, codeHidden).Scan(&disp)
	if disp != 1010 {
		t.Fatalf("displayed budget = %d, want 1010 (raw budget, unchanged by fundability filter)", disp)
	}
}
